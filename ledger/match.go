package ledger

import (
	"sort"
	"strings"
	"time"
)

// PendingTap is a tap waiting for the bank row that confirms it.
type PendingTap struct {
	ID       int64
	Account  string    // the card's ledger account, e.g. "Assets:ChaseChecking"
	Merchant string    // as Wallet reported it: "Rice Coffeehouse"
	Amount   Cents     // positive: what the tap said was spent
	Date     time.Time // local date of the tap
}

// BankRow is a posted bank transaction not yet in the ledger.
type BankRow struct {
	ExtID   string // "<bank account id>:<bank transaction id>"
	Account string
	Date    time.Time
	Amount  Cents  // signed as the bank reports it: purchases are negative
	Desc    string // "RICE COFFEE HOUSE HOUSTON TX 11/16"
}

type MatchKind string

const (
	MatchExact MatchKind = "exact" // same amount
	MatchTip   MatchKind = "tip"   // bank 0–30% higher than the tap
	MatchHold  MatchKind = "hold"  // whole-dollar authorization that settled at another amount
)

// Match is the decision for one bank row. Tap == nil means no tap fits: the
// row is imported on its own.
type Match struct {
	Row        BankRow
	Tap        *PendingTap
	Kind       MatchKind
	Score      float64 // name similarity, 0–1
	Adjustment Cents   // bank amount − tap amount: a tip is > 0, a hold that settled lower < 0
}

const (
	windowBefore  = 7 * 24 * time.Hour // a tap may be up to a week older than its bank row
	windowAfter   = 24 * time.Hour     // or a day newer (dates are local, posting is not)
	nameThreshold = 0.25               // minimum similarity for exact and tip matches
	holdThreshold = 0.40               // holds prove little by amount, so names must match better
	tieTolerance  = 0.05               // scores this close count as a tie → oldest tap wins
	maxTipPercent = 30
)

// MatchTaps decides, for every bank row, which pending tap it confirms. It
// is pure and deterministic (no database, no clock): rows are handled oldest
// first, each tap is used at most once, and ties go to the oldest tap.
func MatchTaps(rows []BankRow, taps []PendingTap) []Match {
	rows = append([]BankRow(nil), rows...)
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Date.Equal(rows[j].Date) {
			return rows[i].Date.Before(rows[j].Date)
		}
		return rows[i].ExtID < rows[j].ExtID
	})
	used := map[int64]bool{}
	var out []Match
	for _, row := range rows {
		m := Match{Row: row}
		if isPurchase(row) {
			if c := bestCandidate(row, taps, used); c != nil {
				used[c.tap.ID] = true
				m.Tap, m.Kind, m.Score = c.tap, c.kind, c.score
				m.Adjustment = -row.Amount - c.tap.Amount
			}
		}
		out = append(out, m)
	}
	return out
}

type candidate struct {
	tap   *PendingTap
	kind  MatchKind
	score float64
}

var kindRank = map[MatchKind]int{MatchExact: 0, MatchTip: 1, MatchHold: 2}

func bestCandidate(row BankRow, taps []PendingTap, used map[int64]bool) *candidate {
	spent := -row.Amount
	var cands []candidate
	for i := range taps {
		t := &taps[i]
		if used[t.ID] || t.Account != row.Account {
			continue
		}
		if t.Date.Before(row.Date.Add(-windowBefore)) || t.Date.After(row.Date.Add(windowAfter)) {
			continue
		}
		kind, ok := amountFit(t.Amount, spent)
		if !ok {
			continue
		}
		score := Similarity(t.Merchant, row.Desc)
		need := nameThreshold
		if kind == MatchHold {
			need = holdThreshold
		}
		if score >= need {
			cands = append(cands, candidate{t, kind, score})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	// The strongest kind of amount evidence wins (exact > tip > hold); then
	// the best name; scores within tieTolerance of the best are a tie, and
	// the oldest tap wins (first in, first out).
	sort.Slice(cands, func(i, j int) bool {
		if kindRank[cands[i].kind] != kindRank[cands[j].kind] {
			return kindRank[cands[i].kind] < kindRank[cands[j].kind]
		}
		return cands[i].score > cands[j].score
	})
	best := cands[0]
	for _, c := range cands[1:] {
		if c.kind != best.kind || c.score < cands[0].score-tieTolerance {
			break
		}
		if c.tap.Date.Before(best.tap.Date) || (c.tap.Date.Equal(best.tap.Date) && c.tap.ID < best.tap.ID) {
			best = c
		}
	}
	return &best
}

// amountFit says whether a tap of `tap` cents can be the bank charge of `spent` cents.
func amountFit(tap, spent Cents) (MatchKind, bool) {
	switch {
	case spent == tap:
		return MatchExact, true
	case spent > tap && spent*100 <= tap*(100+maxTipPercent):
		return MatchTip, true
	case tap%100 == 0 && spent > 0:
		return MatchHold, true // e.g. a $10.00 pre-authorization that settled at $2.99
	}
	return "", false
}

// isPurchase: money out that isn't a transfer or a payment to a person.
// Only purchases can have taps.
func isPurchase(r BankRow) bool {
	upper := strings.ToUpper(r.Desc)
	return r.Amount < 0 && !containsAny(upper, peerWords) && !containsAny(upper, transferWords)
}

// Words banks add that say nothing about the merchant.
var noiseWords = map[string]bool{"pos": true, "debit": true, "purchase": true, "card": true, "recurring": true}

// Similarity is the Jaccard index of the two names' character trigrams
// (3-letter chunks), after normalizing: "Rice Coffeehouse" and
// "RICE COFFEE HOUSE HOUSTON TX 11/16" share every trigram of the shorter
// name. Trigrams forgive what banks do to names (prefixes, store numbers,
// dates, missing spaces, truncation) while keeping letter order, which a plain
// set of letters would lose.
func Similarity(a, b string) float64 {
	ta, tb := trigrams(a), trigrams(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	shared := 0
	for g := range ta {
		if tb[g] {
			shared++
		}
	}
	return float64(shared) / float64(len(ta)+len(tb)-shared)
}

func trigrams(s string) map[string]bool {
	var b strings.Builder
	for _, w := range strings.Fields(Normalize(s)) {
		if !noiseWords[w] {
			b.WriteString(w) // spaces dropped: "coffee house" = "coffeehouse"
		}
	}
	r := []rune(b.String())
	set := map[string]bool{}
	for i := 0; i+3 <= len(r); i++ {
		set[string(r[i:i+3])] = true
	}
	return set
}
