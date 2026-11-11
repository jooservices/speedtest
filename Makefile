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
ci: vet test build lint test-install test-matrix

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
	shellcheck internal/center/install/install.sh scripts/test-install.sh internal/center/installer/*_remote.sh deploy/prod/prod.sh deploy/prod/entrypoint.sh deploy/observability/observe.sh deploy/observability/probe.sh

## test-install runs ShellCheck and the installer test against a fake download server.
test-install: shellcheck
	sh scripts/test-install.sh

VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null | sed 's/^v//' || true)
ifeq ($(strip $(VERSION)),)
VERSION := 0.0.0-dev+$(shell git rev-parse --short HEAD)
endif
COMMIT := $(shell git rev-parse HEAD)
BUILT_AT := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
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
	python3 scripts/release-manifest.py "$(DIST)" "$(VERSION)" "$(COMMIT)" "$(BUILT_AT)"
	@echo "release $(VERSION) written to $(DIST)/"


.PHONY: test-ui lint-ui

## test-ui checks the contract-backed UI data layer without a database.
test-ui:
	npm test
	npm run test:coverage

## lint-ui checks syntax of the changed browser scripts.
lint-ui:
	npm run lint

.PHONY: e2e
## e2e runs the browser end-to-end scenarios against a real center and real agents (needs .env, psql and a Chromium).
e2e:
	node tests/e2e/main.mjs

.PHONY: perf
## perf runs the dashboard load test (10 users, 50 nodes, ~70k results) against a real center; needs .env and psql.
perf:
	node tests/perf/dashboard.mjs

.PHONY: test-matrix
## test-matrix fails when an operation of docs/openapi.yaml has no test or docs/test-matrix.md is stale.
test-matrix:
	node scripts/test-matrix.mjs

.PHONY: mutation-check
## mutation-check applies five deliberate breakages and requires the suite to catch each (needs .env).
mutation-check:
	python3 scripts/mutation-check.py

.PHONY: integration-e2e
## integration-e2e runs the serial integration suite, the real-center browser E2E (with OpenAPI conformance of every response) and the mutation checks.
integration-e2e:
	python3 scripts/geo-test-fixtures.py
	$(GO) test -race -p 1 -tags integration ./...
	node tests/e2e/main.mjs
	python3 scripts/mutation-check.py

.PHONY: ui-coverage
## ui-coverage runs every browser and unit run that records UI coverage, then fails when a ui/assets/js file is under 85% of lines.
ui-coverage:
	node tests/e2e/main.mjs
	node tests/ui/smoke.mjs
	node tests/ui/wizard.mjs
	node tests/ui/coverage.mjs
	node scripts/ui-coverage.mjs --min 85

.PHONY: test-observability
## test-observability checks the dashboard/alert definitions and runs the Vector config's own tests (needs Docker).
test-observability:
	python3 -m unittest discover -s tests/observability
	docker run --rm -e OPENOBSERVE_URL=http://127.0.0.1:5080 -e OPENOBSERVE_USER=u -e OPENOBSERVE_PASSWORD=p -v "$(PWD)/deploy/observability/vector.yaml":/etc/vector/vector.yaml:ro timberio/vector:0.43.1-alpine test /etc/vector/vector.yaml
