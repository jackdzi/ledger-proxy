# ledger

A personal double-entry ledger. Apple Pay taps arrive from an iOS Shortcut
and are recorded exactly once, even if the phone sends them many times.

## How it works

- Every purchase is a **transaction** with two **entries** that sum to zero:
  a $4.50 coffee on a credit card is `Expenses:Uncategorized +450` and
  `Liabilities:AppleCard −450`. Amounts are integer cents.
- **Postgres enforces the rules**, not just Go: a transaction that doesn't
  balance can't be committed, and entries can't be edited or deleted.
- **Taps are idempotent.** The server builds a key from the tap (card,
  merchant, amount, time) and saves the response with the transaction in
  one database commit. A retry gets the saved response back instead of
  creating a duplicate.

## Run it

Needs Go and Docker.

```sh
cp cards.example.txt cards.txt   # then list your cards
make db schema                   # start Postgres, create the tables
make run                         # ledgerd on 127.0.0.1:8080
```

Send a tap:

```sh
curl -X POST localhost:8080/v1/taps \
  -H 'Authorization: Bearer dev-tap-token' \
  -d '{"card":"Apple Card","merchant":"Blue Bottle","amount":"$4.50","occurred_at":"'"$(date -u +%FT%TZ)"'"}'
```

Look at the result with `make psql`. Run the tests with `make test`.
`make db-stop` stops Postgres and deletes the dev data.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `DATABASE_URL` | dev database | Postgres, as the `ledger_app` role |
| `TAP_TOKEN` | `dev-tap-token` | Bearer token for `POST /v1/taps` |
| `CARDS_FILE` | `cards.txt` | Which account each card uses |
| `LISTEN_ADDR` | `127.0.0.1:8080` | |

Put real values in `.env` (gitignored). See `PLAN.md` for what's next.
