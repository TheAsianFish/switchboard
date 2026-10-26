SHELL := bash
GOLANGCI_VERSION := v2.15.0
BIN := $(CURDIR)/bin
GOLANGCI := $(BIN)/golangci-lint
export GOTOOLCHAIN ?= auto

.PHONY: help tools check fmt vet lint test build test-integration dev bench sdk-smoke record-fixtures results docker clean

help: ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

tools: ## install golangci-lint into ./bin
	GOTOOLCHAIN=$$(go env GOVERSION) GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

check: fmt vet lint test ## the per-PR gate: gofmt, go vet, golangci-lint, go test -race

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:
	@test -x $(GOLANGCI) || { echo "run 'make tools' first"; exit 1; }
	$(GOLANGCI) run ./...

test:
	go test -race -count=1 ./...

build: ## build ./bin/switchboard
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/switchboard ./cmd/switchboard

test-integration: ## tests against a real redis:8 (from M2)
	go test -race -count=1 -tags integration ./...

dev: ## gateway + Ollama in compose (Redis, Jaeger, Prometheus join in later milestones)
	docker compose -f deploy/compose.yaml up --build

bench: ## Q1: gateway overhead through the fake provider (about 12 minutes)
	eval/bench/q1.sh

sdk-smoke: build ## unmodified OpenAI and Anthropic Python SDKs streaming through the gateway (needs Ollama)
	eval/sdk/smoke.sh

record-fixtures: ## re-capture provider fixtures from Ollama
	scripts/record-fixtures.sh ollama

results: ## render RESULTS.md from eval/results/
	go run ./cmd/switchboard eval results

docker: ## build the container image
	docker build -t switchboard:dev .

clean:
	rm -rf $(BIN) dist
