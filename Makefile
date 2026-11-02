GO ?= go

.PHONY: build test vet lint ci run-center run-agent

## build compiles every package.
build:
	$(GO) build ./...

## test runs the unit tests.
test:
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
	$(GO) run ./cmd/agent run
