GO ?= go

.PHONY: build test vet lint ci run-center run-agent

## build compiles every package.
build:
	$(GO) build ./...

## test runs the unit tests.
test:
	python3 scripts/geo-test-fixtures.py
	$(GO) test ./...

## vet reports suspicious constructs.
vet:
	$(GO) vet ./...

## lint runs golangci-lint with the repository configuration.
lint:
	golangci-lint run

## ci runs the same checks as CI: vet, test, build and lint.
ci: vet test build lint

## run-center starts the command center from the current .env.
run-center:
	$(GO) run ./cmd/center

## run-agent starts a node agent from the current .env.
run-agent:
	$(GO) run ./cmd/agent

.PHONY: migrate migrate-down test-db

## migrate applies all pending embedded migrations using .env.
migrate:
	$(GO) run ./internal/center/db/cmd/migrate up

## migrate-down rolls back the latest migration (destructive; verify DB first).
migrate-down:
	$(GO) run ./internal/center/db/cmd/migrate down

## test-db exercises migrations and constraints on a verified non-live database.
test-db:
	$(GO) test -p 1 -tags integration ./internal/center/db/... ./internal/center/geo -count=1 -v
