package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestParseDollars(t *testing.T) {
	good := map[string]Cents{"$4.50": 450, "4.5": 450, "4": 400, "$0.01": 1, "$1,234.56": 123456}
	for in, want := range good {
		if got, err := ParseDollars(in); err != nil || got != want {
			t.Errorf("ParseDollars(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "$", "4.555", "-$3", "abc", "4.5.6", "$4.50x", "1234567890"} {
		if _, err := ParseDollars(in); err == nil {
			t.Errorf("ParseDollars(%q) should fail", in)
		}
	}
}

// testDB connects to the dev database (make db schema) as ledger_app.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set: run tests with make test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// accounts creates an expense and a card account unique to this test run.
func accounts(t *testing.T, pool *pgxpool.Pool) (expense, card int64) {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%s:%d", t.Name(), time.Now().UnixNano())
	expense, err := Account(ctx, pool, "Expenses:Test:"+suffix, "expense")
	if err != nil {
		t.Fatal(err)
	}
	card, err = Account(ctx, pool, "Liabilities:Test:"+suffix, "liability")
	if err != nil {
		t.Fatal(err)
	}
	return expense, card
}

func coffee(expense, card int64) Transaction {
	return Transaction{
		OccurredAt: time.Now(), Description: "test coffee", Status: "pending", Source: "tap",
		Entries: []Entry{{AccountID: expense, Amount: 450}, {AccountID: card, Amount: -450}},
	}
}

func TestPostAndBalance(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	expense, card := accounts(t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Post(ctx, tx, coffee(expense, card)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM accounts WHERE id = $1`, card).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if bal, err := Balance(ctx, pool, name); err != nil || bal != -450 {
		t.Fatalf("card balance = %d, %v; want -450", bal, err)
	}
}

// Postgres itself must refuse an unbalanced transaction, even if Go code
// skips Post's check. This test writes the rows with raw SQL.
func TestDatabaseRejectsUnbalanced(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	expense, card := accounts(t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO transactions (occurred_at, description, status, source)
		VALUES (now(), 'bad', 'posted', 'manual') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO entries (transaction_id, account_id, amount_cents)
		VALUES ($1, $2, 100), ($1, $3, -50)`, id, expense, card); err != nil {
		t.Fatal(err)
	}

	if err := tx.Commit(ctx); err == nil {
		t.Fatal("Postgres committed entries summing to +50")
	}
}

func TestPostRejectsUnbalanced(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	expense, card := accounts(t, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	bad := coffee(expense, card)
	bad.Entries[1].Amount = -400
	if _, err := Post(ctx, tx, bad); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("Post(unbalanced) = %v, want ErrUnbalanced", err)
	}
}

func TestEntriesCantBeChanged(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	expense, _ := accounts(t, pool)
	for _, sql := range []string{
		`UPDATE entries SET amount_cents = 999 WHERE account_id = $1`,
		`DELETE FROM entries WHERE account_id = $1`,
	} {
		_, err := pool.Exec(ctx, sql, expense)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" { // permission denied
			t.Errorf("%q: got %v, want permission denied", sql, err)
		}
	}
}

// The MVP's main promise: the same request sent 50 times at once
// creates exactly one transaction, and every caller gets the same answer.
func TestDoConcurrentSameKey(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	expense, card := accounts(t, pool)
	key := fmt.Sprintf("test|%d", time.Now().UnixNano())

	var wg sync.WaitGroup
	responses := make([]Response, 50)
	errs := make([]error, 50)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i], errs[i] = Do(ctx, pool, key, []byte(key), func(tx pgx.Tx) (Response, error) {
				id, err := Post(ctx, tx, coffee(expense, card))
				return Response{Status: 201, Body: []byte(fmt.Sprint(id))}, err
			})
		}()
	}
	wg.Wait()

	for i := range 50 {
		if errs[i] != nil {
			t.Fatalf("request %d failed: %v", i, errs[i])
		}
		if string(responses[i].Body) != string(responses[0].Body) {
			t.Fatalf("different answers: %q vs %q", responses[i].Body, responses[0].Body)
		}
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT transaction_id) FROM entries WHERE account_id = $1`, expense).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d transactions from 50 identical requests, want 1", n)
	}
}

func TestDoKeyReusedForDifferentRequest(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	key := fmt.Sprintf("test|%d", time.Now().UnixNano())
	ok := func(tx pgx.Tx) (Response, error) { return Response{Status: 201, Body: []byte("{}")}, nil }

	if _, err := Do(ctx, pool, key, []byte("first"), ok); err != nil {
		t.Fatal(err)
	}
	if _, err := Do(ctx, pool, key, []byte("second"), ok); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("got %v, want ErrKeyReused", err)
	}
}
