GO ?= go

.PHONY: build test vet lint ci run-center run-agent release test-install shellcheck

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
ci: vet test build lint test-install

## run-center starts the command center from the current .env.
run-center:
	$(GO) run ./cmd/center

## run-agent starts a node agent from the current .env.
run-agent:
	$(GO) run ./cmd/agent run

.PHONY: migrate migrate-down test-db

## migrate applies all pending embedded migrations using .env.
migrate:
	$(GO) run ./internal/center/db/cmd/migrate up

## migrate-down rolls back the latest migration (destructive; verify DB first).
migrate-down:
	$(GO) run ./internal/center/db/cmd/migrate down

## test-db exercises migrations and constraints on a verified non-live database.
test-db:
	$(GO) test -p 1 -tags integration ./cmd/center ./internal/center/db/... ./internal/center/geo ./internal/center/ipwatch ./internal/center/nodes ./internal/center/server -count=1 -v

## shellcheck lints the shell scripts shipped or used by the project.
shellcheck:
	shellcheck internal/center/install/install.sh scripts/test-install.sh

## test-install runs ShellCheck and the installer test against a fake download server.
test-install: shellcheck
	sh scripts/test-install.sh

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST ?= dist
LDFLAGS = -s -w -X main.version=$(VERSION)

## release builds static binaries and SHA256SUMS into $(DIST): agent for linux amd64 and arm64,
## center and migrate for linux amd64, plus the agent service unit. Set VERSION=x.y.z to stamp them.
release:
	rm -rf $(DIST)
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/speedtest-agent-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/speedtest-agent-linux-arm64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/speedtest-center-linux-amd64 ./cmd/center
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/speedtest-migrate-linux-amd64 ./internal/center/db/cmd/migrate
	cp deploy/speedtest-agent.service $(DIST)/speedtest-agent.service
	cd $(DIST) && sha256sum speedtest-agent-linux-amd64 speedtest-agent-linux-arm64 speedtest-center-linux-amd64 speedtest-migrate-linux-amd64 speedtest-agent.service > SHA256SUMS
	@echo "release $(VERSION) written to $(DIST)/"
