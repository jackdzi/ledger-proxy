package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Payments to and from people (checked first: "Zelle payment to …" is not a
// transfer between your own accounts). Money sent counts as spending, money
// received as income.
var peerWords = []string{"ZELLE", "VENMO", "CASH APP", "APPLE CASH"}

// Bank rows whose description contains one of these move money between your
// own accounts (e.g. paying the credit card from checking): not spending.
var transferWords = []string{"PAYMENT", "TRANSFER", "AUTOPAY"}

// ImportResult counts what one ImportBank run did.
type ImportResult struct {
	Matched, Adjusted                     int // bank rows that confirmed a tap; of those, with a tip/hold adjustment
	Purchases, Refunds, Transfers, Income int // bank rows imported on their own
}

func (r ImportResult) Total() int {
	return r.Matched + r.Purchases + r.Refunds + r.Transfers + r.Income
}

// ImportBank reconciles posted bank rows with the ledger. Each row either
// confirms a pending tap (the tap becomes posted and is linked to the bank
// id; if the amounts differ, an adjustment transaction records the tip or the
// settled hold), or is imported on its own (no tap fits: online purchases,
// autopay, transfers, income, taps that never arrived).
//
// The decisions come from MatchTaps, a pure function; this applies them. Each
// row is handled at most once: its "<bank account id>:<bank transaction id>"
// ends up as external_id on exactly one transaction (the tap, or the
// imported row), and UNIQUE (source, external_id) backs that up.
func ImportBank(ctx context.Context, pool *pgxpool.Pool, loc *time.Location) (ImportResult, error) {
	var res ImportResult
	rows, err := unhandledBankRows(ctx, pool)
	if err != nil || len(rows) == 0 {
		return res, err
	}
	taps, err := pendingTaps(ctx, pool, loc)
	if err != nil {
		return res, err
	}

	for _, m := range MatchTaps(rows, taps) {
		var err error
		if m.Tap != nil {
			err = applyMatch(ctx, pool, loc, m)
			if err == nil {
				res.Matched++
				if m.Adjustment != 0 {
					res.Adjusted++
				}
			}
		} else {
			var count *int
			count, err = importAlone(ctx, pool, loc, m.Row, &res)
			if err == nil && count != nil {
				*count++
			}
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // already handled (a concurrent run)
			continue
		}
		if err != nil {
			return res, fmt.Errorf("bank row %s: %w", m.Row.ExtID, err)
		}
	}
	return res, nil
}

// unhandledBankRows: posted rows whose id isn't on any transaction yet.
func unhandledBankRows(ctx context.Context, pool *pgxpool.Pool) ([]BankRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT b.bank_account_id || ':' || b.bank_txn_id, b.account, b.date, b.amount_cents, b.description
		FROM bank_transactions b
		WHERE b.status = 'posted' AND b.amount_cents <> 0
		  AND NOT EXISTS (SELECT 1 FROM transactions t
		                  WHERE t.external_id = b.bank_account_id || ':' || b.bank_txn_id)
		ORDER BY b.date, 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BankRow
	for rows.Next() {
		var r BankRow
		if err := rows.Scan(&r.ExtID, &r.Account, &r.Date, &r.Amount, &r.Desc); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// pendingTaps: taps not yet confirmed by a bank row, with their card account
// and amount.
func pendingTaps(ctx context.Context, pool *pgxpool.Pool, loc *time.Location) ([]PendingTap, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, t.description, (t.occurred_at AT TIME ZONE $1)::date,
		       max(a.name) FILTER (WHERE a.type IN ('asset', 'liability')),
		       sum(e.amount_cents) FILTER (WHERE a.type = 'expense')
		FROM transactions t
		JOIN entries e  ON e.transaction_id = t.id
		JOIN accounts a ON a.id = e.account_id
		WHERE t.source = 'tap' AND t.status = 'pending' AND t.external_id IS NULL
		GROUP BY t.id`, loc.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingTap
	for rows.Next() {
		var t PendingTap
		var account *string
		var amount *int64
		if err := rows.Scan(&t.ID, &t.Merchant, &t.Date, &account, &amount); err != nil {
			return nil, err
		}
		if account == nil || amount == nil {
			continue
		}
		t.Account, t.Amount = *account, Cents(*amount)
		out = append(out, t)
	}
	return out, rows.Err()
}

// applyMatch confirms a tap: it becomes posted and carries the bank id. If
// the bank charged a different amount, an adjustment transaction records the
// difference (the tap itself is never edited).
func applyMatch(ctx context.Context, pool *pgxpool.Pool, loc *time.Location, m Match) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx,
		`UPDATE transactions SET status = 'posted', external_id = $2
		 WHERE id = $1 AND status = 'pending' AND external_id IS NULL`, m.Tap.ID, m.Row.ExtID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return nil // the tap changed meanwhile; the row is retried next run
	}
	if m.Adjustment != 0 {
		var category string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(category, '') FROM transactions WHERE id = $1`, m.Tap.ID).Scan(&category); err != nil {
			return err
		}
		accountID, err := Account(ctx, tx, m.Row.Account, accountTypeOf(m.Row.Account))
		if err != nil {
			return err
		}
		spendingID, err := Account(ctx, tx, "Expenses:Spending", "expense")
		if err != nil {
			return err
		}
		y, mo, d := m.Row.Date.Date()
		if _, err := Post(ctx, tx, Transaction{
			OccurredAt:  time.Date(y, mo, d, 12, 0, 0, 0, loc),
			Description: fmt.Sprintf("%s adjustment (tap #%d): %s", m.Kind, m.Tap.ID, m.Row.Desc),
			Status:      "posted",
			Source:      "bank",
			ExternalID:  m.Row.ExtID + ":adjustment",
			Category:    category,
			Entries: []Entry{
				{AccountID: spendingID, Amount: m.Adjustment}, // a tip adds spending; a hold that settled lower removes some
				{AccountID: accountID, Amount: -m.Adjustment},
			},
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// importAlone records a bank row no tap accounts for, and returns which
// counter to increase.
func importAlone(ctx context.Context, pool *pgxpool.Pool, loc *time.Location, r BankRow, res *ImportResult) (*int, error) {
	accountType := accountTypeOf(r.Account)

	// What is the other side of this money movement?
	var counter, counterType string
	var count *int
	upper := strings.ToUpper(r.Desc)
	switch {
	case containsAny(upper, peerWords) && r.Amount < 0:
		counter, counterType, count = "Expenses:Spending", "expense", &res.Purchases
	case containsAny(upper, peerWords):
		counter, counterType, count = "Income:Other", "income", &res.Income
	case containsAny(upper, transferWords):
		counter, counterType, count = "Assets:Transfers", "asset", &res.Transfers
	case r.Amount < 0:
		counter, counterType, count = "Expenses:Spending", "expense", &res.Purchases
	case accountType == "liability":
		counter, counterType, count = "Expenses:Spending", "expense", &res.Refunds
	default:
		counter, counterType, count = "Income:Other", "income", &res.Income
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	accountID, err := Account(ctx, tx, r.Account, accountType)
	if err != nil {
		return nil, err
	}
	counterID, err := Account(ctx, tx, counter, counterType)
	if err != nil {
		return nil, err
	}
	y, m, d := r.Date.Date() // midday local time, so the date never shifts across a time zone
	if _, err := Post(ctx, tx, Transaction{
		OccurredAt:  time.Date(y, m, d, 12, 0, 0, 0, loc),
		Description: r.Desc,
		Status:      "posted",
		Source:      "bank",
		ExternalID:  r.ExtID,
		Entries: []Entry{
			{AccountID: accountID, Amount: r.Amount},  // the bank account, signed as the bank reports it
			{AccountID: counterID, Amount: -r.Amount}, // the other side
		},
	}); err != nil {
		return nil, err
	}
	return count, tx.Commit(ctx)
}

func accountTypeOf(account string) string {
	if strings.HasPrefix(account, "Liabilities:") {
		return "liability"
	}
	return "asset"
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
