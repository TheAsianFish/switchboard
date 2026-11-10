#!/usr/bin/env bash
# Q5 labels: sends every question in eval/router/data/questions.jsonl to
# every candidate through the gateway and records each graded reply under
# eval/recordings/router/. Resumes: a (question, model) pair already
# recorded is not sent again, so a finished run costs $0 to repeat.
#
#   eval/router/label.sh                                  # the local ladder
#   MODELS=claude-haiku-5-5,claude-sonnet-5-5,claude-opus-5-5 eval/router/label.sh
#   LIMIT=10 eval/router/label.sh                         # a quick smoke
set -euo pipefail

cd "$(dirname "$0")/../.."

models=${MODELS:-qwen2.5:0.5b,qwen2.5:1.5b,qwen2.5:3b}
limit=${LIMIT:-0}
ollama=${OLLAMA_URL:-http://127.0.0.1:11434}
addr=127.0.0.1:8091

if [[ $models == *claude* && -z ${ANTHROPIC_API_KEY:-} ]]; then
	echo "router-label: ANTHROPIC_API_KEY is not set; the Claude candidates would refuse every request" >&2
	exit 1
fi
if [[ $models == *qwen* ]] && ! curl -fsS -o /dev/null "$ollama/api/version"; then
	echo "router-label: no Ollama at $ollama" >&2
	exit 1
fi

logs=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q5.XXXXXX")
go build -o bin/switchboard ./cmd/switchboard
SWITCHBOARD_PROVIDER_OLLAMA_BASE_URL="$ollama/v1" SWITCHBOARD_CONFIG=eval/router/switchboard.yaml \
	bin/switchboard serve >"$logs/gateway.log" 2>&1 &
gateway=$!
trap 'kill $gateway 2>/dev/null || true; wait $gateway 2>/dev/null || true' EXIT
for _ in $(seq 1 100); do
	curl -fsS -o /dev/null "http://$addr/healthz" 2>/dev/null && break
	sleep 0.1
done

SWITCHBOARD_EVAL_KEY=sk-router-eval bin/switchboard eval router label \
	--url "http://$addr" --models "$models" --limit "$limit"
echo "router-label: recordings in eval/recordings/router/; gateway log in $logs" >&2
