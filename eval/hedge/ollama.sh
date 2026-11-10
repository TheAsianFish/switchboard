#!/usr/bin/env bash
# Q4, the local check: is the fake provider's tail realistic, and does
# hedging help against a real backend? Runs streamed requests through the
# gateway to a local Ollama with hedging off and on, at a low rate a CPU
# can serve. Ollama must be running with the model pulled. Raw results go
# to eval/results/q4-hedge-ollama/; `make results` renders them.
#
#   OLLAMA_URL=http://127.0.0.1:11434 eval/hedge/ollama.sh
#   RATE=1 DURATION=20s WARMUP=10s eval/hedge/ollama.sh   # a quick smoke
set -euo pipefail

cd "$(dirname "$0")/../.."

ollama=${OLLAMA_URL:-http://127.0.0.1:11434}
model=${MODEL:-qwen2.5:0.5b}
rate=${RATE:-1}
duration=${DURATION:-180s}
warmup=${WARMUP:-30s}
max_tokens=${MAX_TOKENS:-16}
gateway_addr=127.0.0.1:8080
admin_addr=127.0.0.1:9190
out=${OUT:-eval/results/q4-hedge-ollama}
command="MODEL=$model RATE=$rate DURATION=$duration WARMUP=$warmup MAX_TOKENS=$max_tokens eval/hedge/ollama.sh"

logs=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q4o.XXXXXX")
gateway=
stop_gateway() {
	if [[ -n $gateway ]]; then
		kill "$gateway" 2>/dev/null || true
		wait "$gateway" 2>/dev/null || true
		gateway=
	fi
}
cleanup() {
	local status=$?
	stop_gateway
	if [[ $status -ne 0 ]]; then
		echo "q4-ollama: failed; logs in $logs" >&2
	fi
	exit "$status"
}
trap cleanup EXIT

wait_healthy() {
	local i
	for i in $(seq 1 100); do
		if curl -fsS -o /dev/null "http://$1/healthz" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	echo "q4-ollama: gateway at $1 not healthy after 10 s" >&2
	return 1
}

version=$(curl -fsS "$ollama/api/version" | sed -E 's/.*"version":"([^"]*)".*/\1/') || {
	echo "q4-ollama: no Ollama at $ollama" >&2
	exit 1
}

commit=$(git rev-parse --short HEAD)
if ! git diff --quiet HEAD -- . ':(exclude)eval/results' ||
	[[ -n $(git ls-files --others --exclude-standard -- . ':(exclude)eval/results') ]]; then
	commit+=-dirty
fi
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build -o bin/switchboard ./cmd/switchboard

# The hedge window is smaller than the synthetic run's: at one request a
# second, 1000 samples would take the whole run to fill.
config() { # config <enabled>
	cat <<YAML
listen: $gateway_addr
admin_listen: $admin_addr
log: {format: json, level: warn}
providers:
  - name: ollama
    type: openaicompat
    base_url: $ollama/v1
    timeout: 120s
    resilience:
      hedge: {enabled: $1, quantile: 0.95, window: 200, min_samples: 20, budget: 0.1, burst: 10}
models:
  - {name: local, provider: ollama, upstream: "$model"}
keys:
  - {name: bench, key: sk-bench}
YAML
}

# One untimed request loads the model, so neither arm pays for it.
curl -fsS "$ollama/api/generate" -d "{\"model\":\"$model\",\"prompt\":\"hi\",\"stream\":false,\"options\":{\"num_predict\":1}}" >/dev/null

mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.prom
cat >"$out/meta.json" <<JSON
{
  "commit": "$commit",
  "date": "$date",
  "model": "$model",
  "ollama_version": "$version",
  "rate": $rate,
  "duration": "$duration",
  "warmup": "$warmup",
  "max_tokens": $max_tokens,
  "command": "$command"
}
JSON

for arm in off on; do
	enabled=false
	[[ $arm == on ]] && enabled=true
	config "$enabled" >"$logs/$arm.yaml"
	SWITCHBOARD_CONFIG="$logs/$arm.yaml" bin/switchboard serve >"$logs/gateway-$arm.log" 2>&1 &
	gateway=$!
	wait_healthy "$gateway_addr"
	bin/switchboard bench --url "http://$gateway_addr" --format openai --model local --key sk-bench \
		--stream --samples --rate "$rate" --duration "$duration" --warmup "$warmup" \
		--max-tokens "$max_tokens" --timeout 120s --label "ollama-hedge-$arm" --out "$out/$arm.json"
	curl -fsS "http://$admin_addr/metrics" | grep -E '^switchboard_provider_(hedges|retries)_total' >"$out/$arm-metrics.prom" || true
	curl -fsS "http://$admin_addr/providers" >"$out/$arm-providers.json"
	stop_gateway
done

echo "q4-ollama: results in $out; logs in $logs. Run 'make results' to render RESULTS.md." >&2
