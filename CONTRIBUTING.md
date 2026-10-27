# Contributing

This file is the contributor guide and the first thing to read before working
on the repository. PLAN.md is the source of truth for scope and milestones;
RESULTS.md (from M1) records every measured number.

## Setup

- Go 1.27 (the `go` directive in `go.mod`; `GOTOOLCHAIN=auto` downloads it on
  an older install; if that download is blocked, lower the directive to the
  installed version, nothing in the code needs a newer language level).
- Docker with Compose v2 (Redis 8, Ollama, Jaeger, Prometheus, the fleet).
- Node 22 and npm for `web/` (from M6).
- `golangci-lint` v2 (`make tools` installs it into `./bin`).
- Optional for the evals: a local Ollama with `all-minilm` and
  `nomic-embed-text` pulled; `ANTHROPIC_API_KEY` in the environment for live
  runs (never in a file, never printed, never committed).

```sh
git clone https://github.com/TheAsianFish/switchboard
cd switchboard
make tools
make check
```

## Commands

| Command | What it does | Where it runs |
|---------|--------------|---------------|
| `make check` | `gofmt`, `go vet`, `golangci-lint`, `go test -race ./...` (offline, under 2 minutes) | every PR |
| `make test-integration` | Lua and vector-index tests against a real `redis:8` container | every PR (service container) |
| `make ui-check` | typecheck, lint and Vitest for `web/` | every PR from M6 |
| `make dev` | `docker compose up` with one replica and Ollama (Redis from M2, Jaeger and Prometheus from M4) | local |
| `make fleet` | the same with three replicas behind the load balancer | local |
| `make sdk-smoke` | unmodified OpenAI and Anthropic Python SDKs stream through the gateway from Ollama (and Claude with a key) | local |
| `make record-fixtures` | re-capture the provider fixtures from Ollama (`scripts/record-fixtures.sh anthropic` for Claude) | local |
| `make bench` | overhead benchmark (Q1) through the fake provider | local, manual workflow |
| `make test-limits` | multi-replica over-admission test (Q2) | local, manual workflow |
| `make eval-cache` | semantic cache threshold sweep (Q3); needs Ollama | local, manual workflow |
| `make bench-hedge` | hedging benchmark (Q4); live part needs a key | local, manual workflow |
| `make eval-router` | router frontier (Q5); replays recordings, labels live with a key | local, manual workflow |
| `make chaos` | the phased kill run (Q6) | local, manual workflow |
| `make results` | re-render RESULTS.md from `eval/results/` | after any of the above |

Per-PR CI runs only the first three. Benchmarks, evals and chaos are `make`
targets and a `workflow_dispatch` workflow; they never run on push.

## Layout and conventions

- `cmd/switchboard` is the only `main`. Everything else is under `internal/`
  and reachable through the subcommands `serve`, `bench`, `fake-provider`,
  `eval` and `chaos`.
- Canonical types live in `internal/core`; front ends and providers translate
  at the edges and never leak wire-format types inward.
- Every provider, store and embedder is an interface with a fake. Unit tests
  use fakes and `miniredis`; nothing in `go test ./...` touches the network.
- Errors: wrap with `fmt.Errorf("...: %w", err)`; sentinel errors in the
  package that owns them; HTTP status mapping only in the front ends.
- Context first argument everywhere; every outbound call has a deadline.
- Logging: `log/slog`, JSON in production, text in `make dev`; request id and
  trace id on every line; never log request bodies or keys.
- Config: `SWITCHBOARD_*` environment variables override `switchboard.yaml`.
  Secrets only from the environment.
- Tests: table-driven, `t.Parallel()` where safe, `-race` always. Recorded
  provider streams are fixtures under `internal/provider/testdata/` and are
  real captures, never hand-written.
- Measured numbers: a harness writes raw JSON under `eval/results/<name>/`,
  `make results` renders RESULTS.md. No number is typed into RESULTS.md or the
  README by hand. Every live run records model, sample size, dollars and
  hardware.
- Frontend (`web/`): React 19, TypeScript `strict`, Vite, Vitest; data from
  the admin API only; no state library until it is needed.

## Quality gates

A PR is mergeable when:

1. `make check`, `make test-integration` and (from M6) `make ui-check` are
   green in CI.
2. The milestone's definition of done in PLAN.md is met and its checkbox is
   ticked in the same PR.
3. Any new number in RESULTS.md comes from a committed raw result file and
   names the command that produced it.
4. No secrets, no `.env` files, no generated binaries, no `node_modules`.

## Commits and pull requests

- One logical step per commit, imperative subject under 72 characters, a body
  only when the why is not obvious. No "WIP" or "fix typo" commits on a
  branch that is up for review: fold them in.
- Branch per milestone (`dev/m1`, `dev/m2`, ...), PR against `main`, squash
  never: the history is the record.
- The PR body says what was built, how it was verified (commands and their
  output) and what is left under "To measure" or "Known problems".
