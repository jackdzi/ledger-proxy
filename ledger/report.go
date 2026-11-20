package ledger

import (
	"context"
	"sort"
	"time"
)

// Purchase is one spending transaction in a report.
type Purchase struct {
	ID          int64
	OccurredAt  time.Time
	Description string
	Category    string // "" = not categorized yet
	Status      string // "pending" or "posted"
	Amount      Cents
}

type CategoryTotal struct {
	Category string // "" = not categorized yet
	Total    Cents
	Count    int
}

// Report is the spending in a period: a month, a week, or any range.
type Report struct {
	From, To   time.Time // [From, To)
	Total      Cents
	Categories []CategoryTotal // largest first
	Purchases  []Purchase      // newest first

	Income      Cents // money in during the period (income entries, sign flipped)
	IncomeCount int
}

// SpendingReport lists purchases (transactions with expense entries) that
// occurred in [from, to), skipping voided ones, and totals them per category.
// The caller picks the period, so the same query serves months and weeks.
func SpendingReport(ctx context.Context, db DB, from, to time.Time) (Report, error) {
	r := Report{From: from, To: to}
	rows, err := db.Query(ctx,
		`SELECT t.id, t.occurred_at, t.description, COALESCE(t.category, ''), t.status,
		        SUM(e.amount_cents)
		 FROM transactions t
		 JOIN entries e  ON e.transaction_id = t.id
		 JOIN accounts a ON a.id = e.account_id
		 WHERE a.type = 'expense' AND t.status <> 'voided'
		   AND t.occurred_at >= $1 AND t.occurred_at < $2
		 GROUP BY t.id
		 ORDER BY t.occurred_at DESC, t.id DESC`, from, to)
	if err != nil {
		return r, err
	}
	defer rows.Close()

	totals := map[string]*CategoryTotal{}
	for rows.Next() {
		var p Purchase
		if err := rows.Scan(&p.ID, &p.OccurredAt, &p.Description, &p.Category, &p.Status, &p.Amount); err != nil {
			return r, err
		}
		r.Purchases = append(r.Purchases, p)
		r.Total += p.Amount
		if totals[p.Category] == nil {
			totals[p.Category] = &CategoryTotal{Category: p.Category}
		}
		totals[p.Category].Total += p.Amount
		totals[p.Category].Count++
	}
	if err := rows.Err(); err != nil {
		return r, err
	}

	for _, t := range totals {
		r.Categories = append(r.Categories, *t)
	}
	sort.Slice(r.Categories, func(i, j int) bool { return r.Categories[i].Total > r.Categories[j].Total })

	// Money in: income entries are credits (negative), so flip the sign.
	err = db.QueryRow(ctx,
		`SELECT COALESCE(-SUM(e.amount_cents), 0), COUNT(DISTINCT t.id)
		 FROM transactions t
		 JOIN entries e  ON e.transaction_id = t.id
		 JOIN accounts a ON a.id = e.account_id
		 WHERE a.type = 'income' AND t.status <> 'voided'
		   AND t.occurred_at >= $1 AND t.occurred_at < $2`, from, to).Scan(&r.Income, &r.IncomeCount)
	return r, err
}

// TxRow is one transaction as listed on the transactions page.
type TxRow struct {
	ID          int64
	OccurredAt  time.Time
	Description string
	Status      string // pending, posted or voided
	Source      string // tap, bank or manual
	Category    string
	Account     string // the card or bank account involved, e.g. "Assets:ChaseChecking"
	Amount      Cents  // signed for that account: money out is negative
	Matched     bool   // a tap confirmed by its bank row
}

// ListTransactions lists every transaction in [from, to), newest first:
// taps and bank rows, spending, transfers and income, including voided ones.
func ListTransactions(ctx context.Context, db DB, from, to time.Time) ([]TxRow, error) {
	rows, err := db.Query(ctx, `
		SELECT * FROM (
		  SELECT DISTINCT ON (t.id)
		         t.id, t.occurred_at, t.description, t.status, t.source, COALESCE(t.category, ''),
		         a.name, e.amount_cents, (t.source = 'tap' AND t.external_id IS NOT NULL)
		  FROM transactions t
		  JOIN entries e  ON e.transaction_id = t.id
		  JOIN accounts a ON a.id = e.account_id
		  WHERE t.occurred_at >= $1 AND t.occurred_at < $2
		    AND a.type IN ('asset', 'liability')
		  -- show the real account, not the transfer clearing account
		  ORDER BY t.id, (a.name = 'Assets:Transfers'), a.name
		) x
		ORDER BY occurred_at DESC, id DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TxRow
	for rows.Next() {
		var r TxRow
		if err := rows.Scan(&r.ID, &r.OccurredAt, &r.Description, &r.Status, &r.Source, &r.Category,
			&r.Account, &r.Amount, &r.Matched); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
