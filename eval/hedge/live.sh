#!/usr/bin/env bash
# Q4 against a real backend, shared by eval/hedge/ollama.sh (local Ollama)
# and eval/hedge/claude.sh (the Anthropic API): streamed requests through
# the gateway with hedging off and then on, at a low open-loop rate. Run
# one of those two; PROVIDER (ollama or anthropic) selects the backend.
# Raw results go to $OUT; `make results` renders them.
set -euo pipefail

cd "$(dirname "$0")/../.."

provider=${PROVIDER:?eval/hedge/live.sh: run eval/hedge/ollama.sh or eval/hedge/claude.sh}
rate=${RATE:-1}
duration=${DURATION:-180s}
warmup=${WARMUP:-30s}
max_tokens=${MAX_TOKENS:-16}
gateway_addr=127.0.0.1:8080
admin_addr=127.0.0.1:9190

case $provider in
ollama)
	tag=q4-ollama
	ollama=${OLLAMA_URL:-http://127.0.0.1:11434}
	model=${MODEL:-qwen2.5:0.5b}
	out=${OUT:-eval/results/q4-hedge-ollama}
	script=eval/hedge/ollama.sh
	;;
anthropic)
	tag=q4-claude
	model=${MODEL:-claude-haiku-5-5}
	out=${OUT:-eval/results/q4-hedge-claude}
	script=eval/hedge/claude.sh
	# The key is only checked for presence; it is read by the gateway from
	# its environment and never written anywhere by this script.
	if [[ -z ${ANTHROPIC_API_KEY:-} ]]; then
		echo "$tag: ANTHROPIC_API_KEY is not set; this run calls the paid Anthropic API." >&2
		exit 1
	fi
	;;
*)
	echo "eval/hedge/live.sh: PROVIDER must be ollama or anthropic, not '$provider'" >&2
	exit 2
	;;
esac
command="MODEL=$model RATE=$rate DURATION=$duration WARMUP=$warmup MAX_TOKENS=$max_tokens $script"

logs=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-$tag.XXXXXX")
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
		echo "$tag: failed; logs in $logs" >&2
	fi
	exit "$status"
}
trap cleanup EXIT

wait_healthy() {
	for _ in $(seq 1 100); do
		if curl -fsS -o /dev/null "http://$1/healthz" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	echo "$tag: gateway at $1 not healthy after 10 s" >&2
	return 1
}

if [[ $provider == ollama ]]; then
	version=$(curl -fsS "$ollama/api/version" | sed -E 's/.*"version":"([^"]*)".*/\1/') || {
		echo "$tag: no Ollama at $ollama" >&2
		exit 1
	}
fi

commit=$(git rev-parse --short HEAD)
if ! git diff --quiet HEAD -- . ':(exclude)eval/results' ||
	[[ -n $(git ls-files --others --exclude-standard -- . ':(exclude)eval/results') ]]; then
	commit+=-dirty
fi
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build -o bin/switchboard ./cmd/switchboard

# The provider block for the gateway config.
upstream() {
	if [[ $provider == ollama ]]; then
		cat <<YAML
  - name: ollama
    type: openaicompat
    base_url: $ollama/v1
YAML
	else
		cat <<YAML
  - name: anthropic
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
YAML
	fi
}

# The hedge window is smaller than the synthetic run's: at one request a
# second, 1000 samples would take the whole run to fill.
config() { # config <enabled>
	cat <<YAML
listen: $gateway_addr
admin_listen: $admin_addr
log: {format: json, level: warn}
providers:
$(upstream)
    timeout: 120s
    resilience:
      hedge: {enabled: $1, quantile: 0.95, window: 200, min_samples: 20, budget: 0.1, burst: 10}
models:
  - {name: local, provider: $provider, upstream: "$model"}
keys:
  - {name: bench, key: sk-bench}
YAML
}

mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.prom
if [[ $provider == ollama ]]; then
	# One untimed request loads the model, so neither arm pays for it.
	curl -fsS "$ollama/api/generate" -d "{\"model\":\"$model\",\"prompt\":\"hi\",\"stream\":false,\"options\":{\"num_predict\":1}}" >/dev/null
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
	# The hedge and retry counters, as the committed local run has them.
	series='^switchboard_provider_(hedges|retries)_total'
else
	cat >"$out/meta.json" <<JSON
{
  "commit": "$commit",
  "date": "$date",
  "provider": "anthropic",
  "model": "$model",
  "rate": $rate,
  "duration": "$duration",
  "warmup": "$warmup",
  "max_tokens": $max_tokens,
  "command": "$command"
}
JSON
	# The token counters too: they put the run in dollars.
	series='^switchboard_(provider_(hedges|retries)_total|tokens_total)'
fi

for arm in off on; do
	enabled=false
	[[ $arm == on ]] && enabled=true
	config "$enabled" >"$logs/$arm.yaml"
	SWITCHBOARD_CONFIG="$logs/$arm.yaml" bin/switchboard serve >"$logs/gateway-$arm.log" 2>&1 &
	gateway=$!
	wait_healthy "$gateway_addr"
	bin/switchboard bench --url "http://$gateway_addr" --format openai --model local --key sk-bench \
		--stream --samples --rate "$rate" --duration "$duration" --warmup "$warmup" \
		--max-tokens "$max_tokens" --timeout 120s --label "$provider-hedge-$arm" --out "$out/$arm.json"
	curl -fsS "http://$admin_addr/metrics" | grep -E "$series" >"$out/$arm-metrics.prom" || true
	curl -fsS "http://$admin_addr/providers" >"$out/$arm-providers.json"
	stop_gateway
done

echo "$tag: results in $out; logs in $logs. Run 'make results' to render RESULTS.md." >&2
