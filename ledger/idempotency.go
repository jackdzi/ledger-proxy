package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Response is what a request returned. It's saved with the key so a
// retry gets back exactly the same bytes.
type Response struct {
	Status int
	Body   []byte
}

var ErrKeyReused = errors.New("ledger: idempotency key was already used for a different request")

// Do runs fn at most once per key. The first call runs fn and saves its
// response in the same database transaction as fn's changes. Later calls
// with the same key and request get the saved response, without running fn.
func Do(ctx context.Context, pool *pgxpool.Pool, key string, request []byte,
	fn func(tx pgx.Tx) (Response, error)) (Response, error) {

	hash := sha256.Sum256(request)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return Response{}, err
	}
	defer tx.Rollback(ctx) // undoes everything unless we reach Commit

	// Claim the key. If another request with this key is still running,
	// Postgres makes this INSERT wait until that one commits or rolls back.
	tag, err := tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, $2)
		 ON CONFLICT (key) DO NOTHING`,
		key, hash[:])
	if err != nil {
		return Response{}, fmt.Errorf("claim key: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// The key was already used and committed: replay the saved response.
		var savedHash []byte
		var saved Response
		err := tx.QueryRow(ctx,
			`SELECT request_hash, response_code, response_body FROM idempotency_keys WHERE key = $1`,
			key).Scan(&savedHash, &saved.Status, &saved.Body)
		if err != nil {
			return Response{}, fmt.Errorf("read saved response: %w", err)
		}
		if !bytes.Equal(savedHash, hash[:]) {
			return Response{}, ErrKeyReused
		}
		return saved, nil
	}

	// First time we've seen this key: do the work and save the response.
	resp, err := fn(tx)
	if err != nil {
		return Response{}, err // rollback: the key is released, so a retry can run again
	}
	_, err = tx.Exec(ctx,
		`UPDATE idempotency_keys SET response_code = $2, response_body = $3 WHERE key = $1`,
		key, resp.Status, resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("save response: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Response{}, fmt.Errorf("commit: %w", err)
	}
	return resp, nil
}
