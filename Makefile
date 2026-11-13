SHELL := bash
GOLANGCI_VERSION := v2.15.0
BIN := $(CURDIR)/bin
GOLANGCI := $(BIN)/golangci-lint
export GOTOOLCHAIN ?= auto

.PHONY: help tools check fmt vet lint test build test-integration dev fleet bench test-limits bench-hedge bench-stall chaos chaos-flap chaos-orphan chaos-probe chaos-retry chaos-persist bench-persist sim-budget eval-cache eval-cache-data eval-router eval-router-data eval-router-embed eval-router-label sdk-smoke record-fixtures results ui ui-check docker dist check-install clean

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

ui: ## build the admin page into internal/admin/ui/dist (Node 22+)
	cd web && npm ci && npm run build

ui-check: ## typecheck, lint, test and rebuild the admin page; fails if the committed build is stale
	cd web && npm ci && npm run typecheck && npm run lint && npm test && npm run build
	@git diff --exit-code -- internal/admin/ui/dist || { echo "internal/admin/ui/dist is stale: run make ui and commit internal/admin/ui/dist"; exit 1; }
	@if [ -n "$$(git status --porcelain -- internal/admin/ui/dist | grep '^??')" ]; then \
		echo "untracked files in internal/admin/ui/dist: run make ui and commit internal/admin/ui/dist"; exit 1; \
	fi

# With SWITCHBOARD_TEST_REDIS_URL set, the LOADING tests (internal/redistest)
# also need SWITCHBOARD_TEST_REDIS_LOADING_URL, a second Redis started with
# --enable-debug-command yes, and skip without it; with-redis.sh starts both.
test-integration: ## Lua scripts against a real redis:8 ($$SWITCHBOARD_TEST_REDIS_URL, or throwaway containers)
	@if [ -n "$$SWITCHBOARD_TEST_REDIS_URL" ]; then \
		go test -race -count=1 -tags integration ./...; \
	else \
		scripts/with-redis.sh go test -race -count=1 -tags integration ./...; \
	fi

dev: ## one gateway, Redis, Ollama, the fake provider, Jaeger and Prometheus in compose
	docker compose -f deploy/compose.yaml --profile dev up --build

fleet: ## three gateways behind nginx sharing Redis, plus Ollama, the fake provider, Jaeger and Prometheus
	docker compose -f deploy/compose.yaml --profile fleet up --build

bench: ## Q1: gateway overhead through the fake provider, passthrough and per feature (about 26 minutes; Docker for Redis, Jaeger and Ollama)
	eval/bench/q1.sh

test-limits: ## Q2: over-admission and shared budget across three replicas in compose (about 12 minutes)
	eval/limits/run.sh

eval-cache: ## Q3: semantic cache threshold sweep (Ollama with all-minilm and nomic-embed-text) and FLAT vs HNSW in a throwaway redis:8
	go run ./cmd/switchboard eval cache
	scripts/with-redis.sh go run ./cmd/switchboard eval cache-index

eval-cache-data: ## rebuild the Q3 pair sets in eval/cache/data (Ollama with qwen2.5:3b, about 2.5 h on a laptop CPU; resumes)
	go run ./cmd/switchboard eval cache-data

ROUTER_MODELS := qwen2.5:0.5b,qwen2.5:1.5b,qwen2.5:3b

eval-router: ## Q5: 5-fold cross-validated router frontier against the random-mix line, from the committed recordings ($$0, offline)
	go run ./cmd/switchboard eval router run --models "$(or $(MODELS),$(ROUTER_MODELS))"
	go run ./cmd/switchboard eval results

eval-router-embed: ## Q5: re-embed the eval set's prompts (Ollama with nomic-embed-text)
	go run ./cmd/switchboard eval router embed

eval-router-data: ## rebuild the Q5 eval set in eval/router/data (downloads GSM8K and MMLU test splits)
	go run ./cmd/switchboard eval router data

eval-router-label: ## Q5 labels: every candidate answers every question through the gateway (local ladder by default; resumes; MODELS=claude-... with a key)
	eval/router/label.sh

sdk-smoke: build ## unmodified OpenAI and Anthropic Python SDKs streaming through the gateway (needs Ollama)
	eval/sdk/smoke.sh

record-fixtures: ## re-capture provider fixtures from Ollama
	scripts/record-fixtures.sh ollama
	scripts/record-fixtures.sh ollama-tools

bench-hedge: ## Q4: hedging off and on through the fake provider's slow tail (about 3 minutes)
	eval/hedge/run.sh

bench-stall: ## M10: a stalled provider through one gateway, before and after the first-token timeout (about 6 minutes)
	eval/stall/run.sh

chaos: ## Q6: replica and Redis kills under load in a 3-replica compose fleet (about 3 minutes; REDIS_PERSIST=none|everysec|always for M9's arms)
	chaos/run.sh

chaos-persist: ## M9: the Q6 schedule once per Redis append-only mode, none, everysec and always (about 10 minutes; MODES= to pick)
	for m in $(or $(MODES),none everysec always); do REDIS_PERSIST=$$m chaos/run.sh || exit 1; done

bench-persist: ## M9: Q1's limits-and-budget config at 200 req/s against Redis with each append-only mode (about 11 minutes)
	eval/bench/persist.sh

chaos-flap: ## M8: Redis cut off and reconnected eight times, a replica killed while degraded (about 4 minutes; RUN=before|after)
	@test -n "$(RUN)" || { echo "set RUN, e.g. make chaos-flap RUN=after"; exit 1; }
	SCHEDULE=flap OUT=eval/results/m8-flap/$(RUN) chaos/run.sh

chaos-orphan: ## M12: a replica killed with budgeted streams in flight, spent reconciled past the 30 s hold TTL (about 3 minutes; RUN=before|after, BEFORE_REF=c22dd75)
	@test -n "$(RUN)" || { echo "set RUN, e.g. make chaos-orphan RUN=after"; exit 1; }
	SCHEDULE=orphan RUN=$(RUN) chaos/run.sh

chaos-probe: ## M13: M8's flap schedule with every store call over 50 ms logged, attributed by component into eval/results/m13-outage/<RUN>/ (about 4 minutes; RUN=before|after, before builds the replicas from BEFORE_REF, default 42f5f0c)
	@test -n "$(RUN)" || { echo "set RUN, e.g. make chaos-probe RUN=after"; exit 1; }
	SCHEDULE=flap OUT=eval/results/m13-outage/$(RUN) CHAOS_COMMAND="make chaos-probe RUN=$(RUN)" \
		$(if $(filter before,$(RUN)),REPLICA_REF=$(or $(BEFORE_REF),42f5f0c)) chaos/run.sh
	go run ./cmd/switchboard chaos outage --dir eval/results/m13-outage/$(RUN)

chaos-retry: ## M13: the Q6 schedule with REDIS_PERSIST=everysec at RETRY_INTERVAL=1s and 250ms, three runs each, interleaved, into eval/results/m13-outage/retry-<interval>/run-<n>/ (about 25 minutes; RUNS=, INTERVALS=)
	for n in $(or $(RUNS),1 2 3); do for iv in $(or $(INTERVALS),1s 250ms); do \
		RETRY_INTERVAL=$$iv REDIS_PERSIST=everysec OUT=eval/results/m13-outage/retry-$$iv/run-$$n \
		CHAOS_COMMAND="RETRY_INTERVAL=$$iv REDIS_PERSIST=everysec make chaos" chaos/run.sh || exit 1; \
		go run ./cmd/switchboard chaos outage --dir eval/results/m13-outage/retry-$$iv/run-$$n || exit 1; done; done

sim-budget: ## M11, re-run in M12: seeded fault schedules against the budget ledger, as written and with a planted bug, into eval/results/m12-budget-sim/ (SEEDS=100000, about 30 minutes on 4 cores)
	go run ./cmd/switchboard eval budget-sim -seeds $(or $(SEEDS),100000)

results: ## render RESULTS.md and the README results table from eval/results/
	go run ./cmd/switchboard eval results

docker: ## build the container image
	docker build -t switchboard:dev .

dist: ## release archives for linux/darwin x amd64/arm64 and SHA256SUMS in dist/ (VERSION=v1.2.3)
	scripts/dist.sh

check-install: ## go install from a bare copy of this repo in a clean golang container, then serve a request (REF=HEAD)
	scripts/check-install.sh $(or $(REF),HEAD)

clean:
	rm -rf $(BIN) dist
