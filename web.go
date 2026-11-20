package main

import (
	"crypto/subtle"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackdzi/ledger-proxy/ledger"
)

//go:embed report.html
var reportHTML string

//go:embed transactions.html
var transactionsHTML string

// The page templates are compiled into the binary. html/template escapes
// every value, so a merchant named "<script>…" is shown as text, never run.
var pageFuncs = template.FuncMap{
	"money": func(c ledger.Cents) string { return c.String() },
	"day":   func(t time.Time) string { return t.Format("Jan 2") },
	"label": func(category string) string {
		switch category {
		case "":
			return "Not yet sorted"
		case "going_out":
			return "Going out"
		}
		return strings.ToUpper(category[:1]) + category[1:]
	},
	// percent: a category's share of the total, for the bar width.
	"percent": func(part, total ledger.Cents) int {
		if total <= 0 || part <= 0 {
			return 0
		}
		return int(part * 100 / total)
	},
	// short: "Assets:ChaseChecking" → "ChaseChecking"
	"short": func(account string) string {
		if _, rest, ok := strings.Cut(account, ":"); ok {
			return rest
		}
		return account
	},
}

var (
	reportPage       = template.Must(template.New("report").Funcs(pageFuncs).Parse(reportHTML))
	transactionsPage = template.Must(template.New("transactions").Funcs(pageFuncs).Parse(transactionsHTML))
)

// GET /transactions?month=2026-11: every transaction in a month (default: this one).
func (s *server) transactionsPageHandler(w http.ResponseWriter, r *http.Request) {
	from := time.Now().In(s.loc)
	from = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, s.loc)
	if m := r.URL.Query().Get("month"); m != "" {
		parsed, err := time.ParseInLocation("2006-01", m, s.loc)
		if err != nil {
			http.Error(w, "month must look like 2026-11", http.StatusBadRequest)
			return
		}
		from = parsed
	}
	to := from.AddDate(0, 1, 0)

	rows, err := ledger.ListTransactions(r.Context(), s.pool, from, to)
	if err != nil {
		slog.Error("list transactions failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	matched, pending := 0, 0
	for i := range rows {
		rows[i].OccurredAt = rows[i].OccurredAt.In(s.loc)
		if rows[i].Matched {
			matched++
		}
		if rows[i].Status == "pending" {
			pending++
		}
	}
	link := func(t time.Time) string { return "/transactions?month=" + t.Format("2006-01") }

	privatePageHeaders(w)
	err = transactionsPage.Execute(w, map[string]any{
		"Title":   from.Format("January 2006"),
		"Rows":    rows,
		"Matched": matched,
		"Pending": pending,
		"PrevURL": link(from.AddDate(0, -1, 0)),
		"NextURL": link(to),
	})
	if err != nil {
		slog.Error("render transactions", "err", err)
	}
}

// GET /?view=month|week&date=2026-11-16 (any date inside the period; default today)
func (s *server) reportPageHandler(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if view != "week" {
		view = "month"
	}
	day := time.Now().In(s.loc)
	if d := r.URL.Query().Get("date"); d != "" {
		parsed, err := time.ParseInLocation("2006-01-02", d, s.loc)
		if err != nil {
			http.Error(w, "date must look like 2026-11-16", http.StatusBadRequest)
			return
		}
		day = parsed
	}
	from, to := period(view, day)

	rep, err := ledger.SpendingReport(r.Context(), s.pool, from, to)
	if err != nil {
		slog.Error("report failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for i := range rep.Purchases {
		rep.Purchases[i].OccurredAt = rep.Purchases[i].OccurredAt.In(s.loc) // show local dates
	}

	title := from.Format("January 2006")
	if view == "week" {
		title = from.Format("Jan 2") + " – " + to.AddDate(0, 0, -1).Format("Jan 2, 2006")
	}
	link := func(v string, d time.Time) string { return "/?view=" + v + "&date=" + d.Format("2006-01-02") }

	privatePageHeaders(w)
	err = reportPage.Execute(w, map[string]any{
		"View":     view,
		"Title":    title,
		"Report":   rep,
		"MonthURL": link("month", day),
		"WeekURL":  link("week", day),
		"PrevURL":  link(view, from.AddDate(0, 0, -1)), // a day inside the previous period
		"NextURL":  link(view, to),                     // the first day of the next period
	})
	if err != nil {
		slog.Error("render report", "err", err)
	}
}

// period returns [from, to) for the month or the Monday-to-Sunday week
// containing day, in day's time zone.
func period(view string, day time.Time) (time.Time, time.Time) {
	y, m, d := day.Date()
	if view == "week" {
		start := time.Date(y, m, d, 0, 0, 0, 0, day.Location())
		back := (int(start.Weekday()) + 6) % 7 // days since Monday
		from := start.AddDate(0, 0, -back)
		return from, from.AddDate(0, 0, 7)
	}
	from := time.Date(y, m, 1, 0, 0, 0, 0, day.Location())
	return from, from.AddDate(0, 1, 0)
}

// requirePassword asks the browser for a password (HTTP Basic Auth; Safari
// and iCloud Keychain can remember it). The username is ignored. Only safe
// because every request is over TLS.
func requirePassword(password string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, got, ok := r.BasicAuth()
		// Ignore surrounding whitespace: passwords copied from a QR code or a
		// terminal often bring a trailing line break. Ours never contain any.
		got = strings.TrimSpace(got)
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(password)) != 1 {
			// Log failures without the password: whether one was sent, and its
			// length (a copy with a trailing space or line break is 1–2 longer).
			if ok {
				slog.Warn("login failed: wrong password", "length", len(got), "want_length", len(password),
					"has_whitespace", strings.ContainsAny(got, " \t\r\n"))
			} else if r.Header.Get("Authorization") != "" {
				slog.Warn("login failed: unreadable credentials")
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="ledger", charset="UTF-8"`)
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// privatePageHeaders: don't cache financial data, don't allow scripts or
// embedding, don't leak the URL to other sites.
func privatePageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}
