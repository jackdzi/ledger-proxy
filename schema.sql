-- Ledger schema. Apply as the owner role (see Makefile):
--   psql "$OWNER_URL" -f schema.sql
-- ledgerd connects as ledger_app, which this file gives limited rights.

-- Accounts: where money lives. Names are like 'Assets:Checking' or
-- 'Expenses:Food'.
CREATE TABLE accounts (
  id    BIGSERIAL PRIMARY KEY,
  name  TEXT NOT NULL UNIQUE,
  type  TEXT NOT NULL CHECK (type IN ('asset', 'liability', 'income', 'expense', 'equity'))
);

-- Transactions: one real-world event (a coffee, a paycheck).
CREATE TABLE transactions (
  id           BIGSERIAL PRIMARY KEY,
  occurred_at  TIMESTAMPTZ NOT NULL,
  description  TEXT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('pending', 'posted', 'voided')),
  source       TEXT NOT NULL CHECK (source IN ('tap', 'bank', 'manual')),
  external_id  TEXT,                     -- the bank's id, once matched
  category     TEXT CHECK (category IN ('food', 'going_out', 'transportation', 'other')),
                                         -- a label for reports; NULL = not categorized yet
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source, external_id)           -- a bank row is recorded once
);

-- The categorizer's queue: transactions still waiting for a category.
CREATE INDEX transactions_uncategorized ON transactions (id) WHERE category IS NULL;

-- Merchant → category. pattern is a normalized merchant name ("blue bottle"),
-- matched as a substring of the normalized merchant; the longest match wins.
CREATE TABLE rules (
  pattern     TEXT PRIMARY KEY,
  category    TEXT NOT NULL CHECK (category IN ('food', 'going_out', 'transportation', 'other')),
  source      TEXT NOT NULL CHECK (source IN ('you', 'llm')),   -- 'you' always wins
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Entries: the money movements. Every transaction has 2+ entries that sum to 0.
-- Positive = debit, negative = credit. Integer cents, never floats.
CREATE TABLE entries (
  id              BIGSERIAL PRIMARY KEY,
  transaction_id  BIGINT NOT NULL REFERENCES transactions(id),
  account_id      BIGINT NOT NULL REFERENCES accounts(id),
  amount_cents    BIGINT NOT NULL CHECK (amount_cents <> 0)
);
CREATE INDEX ON entries (transaction_id);
CREATE INDEX ON entries (account_id);

-- Idempotency keys: one row per client request key, holding the stored response.
-- BYTEA, not JSONB: JSONB reorders keys, so replays wouldn't be byte-identical.
CREATE TABLE idempotency_keys (
  key            TEXT PRIMARY KEY,
  request_hash   BYTEA NOT NULL,
  response_code  INT,
  response_body  BYTEA,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Bank feed (SimpleFIN). Staged here; the matcher (L4) turns rows into ledger transactions.
-- Every response, exactly as received, for reprocessing; also the sync's clock.
CREATE TABLE raw_feed (
  id          BIGSERIAL PRIMARY KEY,
  fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  body        BYTEA NOT NULL          -- the raw JSON bytes
);

-- One row per bank transaction, kept up to date as it moves pending → posted.
-- SimpleFIN ids are unique per account, so the key is (account id, transaction id).
CREATE TABLE bank_transactions (
  bank_account_id  TEXT NOT NULL,             -- SimpleFIN's account id
  bank_txn_id      TEXT NOT NULL,             -- SimpleFIN's transaction id
  account          TEXT NOT NULL,             -- ledger account, e.g. 'Liabilities:AppleCard'
  date             DATE NOT NULL,             -- when it happened (or posted, if that's all we know)
  amount_cents     BIGINT NOT NULL,           -- signed as the bank reports it: purchases are negative
  description      TEXT NOT NULL,             -- raw bank text: 'SQ *BLUE BOTTLE 0423'
  status           TEXT NOT NULL CHECK (status IN ('pending', 'posted')),
  first_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (bank_account_id, bank_txn_id)
);
CREATE INDEX ON bank_transactions (account, date);

-- Rule 1: every transaction balances.
-- Deferred means the check runs at COMMIT, so entries can be inserted one at a
-- time. If any transaction touched doesn't sum to zero, the whole commit fails.
CREATE FUNCTION check_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT SUM(amount_cents) FROM entries WHERE transaction_id = NEW.transaction_id) <> 0 THEN
    RAISE EXCEPTION 'transaction % does not balance', NEW.transaction_id;
  END IF;
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER entries_balanced
  AFTER INSERT ON entries
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION check_balanced();

-- Rule 2: history is never edited.
-- ledger_app can add entries but never change or delete them. A wrong amount
-- is fixed by voiding the transaction and posting a corrected one.
GRANT SELECT, INSERT ON accounts, transactions, entries TO ledger_app;
GRANT UPDATE (status, external_id, category) ON transactions TO ledger_app;   -- pending → posted, link the bank id, set the category
GRANT SELECT, INSERT ON rules TO ledger_app;
GRANT SELECT, INSERT, DELETE ON idempotency_keys TO ledger_app;
GRANT UPDATE (response_code, response_body) ON idempotency_keys TO ledger_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO ledger_app;
GRANT SELECT, INSERT ON raw_feed TO ledger_app;
GRANT SELECT, INSERT, UPDATE ON bank_transactions TO ledger_app;   -- UPDATE: pending → posted
