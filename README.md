# Switchboard

One Go binary that sits between your services and your LLM providers. It
speaks the OpenAI and Anthropic APIs in front, fans out to Anthropic, Ollama
or any OpenAI-compatible server behind, streams tokens through unchanged, and
enforces per-key rate limits and dollar budgets that stay correct across
replicas through Redis. On top of that: retries and hedged requests for the
latency tail, an exact and a semantic cache with a measured false-hit rate, a
`model: "auto"` router trained on an offline eval set, cost accounting per key
and model, OpenTelemetry traces, Prometheus metrics and a small admin page.
Every feature ships with the benchmark or eval that says what it is worth.

**Status: in progress.** See [PLAN.md](PLAN.md) for the design, the questions
the project answers by experiment, and the milestones. Numbers will appear in
RESULTS.md as they are measured.

## Licence

MIT, see [LICENSE](LICENSE).
