package ledger

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SimpleFIN reads bank data through SimpleFIN Bridge
// (https://www.simplefin.org/protocol.html). Access is a single URL with the
// credentials built in (https://user:pass@host/simplefin), so it's a secret.
type SimpleFIN struct {
	http     *http.Client
	base     string // the access URL without its credentials
	user     string
	password string
}

var ErrAccessRevoked = errors.New("simplefin: access revoked or invalid (403); create a new setup token")

// ClaimSetupToken exchanges a one-time setup token (base64 of a claim URL)
// for the permanent access URL. A token can be claimed only once.
func ClaimSetupToken(ctx context.Context, setupToken string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(setupToken))
	if err != nil {
		return "", fmt.Errorf("simplefin: setup token isn't valid base64: %w", err)
	}
	claimURL, err := url.Parse(string(raw))
	if err != nil || claimURL.Scheme != "https" {
		return "", errors.New("simplefin: setup token doesn't contain an https claim URL")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", claimURL.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("simplefin: claim: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusForbidden {
		return "", errors.New("simplefin: this setup token was already claimed or doesn't exist. " +
			"If you didn't claim it, someone else may have: disable it in SimpleFIN Bridge and make a new one")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("simplefin: claim failed: %s", resp.Status)
	}
	accessURL := strings.TrimSpace(string(body))
	if _, err := NewSimpleFIN(accessURL); err != nil {
		return "", err
	}
	return accessURL, nil
}

// NewSimpleFIN takes the access URL. The credentials are split out and sent
// as HTTP Basic auth, so they never appear in URLs or error messages.
func NewSimpleFIN(accessURL string) (*SimpleFIN, error) {
	u, err := url.Parse(strings.TrimSpace(accessURL))
	if err != nil || u.Scheme != "https" || u.User == nil {
		return nil, errors.New("simplefin: access URL must look like https://user:pass@host/simplefin")
	}
	password, _ := u.User.Password()
	s := &SimpleFIN{http: &http.Client{Timeout: 2 * time.Minute}, user: u.User.Username(), password: password}
	u.User = nil
	s.base = strings.TrimSuffix(u.String(), "/")
	return s, nil
}

// AccountSet is the response of GET /accounts (protocol version 2).
type AccountSet struct {
	Errlist []struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	} `json:"errlist"`
	Errors   []string `json:"errors"` // deprecated, but warnings may still appear here
	Accounts []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Currency     string `json:"currency"`
		Balance      string `json:"balance"`
		Transactions []struct {
			ID           string `json:"id"`
			Posted       int64  `json:"posted"` // Unix time; 0 while pending
			Amount       string `json:"amount"` // signed: purchases are negative
			Description  string `json:"description"`
			TransactedAt int64  `json:"transacted_at"` // Unix time; optional
			Pending      bool   `json:"pending"`
		} `json:"transactions"`
	} `json:"accounts"`
}

// Messages returns the bank's error and warning messages (shown in logs:
// e.g. a login that needs re-authorizing, or a rate-limit warning).
func (a AccountSet) Messages() []string {
	var out []string
	for _, e := range a.Errlist {
		out = append(out, e.Code+": "+e.Msg)
	}
	return append(out, a.Errors...)
}

// Fetch gets all accounts with transactions (including pending) in
// [since, until): the raw response bytes (kept for reprocessing) and the
// parsed set. SimpleFIN allows at most 90 days per request.
func (s *SimpleFIN) Fetch(ctx context.Context, since, until time.Time) ([]byte, AccountSet, error) {
	var set AccountSet
	q := url.Values{
		"version":    {"2"},
		"pending":    {"1"},
		"start-date": {fmt.Sprint(since.Unix())},
		"end-date":   {fmt.Sprint(until.Unix())},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", s.base+"/accounts?"+q.Encode(), nil)
	if err != nil {
		return nil, set, err
	}
	req.SetBasicAuth(s.user, s.password)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, set, fmt.Errorf("simplefin: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, set, fmt.Errorf("simplefin: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return nil, set, ErrAccessRevoked
	case http.StatusPaymentRequired:
		return nil, set, errors.New("simplefin: payment required (402): check the SimpleFIN Bridge subscription")
	default:
		return nil, set, fmt.Errorf("simplefin: %s", resp.Status)
	}
	return body, set, json.Unmarshal(body, &set)
}
