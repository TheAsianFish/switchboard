#!/usr/bin/env bash
# make sdk-smoke: the unmodified OpenAI and Anthropic Python SDKs stream a
# reply through the gateway from Ollama by both translation paths, and from
# Claude when ANTHROPIC_API_KEY is set (one short prompt per SDK with
# claude-haiku-5-5, well under $0.01). Results go to eval/results/sdk-smoke/.
set -euo pipefail
cd "$(dirname "$0")/../.."

out=eval/results/sdk-smoke
venv=bin/sdk-venv
logs=$(mktemp -d)
model=${OLLAMA_MODEL:-qwen2.5:0.5b}

curl -fsS -m 5 http://127.0.0.1:11434/api/tags >/dev/null || {
	echo "Ollama is not reachable on 127.0.0.1:11434; start it and pull $model" >&2
	exit 1
}
[ -x bin/switchboard ] || go build -o bin/switchboard ./cmd/switchboard
[ -x "$venv/bin/python" ] || python3 -m venv "$venv"
"$venv/bin/pip" install -q -r eval/sdk/requirements.txt

SWITCHBOARD_CONFIG=eval/sdk/switchboard.yaml bin/switchboard serve 2>"$logs/gateway.log" &
gw=$!
trap 'kill $gw 2>/dev/null; wait $gw 2>/dev/null; echo "gateway log: $logs/gateway.log" >&2' EXIT
for _ in $(seq 50); do curl -fsS -m 1 http://127.0.0.1:8090/healthz >/dev/null 2>&1 && break; sleep 0.1; done

rm -rf "$out"
models=(--model "$model=ollama /v1/chat/completions (openaicompat provider)"
	--model "$model-via-messages=ollama /v1/messages (anthropic provider)")
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
	models+=(--model "claude-haiku-5-5=api.anthropic.com (anthropic provider)")
else
	echo "ANTHROPIC_API_KEY not set: skipping Claude" >&2
fi
"$venv/bin/python" eval/sdk/smoke.py --base http://127.0.0.1:8090 --key sk-smoke --out "$out" "${models[@]}"
