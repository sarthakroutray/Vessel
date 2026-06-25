PROJECT     := github.com/sarthak/vessel
GOOSE_CMD   := go run -mod=mod github.com/pressly/goose/v3/cmd/goose@v3.24.1
MIGRATIONS  := db/migrations
DATABASE_URL ?= "postgres://vessel:vessel@localhost:5432/vessel?sslmode=disable"

.PHONY: up down build test migrate migrate-down migrate-reset

## up — start Postgres + Redis in the background
up:
	docker compose up -d

## down — stop and remove containers
down:
	docker compose down -v

## build — compile all binaries
build:
	go build ./...

## test — run the full test suite
test:
	go test -count=1 -race ./...

## migrate — apply all pending goose migrations
migrate:
	$(GOOSE_CMD) -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" up

## migrate-down — roll back the last migration
migrate-down:
	$(GOOSE_CMD) -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" down

## migrate-reset — roll back all migrations
migrate-reset:
	$(GOOSE_CMD) -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" reset

## create name="<name>" — scaffold a new migration file
create:
	$(GOOSE_CMD) -dir $(MIGRATIONS) create $(name) sql
