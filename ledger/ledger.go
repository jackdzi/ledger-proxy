// Package ledger records money as balanced double-entry transactions.
// Amounts are integer cents; floats never appear.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Cents is an amount of money in cents. Positive = debit, negative = credit.
type Cents int64

// String formats cents as dollars: 450 → "$4.50", 123456 → "$1,234.56",
// -300 → "-$3.00".
func (c Cents) String() string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	dollars := fmt.Sprint(int64(c) / 100)
	for i := len(dollars) - 3; i > 0; i -= 3 {
		dollars = dollars[:i] + "," + dollars[i:]
	}
	return fmt.Sprintf("%s$%s.%02d", sign, dollars, int64(c)%100)
}

// DB is anything that can run queries: a connection pool or an open
// database transaction (pgx.Tx).
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Entry struct {
	AccountID int64
	Amount    Cents
}

type Transaction struct {
	OccurredAt  time.Time
	Description string
	Status      string // "pending", "posted" or "voided"
	Source      string // "tap", "bank" or "manual"
	Category    string // "food", "going_out", "transportation", "other"; "" = not yet
	ExternalID  string // bank transactions: "<bank account id>:<bank transaction id>"; "" otherwise
	Entries     []Entry
}

var ErrUnbalanced = errors.New("ledger: entries do not sum to zero")

// Post saves a transaction and its entries inside tx and returns the new id.
// Nothing is permanent until the caller commits tx. It checks the balance
// first for a clear error; Postgres checks it again at commit.
func Post(ctx context.Context, tx pgx.Tx, t Transaction) (int64, error) {
	if len(t.Entries) < 2 {
		return 0, errors.New("ledger: a transaction needs at least two entries")
	}
	var sum Cents
	for _, e := range t.Entries {
		if e.Amount == 0 {
			return 0, errors.New("ledger: an entry can't be zero")
		}
		sum += e.Amount
	}
	if sum != 0 {
		return 0, ErrUnbalanced
	}

	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO transactions (occurred_at, description, status, source, category, external_id)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, '')) RETURNING id`,
		t.OccurredAt, t.Description, t.Status, t.Source, t.Category, t.ExternalID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert transaction: %w", err)
	}
	for _, e := range t.Entries {
		_, err := tx.Exec(ctx,
			`INSERT INTO entries (transaction_id, account_id, amount_cents) VALUES ($1, $2, $3)`,
			id, e.AccountID, e.Amount)
		if err != nil {
			return 0, fmt.Errorf("insert entry: %w", err)
		}
	}
	return id, nil
}

// Account returns the id of the named account, creating it if needed.
func Account(ctx context.Context, db DB, name, typ string) (int64, error) {
	_, err := db.Exec(ctx,
		`INSERT INTO accounts (name, type) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`,
		name, typ)
	if err != nil {
		return 0, fmt.Errorf("create account %q: %w", name, err)
	}
	var id int64
	var existing string
	err = db.QueryRow(ctx, `SELECT id, type FROM accounts WHERE name = $1`, name).Scan(&id, &existing)
	if err != nil {
		return 0, fmt.Errorf("find account %q: %w", name, err)
	}
	if existing != typ {
		return 0, fmt.Errorf("account %q is %s, not %s", name, existing, typ)
	}
	return id, nil
}

// Balance sums an account's entries, skipping voided transactions.
// Pending taps count, so the balance includes purchases the bank hasn't posted yet.
func Balance(ctx context.Context, db DB, account string) (Cents, error) {
	var total Cents
	err := db.QueryRow(ctx,
		`SELECT COALESCE(SUM(e.amount_cents), 0)
		 FROM entries e
		 JOIN transactions t ON t.id = e.transaction_id
		 JOIN accounts a     ON a.id = e.account_id
		 WHERE a.name = $1 AND t.status <> 'voided'`,
		account).Scan(&total)
	return total, err
}

// ParseDollars turns "$4.50", "4.5", "4" or "$1,234.56" into cents.
// It rejects negatives, more than two decimals, and anything else odd.
func ParseDollars(s string) (Cents, error) {
	bad := fmt.Errorf("ledger: invalid amount %q", s)
	s = strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(s), "$"), ",", "")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(whole) > 9 || len(frac) > 2 {
		return 0, bad // more than 9 digits = over $999,999,999: surely a mistake
	}
	frac += strings.Repeat("0", 2-len(frac)) // "5" → "50"
	var cents Cents
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, bad
		}
		cents = cents*10 + Cents(r-'0')
	}
	return cents, nil
}
