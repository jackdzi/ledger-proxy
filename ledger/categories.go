package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// Categories are labels for reports. Money movement stays in entries;
// a category only says what a purchase was for.
var Categories = []string{"food", "going_out", "transportation", "other"}

// Classifier returns a merchant's category, e.g. by asking the local LLM.
// Tests pass a fake one.
type Classifier func(ctx context.Context, merchant string) (string, error)

// Payment-processor prefixes some card networks put before the merchant name:
// Square, Toast, Shopify, PayPal, point of sale.
var processorPrefixes = []string{"sq", "tst", "sp", "pp", "pos"}

// Normalize turns a raw merchant name into the form rules use, so a chain's
// different store numbers share one rule:
//
//	"SQ *BLUE BOTTLE 0423" → "blue bottle"
//	"Trader Joe's #123"    → "trader joes"
func Normalize(merchant string) string {
	merchant = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(merchant))
	words := strings.FieldsFunc(merchant, func(r rune) bool { return !unicode.IsLetter(r) })
	if len(words) > 1 && slices.Contains(processorPrefixes, words[0]) {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// RuleFor finds the category for a merchant: the longest rule whose pattern
// appears in the normalized name as whole words ("bar" matches "the bar
// downtown" but not "barnes noble").
func RuleFor(ctx context.Context, db DB, merchant string) (string, bool, error) {
	name := Normalize(merchant)
	if name == "" {
		return "", false, nil
	}
	var category string
	err := db.QueryRow(ctx,
		`SELECT category FROM rules
		 WHERE ' ' || $1 || ' ' LIKE '% ' || pattern || ' %'
		 ORDER BY length(pattern) DESC
		 LIMIT 1`, name).Scan(&category)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return category, err == nil, err
}

// SaveRule records merchant pattern → category. An existing rule for the
// same pattern is kept (yours, or an earlier answer), never overwritten.
func SaveRule(ctx context.Context, db DB, pattern, category, source string) error {
	_, err := db.Exec(ctx,
		`INSERT INTO rules (pattern, category, source) VALUES ($1, $2, $3)
		 ON CONFLICT (pattern) DO NOTHING`, pattern, category, source)
	return err
}

// CategorizePending gives every uncategorized spending transaction a
// category: from a matching rule if there is one, otherwise by asking
// classify and saving the answer as a rule (so each merchant is asked about
// once). It stops at the first classifier error (e.g. the model is down)
// and returns how many it categorized; the rest are retried next time.
func CategorizePending(ctx context.Context, db DB, classify Classifier) (int, error) {
	done := 0
	var after int64
	for {
		// Next uncategorized transaction with a spending entry.
		var id int64
		var merchant string
		err := db.QueryRow(ctx,
			`SELECT t.id, t.description FROM transactions t
			 WHERE t.category IS NULL AND t.status <> 'voided' AND t.id > $1
			   AND EXISTS (SELECT 1 FROM entries e JOIN accounts a ON a.id = e.account_id
			               WHERE e.transaction_id = t.id AND a.type = 'expense')
			 ORDER BY t.id LIMIT 1`, after).Scan(&id, &merchant)
		if errors.Is(err, pgx.ErrNoRows) {
			return done, nil
		}
		if err != nil {
			return done, err
		}
		after = id

		category, found, err := RuleFor(ctx, db, merchant)
		if err != nil {
			return done, err
		}
		if !found {
			pattern := Normalize(merchant)
			if pattern == "" {
				category = "other" // nothing to learn from, e.g. "#123"
			} else {
				// Classify the normalized name: noise like "SQ *… 0423" made the
				// model answer "other" for a café; "blue bottle" gets "food".
				category, err = classify(ctx, pattern)
				if err != nil {
					return done, fmt.Errorf("classify %d: %w", id, err)
				}
				if !slices.Contains(Categories, category) {
					return done, fmt.Errorf("classify %d: unknown category %q", id, category)
				}
				if err := SaveRule(ctx, db, pattern, category, "llm"); err != nil {
					return done, err
				}
			}
		}

		// Only fill in a missing category; never overwrite one.
		if _, err := db.Exec(ctx,
			`UPDATE transactions SET category = $2 WHERE id = $1 AND category IS NULL`,
			id, category); err != nil {
			return done, err
		}
		done++
	}
}
