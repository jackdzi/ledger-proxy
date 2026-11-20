package ledger

import (
	"testing"
	"time"
)

// Real cases from the Wallet screenshot and the bank feed (2026-11-15..17),
// plus distractors. No database needed: MatchTaps is pure.
func TestMatchTapsRealCases(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	const chk = "Assets:ChaseChecking"
	taps := []PendingTap{
		{ID: 1, Account: chk, Merchant: "Copper Kettle", Amount: 1948, Date: day(15)},
		{ID: 2, Account: chk, Merchant: "Elm Coffeehouse", Amount: 475, Date: day(16)},
		{ID: 3, Account: chk, Merchant: "Lfm - Oak U", Amount: 380, Date: day(17)},
		{ID: 4, Account: chk, Merchant: "Bright Pixel Innovation", Amount: 1000, Date: day(17)},
		{ID: 5, Account: chk, Merchant: "Elm Coffeehouse", Amount: 475, Date: day(18)},                          // bank row still pending
		{ID: 6, Account: "Liabilities:UnitedExplorer", Merchant: "Elm Coffeehouse", Amount: 475, Date: day(16)}, // other card
	}
	rows := []BankRow{
		{ExtID: "a", Account: chk, Date: day(15), Amount: -2048, Desc: "SQ *COPPER KETTLE | MAIN Austin TX 11/15"},
		{ExtID: "b", Account: chk, Date: day(16), Amount: -475, Desc: "ELM COFFEE HOUSE AUSTIN TX 11/16"},
		{ExtID: "c", Account: chk, Date: day(17), Amount: -380, Desc: "TST*LFM - OAK U Austin TX 11/17"},
		{ExtID: "d", Account: chk, Date: day(17), Amount: -299, Desc: "BRIGHT PIXEL INNOVAT DAYTON OH 11/17"},
		{ExtID: "e", Account: chk, Date: day(17), Amount: -475, Desc: "SHELL OIL 57442 11/17"},           // same amount, wrong merchant
		{ExtID: "f", Account: chk, Date: day(17), Amount: -2500, Desc: "Zelle payment to Sam 123456789"}, // a person, never a tap
	}

	want := map[string]struct {
		tap  int64
		kind MatchKind
		adj  Cents
	}{
		"a": {1, MatchTip, 100}, // $19.48 tap + $1.00 tip
		"b": {2, MatchExact, 0}, // the OLDER Elm Coffeehouse tap (FIFO), not #5, not the other card's #6
		"c": {3, MatchExact, 0},
		"d": {4, MatchHold, -701}, // $10.00 hold settled at $2.99
		"e": {0, "", 0},           // $4.75 at Shell: no tap's name fits → bank-only
		"f": {0, "", 0},           // Zelle → bank-only
	}
	for _, m := range MatchTaps(rows, taps) {
		w := want[m.Row.ExtID]
		var got int64
		if m.Tap != nil {
			got = m.Tap.ID
		}
		if got != w.tap || m.Kind != w.kind || m.Adjustment != w.adj {
			t.Errorf("row %s (%s): got tap %d %s adj %d (score %.2f); want tap %d %s adj %d",
				m.Row.ExtID, m.Row.Desc, got, m.Kind, m.Adjustment, m.Score, w.tap, w.kind, w.adj)
		}
	}
}

func TestMatchTapsIsDeterministic(t *testing.T) {
	day := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	taps := []PendingTap{
		{ID: 7, Account: "A", Merchant: "Elm Coffeehouse", Amount: 475, Date: day},
		{ID: 8, Account: "A", Merchant: "Elm Coffeehouse", Amount: 475, Date: day},
	}
	rows := []BankRow{
		{ExtID: "y", Account: "A", Date: day, Amount: -475, Desc: "ELM COFFEE HOUSE AUSTIN TX"},
		{ExtID: "x", Account: "A", Date: day, Amount: -475, Desc: "ELM COFFEE HOUSE AUSTIN TX"},
	}
	for i := 0; i < 20; i++ {
		got := MatchTaps(rows, taps)
		if got[0].Row.ExtID != "x" || got[0].Tap.ID != 7 || got[1].Tap.ID != 8 {
			t.Fatalf("run %d: two identical coffees matched differently: %+v", i, got)
		}
	}
}

func TestSimilarity(t *testing.T) {
	for _, c := range []struct {
		a, b    string
		atLeast float64
		below   float64
	}{
		{"Elm Coffeehouse", "ELM COFFEE HOUSE AUSTIN TX 11/16", 0.5, 1.01},
		{"Lfm - Oak U", "TST*LFM - OAK U Austin TX 11/17", 0.3, 1.01},
		{"Juniper Cafe", "POS DEBIT JUNIPER CAFE - ELM Austin TX", 0.3, 1.01},
		{"Copper Kettle", "SQ *COPPER KETTLE | MAIN Austin TX 11/15", 0.3, 1.01},
		{"Elm Coffeehouse", "SHELL OIL 57442 11/17", 0, 0.1},
		{"Copper Kettle", "ELM COFFEE HOUSE AUSTIN TX", 0, 0.1},
	} {
		s := Similarity(c.a, c.b)
		if s < c.atLeast || s >= c.below {
			t.Errorf("Similarity(%q, %q) = %.2f, want in [%.2f, %.2f)", c.a, c.b, s, c.atLeast, c.below)
		}
	}
}
