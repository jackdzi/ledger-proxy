package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SyncResult summarizes one bank sync.
type SyncResult struct {
	New, Updated, Unchanged int
	Unmapped                []string // bank accounts not in bank_accounts.txt: "id (name)"
	Messages                []string // errors and warnings from the bank side
}

// LastBankFetch is when the last sync saved a response (zero if never).
// Kept in the database so restarts don't cost extra requests against
// SimpleFIN's ~24-per-day quota.
func LastBankFetch(ctx context.Context, db DB) (time.Time, error) {
	var t *time.Time
	err := db.QueryRow(ctx, `SELECT max(fetched_at) FROM raw_feed`).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

// SyncBank fetches the last `days` days from SimpleFIN and mirrors every
// transaction of a mapped account into bank_transactions (inserting new
// ones, updating ones that changed, e.g. pending → posted). The raw response
// and all rows are saved in one database transaction: all or nothing.
// accounts maps SimpleFIN account ids to ledger accounts.
// Regular syncs use 30 days, which overlaps previous syncs generously so
// late-posting transactions and changed dates are always picked up. A
// one-time backfill can go up to 89 (SimpleFIN allows 90 per request).
func SyncBank(ctx context.Context, pool *pgxpool.Pool, sf *SimpleFIN, accounts map[string]string, loc *time.Location, days int) (SyncResult, error) {
	var res SyncResult
	if days < 1 || days > 89 {
		return res, fmt.Errorf("days must be 1-89, got %d", days)
	}
	now := time.Now()
	raw, set, err := sf.Fetch(ctx, now.AddDate(0, 0, -days), now)
	if err != nil {
		return res, err
	}
	res.Messages = set.Messages()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `INSERT INTO raw_feed (body) VALUES ($1)`, raw); err != nil {
		return res, fmt.Errorf("save raw response: %w", err)
	}

	for _, acct := range set.Accounts {
		ledgerAccount, ok := accounts[acct.ID]
		if !ok {
			res.Unmapped = append(res.Unmapped, fmt.Sprintf("%s (%s)", acct.ID, acct.Name))
			continue
		}
		if acct.Currency != "USD" {
			res.Messages = append(res.Messages, fmt.Sprintf("skipped %s: currency %s isn't supported", acct.Name, acct.Currency))
			continue
		}
		for _, t := range acct.Transactions {
			amount, err := ParseSignedDollars(t.Amount)
			if err != nil {
				return res, fmt.Errorf("account %s, transaction %s: %w", acct.ID, t.ID, err)
			}
			// The date it happened if the bank says; else when it posted; else today.
			when := now
			if t.TransactedAt > 0 {
				when = time.Unix(t.TransactedAt, 0)
			} else if t.Posted > 0 {
				when = time.Unix(t.Posted, 0)
			}
			status := "posted"
			if t.Pending || t.Posted == 0 {
				status = "pending"
			}

			// Insert, or update only if something changed. xmax = 0 means
			// the row was inserted rather than updated.
			var inserted bool
			err = tx.QueryRow(ctx,
				`INSERT INTO bank_transactions
				   (bank_account_id, bank_txn_id, account, date, amount_cents, description, status)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)
				 ON CONFLICT (bank_account_id, bank_txn_id) DO UPDATE SET
				   account = EXCLUDED.account, date = EXCLUDED.date,
				   amount_cents = EXCLUDED.amount_cents, description = EXCLUDED.description,
				   status = EXCLUDED.status, updated_at = now()
				 WHERE (bank_transactions.account, bank_transactions.date, bank_transactions.amount_cents,
				        bank_transactions.description, bank_transactions.status)
				   IS DISTINCT FROM
				       (EXCLUDED.account, EXCLUDED.date, EXCLUDED.amount_cents,
				        EXCLUDED.description, EXCLUDED.status)
				 RETURNING (xmax = 0)`,
				acct.ID, t.ID, ledgerAccount, when.In(loc).Format("2006-01-02"),
				amount, strings.TrimSpace(t.Description), status).Scan(&inserted)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				res.Unchanged++
			case err != nil:
				return res, fmt.Errorf("save transaction: %w", err)
			case inserted:
				res.New++
			default:
				res.Updated++
			}
		}
	}
	return res, tx.Commit(ctx)
}

// ParseSignedDollars parses bank amounts like "-4.50", "86.46" or "-12.3000"
// into cents. Extra decimal places are allowed only if they're zeros.
func ParseSignedDollars(s string) (Cents, error) {
	s = strings.TrimSpace(s)
	if whole, frac, ok := strings.Cut(s, "."); ok && len(frac) > 2 {
		if strings.Trim(frac[2:], "0") != "" {
			return 0, fmt.Errorf("ledger: amount %q has fractions of a cent", s)
		}
		s = whole + "." + frac[:2]
	}
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		c, err := ParseDollars(rest)
		return -c, err
	}
	return ParseDollars(s)
}
