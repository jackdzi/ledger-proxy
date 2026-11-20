// Command ledgerd is the ledger server.
package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/time/rate"

	"github.com/jackdzi/ledger-proxy/ledger"
)

type server struct {
	pool *pgxpool.Pool
	// card name as iOS reports it → ledger account
	cards map[string]string
	// wakes the categorizer when a tap has no matching rule
	nudge chan struct{}
	loc *time.Location
}

func main() {
	if err := run(); err != nil {
		slog.Error("ledgerd stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	backfill := flag.Int("backfill-days", 0, "fetch this many days (max 89) from SimpleFIN once, then exit")
	flag.Parse()

	dbURL, tapToken := os.Getenv("DATABASE_URL"), os.Getenv("TAP_TOKEN")
	if dbURL == "" || tapToken == "" {
		return errors.New("DATABASE_URL and TAP_TOKEN must be set")
	}
	cardsFile := os.Getenv("CARDS_FILE")
	if cardsFile == "" {
		cardsFile = "cards.txt"
	}
	cards, err := loadCards(cardsFile)
	if err != nil {
		return err
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword != "" && len(adminPassword) < 16 {
		return errors.New("ADMIN_PASSWORD must be at least 16 characters")
	}
	loc, err := time.LoadLocation(envOr("TIMEZONE", "America/Chicago"))
	if err != nil {
		return fmt.Errorf("TIMEZONE: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("can't reach Postgres (did you run make db?): %w", err)
	}

	if *backfill > 0 {
		return backfillBank(ctx, pool, loc, *backfill)
	}

	s := &server{pool: pool, cards: cards, nudge: make(chan struct{}, 1), loc: loc}

	// Categorizer - classifies new merchants
	classify := ledger.Ollama(envOr("OLLAMA_URL", "http://127.0.0.1:11434"), envOr("OLLAMA_MODEL", "gemma3:4b"))
	go categorize(ctx, pool, classify, s.nudge)

	// Bank feed
	if accessURL, err := os.ReadFile(envOr("SIMPLEFIN_ACCESS_URL_FILE", "simplefin_access_url")); err == nil {
		sf, err := ledger.NewSimpleFIN(string(accessURL))
		if err != nil {
			return err
		}
		bankAccounts, err := loadAccountMap(envOr("BANK_ACCOUNTS_FILE", "bank_accounts.txt"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		go func() {
			importBank(ctx, pool, loc, s.nudge)
			syncBank(ctx, pool, sf, bankAccounts, loc, s.nudge)
		}()
	} else {
		slog.Info("bank sync off: run cmd/simplefin-setup to link a bank")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	// At most 30 taps per minute in total
	limiter := rate.NewLimiter(rate.Every(2*time.Second), 10)
	mux.HandleFunc("POST /v1/taps", rateLimit(limiter, requireToken(tapToken, s.postTap)))

	// report login page, 1 request/second
	if adminPassword != "" {
		pageLimiter := rate.NewLimiter(rate.Every(time.Second), 20)
		mux.HandleFunc("GET /{$}", rateLimit(pageLimiter, requirePassword(adminPassword, s.reportPageHandler)))
		mux.HandleFunc("GET /transactions", rateLimit(pageLimiter, requirePassword(adminPassword, s.transactionsPageHandler)))
	} else {
		slog.Warn("ADMIN_PASSWORD not set: report page is off")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // drops clients that send headers very slowly
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// On shutdown: stop accepting connections, let in-flight requests finish.
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		close(done)
	}()

	// TLS: with TLS_HOSTNAME set, ledgerd gets its own Let's Encrypt
	// certificate. The challenge (TLS-ALPN-01) arrives on this same
	// connection through edge's passthrough, so no port 80 is needed.
	if host := os.Getenv("TLS_HOSTNAME"); host != "" {
		certDir := os.Getenv("CERT_DIR")
		if certDir == "" {
			certDir = "certs" // holds the certificate AND the ACME account key: keep it private
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(host), // never request certs for other names
			Cache:      autocert.DirCache(certDir),
		}
		srv.TLSConfig = m.TLSConfig()
		srv.TLSConfig.MinVersion = tls.VersionTLS13
		slog.Info("ledgerd listening (TLS)", "addr", addr, "host", host)
		err = srv.ListenAndServeTLS("", "") // certificates come from autocert
	} else {
		slog.Info("ledgerd listening (plain HTTP, local only)", "addr", addr)
		err = srv.ListenAndServe()
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

// categorize runs every minute, and immediately when a tap arrives with no
// matching rule. Taps never wait for it: if the model is slow or down,
// transactions just stay uncategorized until the next run.
func categorize(ctx context.Context, pool *pgxpool.Pool, classify ledger.Classifier, nudge <-chan struct{}) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		n, err := ledger.CategorizePending(ctx, pool, classify)
		if n > 0 {
			slog.Info("categorized", "count", n)
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("categorizer will retry", "err", err) // ids only, never merchant names
		}
		select {
		case <-ctx.Done():
			return
		case <-nudge:
		case <-tick.C:
		}
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// rateLimit answers 429 when the limiter is out of tokens. It runs before
// the token check, so floods with wrong tokens are capped too.
func rateLimit(l *rate.Limiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow() {
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
			return
		}
		next(w, r)
	}
}

// POST /v1/taps: {"card": "Apple Card", "merchant": "Blue Bottle",
// "amount": "$4.50", "occurred_at": "2026-11-21T09:14:03-06:00"}
// becomes a pending transaction: Expenses:Uncategorized +450, card −450.
func (s *server) postTap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Card       string    `json:"card"`
		Merchant   string    `json:"merchant"`
		Amount     string    `json:"amount"`
		OccurredAt time.Time `json:"occurred_at"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)) // a tap is ~150 bytes
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	account, ok := s.cards[req.Card]
	merchant := strings.TrimSpace(req.Merchant)
	amount, amountErr := ledger.ParseDollars(req.Amount)
	age := time.Since(req.OccurredAt)
	var problem string
	switch {
	case !ok:
		problem = fmt.Sprintf("unknown card %q (add it to the cards file)", req.Card)
	case merchant == "" || len(merchant) > 200:
		problem = "merchant must be 1-200 characters"
	case amountErr != nil || amount == 0:
		problem = `amount must look like "$4.50"`
	case age > 5*time.Minute || age < -5*time.Minute:
		problem = "occurred_at must be within 5 minutes of now"
	}
	if problem != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": problem})
		return
	}

	// The key is built from the parsed tap, so "$4.50" and "4.5" count as the
	// same purchase. It's also the request fingerprint: same key = same tap.
	key := fmt.Sprintf("tap|%s|%s|%d|%s", req.Card, merchant, amount, req.OccurredAt.UTC().Format(time.RFC3339))

	var category string
	resp, err := ledger.Do(r.Context(), s.pool, key, []byte(key), func(tx pgx.Tx) (ledger.Response, error) {
		var err error
		category, _, err = ledger.RuleFor(r.Context(), tx, merchant)
		if err != nil {
			return ledger.Response{}, err
		}
		cardID, err := ledger.Account(r.Context(), tx, account, accountType(account))
		if err != nil {
			return ledger.Response{}, err
		}
		expenseID, err := ledger.Account(r.Context(), tx, "Expenses:Spending", "expense")
		if err != nil {
			return ledger.Response{}, err
		}
		id, err := ledger.Post(r.Context(), tx, ledger.Transaction{
			OccurredAt:  req.OccurredAt,
			Description: merchant,
			Status:      "pending",
			Source:      "tap",
			Category:    category,
			Entries: []ledger.Entry{
				{AccountID: expenseID, Amount: amount},
				{AccountID: cardID, Amount: -amount},
			},
		})
		if err != nil {
			return ledger.Response{}, err
		}
		reply := map[string]any{"transaction_id": id, "status": "pending"}
		if category != "" {
			reply["category"] = category
		}
		body, err := json.Marshal(reply)
		return ledger.Response{Status: http.StatusCreated, Body: body}, err
	})
	if err != nil {
		slog.Error("tap failed", "err", err) // never logs the merchant or amount
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if category == "" {
		select {
		case s.nudge <- struct{}{}: // wake the categorizer
		default: // it's already awake
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	w.Write(resp.Body)
}

// requireToken only lets requests through with "Authorization: Bearer <want>".
// ConstantTimeCompare takes the same time whether the first or last character
// is wrong, so response timing can't be used to guess the token.
func requireToken(want string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got = strings.TrimSpace(got) // a token pasted into the Shortcut may carry a line break
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong token"})
			return
		}
		next(w, r)
	}
}

// loadCards reads which ledger account each card uses, one per line:
//
//	Apple Card  = Liabilities:AppleCard
//	Chase Debit = Assets:Checking
//
// Blank lines and lines starting with # are ignored.
func loadCards(path string) (map[string]string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("read cards file (copy cards.example.txt to %s): %w", path, err)
	}
	cards, err := loadAccountMap(path)
	if err == nil && len(cards) == 0 {
		return nil, fmt.Errorf("%s: no cards listed", path)
	}
	return cards, err
}

// loadAccountMap reads "name or id = Liabilities:X / Assets:X" lines
// (cards.txt, bank_accounts.txt). Everything after a # is a comment.
func loadAccountMap(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#") // strip comments
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, account, ok := strings.Cut(line, "=")
		name, account = strings.TrimSpace(name), strings.TrimSpace(account)
		if !ok || name == "" || accountType(account) == "" {
			return nil, fmt.Errorf("%s line %d: want \"Name = Liabilities:X\" or \"Name = Assets:X\"", path, i+1)
		}
		if _, dup := m[name]; dup {
			return nil, fmt.Errorf("%s line %d: %q listed twice", path, i+1, name)
		}
		m[name] = account
	}
	return m, nil
}

// backfillBank runs one sync over a longer window (e.g. 89 days of history)
// and exits. It uses one request of SimpleFIN's daily quota.
func backfillBank(ctx context.Context, pool *pgxpool.Pool, loc *time.Location, days int) error {
	accessURL, err := os.ReadFile(envOr("SIMPLEFIN_ACCESS_URL_FILE", "simplefin_access_url"))
	if err != nil {
		return fmt.Errorf("no SimpleFIN access URL (run cmd/simplefin-setup first): %w", err)
	}
	sf, err := ledger.NewSimpleFIN(string(accessURL))
	if err != nil {
		return err
	}
	accounts, err := loadAccountMap(envOr("BANK_ACCOUNTS_FILE", "bank_accounts.txt"))
	if err != nil {
		return fmt.Errorf("bank_accounts.txt: %w", err)
	}
	res, err := ledger.SyncBank(ctx, pool, sf, accounts, loc, days)
	for _, m := range res.Messages {
		fmt.Println("bank says:", m)
	}
	for _, u := range res.Unmapped {
		fmt.Println("not in bank_accounts.txt (ignored):", u)
	}
	if err != nil {
		return err
	}
	fmt.Printf("backfill of %d days: %d new, %d updated, %d unchanged\n", days, res.New, res.Updated, res.Unchanged)
	return nil
}

// importBank turns new bank rows into ledger transactions and wakes the
// categorizer so imported purchases get categories.
func importBank(ctx context.Context, pool *pgxpool.Pool, loc *time.Location, nudge chan<- struct{}) {
	res, err := ledger.ImportBank(ctx, pool, loc)
	if err != nil {
		slog.Error("bank import failed", "err", err)
	}
	if res.Total() > 0 {
		slog.Info("bank import", "matched_taps", res.Matched, "with_adjustment", res.Adjusted,
			"purchases", res.Purchases, "refunds", res.Refunds, "transfers", res.Transfers, "income", res.Income)
		select {
		case nudge <- struct{}{}:
		default:
		}
	}
}

// syncBank pulls from SimpleFIN every 6 hours. The schedule is based on the
// last saved fetch in the database, so restarts never cost extra requests
// (SimpleFIN allows ~24 a day and may disable access beyond that).
func syncBank(ctx context.Context, pool *pgxpool.Pool, sf *ledger.SimpleFIN, accounts map[string]string, loc *time.Location, nudge chan<- struct{}) {
	const every = 6 * time.Hour
	wait := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	for {
		last, err := ledger.LastBankFetch(ctx, pool)
		if err != nil {
			slog.Warn("bank sync: can't read last fetch time", "err", err)
			last = time.Now() // be conservative with the quota
		}
		if d := time.Until(last.Add(every)); d > 0 && !wait(d) {
			return
		}
		res, err := ledger.SyncBank(ctx, pool, sf, accounts, loc, 30)
		for _, m := range res.Messages {
			slog.Warn("bank sync: message from the bank side", "msg", m)
		}
		if len(res.Unmapped) > 0 {
			slog.Warn("bank sync: accounts not in bank_accounts.txt were ignored", "accounts", res.Unmapped)
		}
		if err != nil {
			slog.Error("bank sync failed; retrying in 2 hours", "err", err)
			if !wait(2 * time.Hour) {
				return
			}
			continue
		}
		slog.Info("bank sync", "new", res.New, "updated", res.Updated, "unchanged", res.Unchanged)
		importBank(ctx, pool, loc, nudge)
	}
}

// accountType: credit cards are liabilities (you owe), debit cards draw on assets.
func accountType(account string) string {
	switch {
	case strings.HasPrefix(account, "Liabilities:"):
		return "liability"
	case strings.HasPrefix(account, "Assets:"):
		return "asset"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
