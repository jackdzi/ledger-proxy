# Dev setup: Postgres 17 in Docker on 127.0.0.1:55432.
# Put real values in .env (gitignored); these defaults are for local dev only.
-include .env
export DATABASE_URL ?= postgres://ledger_app:dev@127.0.0.1:55432/ledger?sslmode=disable
export TAP_TOKEN    ?= dev-tap-token
export ADMIN_PASSWORD ?= dev-admin-password

.PHONY: db schema run test psql db-stop

db:        ## start Postgres and create the app role (data is deleted on db-stop)
	docker run -d --rm --name ledger-pg -p 127.0.0.1:55432:5432 \
	  -e POSTGRES_USER=ledger_owner -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=ledger \
	  postgres:17
	until docker exec ledger-pg pg_isready -h 127.0.0.1 -U ledger_owner; do sleep 0.5; done
	docker exec ledger-pg psql -U ledger_owner -d ledger -c "CREATE ROLE ledger_app LOGIN PASSWORD 'dev'"

schema:    ## create the tables
	docker exec -i ledger-pg psql -U ledger_owner -d ledger -v ON_ERROR_STOP=1 < schema.sql

run:       ## start ledgerd
	go run .

test:
	go test -race -count=1 ./...

psql:      ## open a database shell
	docker exec -it ledger-pg psql -U ledger_owner -d ledger

db-stop:
	docker stop ledger-pg
