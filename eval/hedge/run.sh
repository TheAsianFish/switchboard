#!/usr/bin/env bash
# Q4: does hedging cut the tail, and at what cost in extra requests? Runs
# the same open-loop streaming load through the gateway with hedging off
# and on, against the fake provider with a log-normal first-byte delay and
# a 5% slow tail (10x). Each arm gets a fresh gateway (an empty latency
# window) and an emptied provider ledger; the ledger counts what really
# went upstream. Raw results go to eval/results/q4-hedge/; `make results`
# renders them into RESULTS.md.
#
#   eval/hedge/run.sh                                  # the full run (make bench-hedge)
#   RATE=50 DURATION=5s WARMUP=2s eval/hedge/run.sh    # a quick smoke
set -euo pipefail

cd "$(dirname "$0")/../.."

rate=${RATE:-200}
duration=${DURATION:-60s}
warmup=${WARMUP:-5s}
arms=${ARMS:-off on}
fake_latency=50ms
fake_sigma=0.3
fake_slow_fraction=0.05
fake_slow_factor=10
fake_tokens=30
fake_interval=5ms
fake_seed=${SEED:-1}
fake_addr=127.0.0.1:9101
gateway_addr=127.0.0.1:8080
admin_addr=127.0.0.1:9190
out=${OUT:-eval/results/q4-hedge}
command="${ARMS:+ARMS='$arms' }${OUT:+OUT=$out }RATE=$rate DURATION=$duration WARMUP=$warmup SEED=$fake_seed eval/hedge/run.sh"

logs=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q4.XXXXXX")
pids=()
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
	for pid in ${pids[@]+"${pids[@]}"}; do
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	if [[ $status -ne 0 ]]; then
		echo "q4: failed; server logs in $logs" >&2
	fi
	exit "$status"
}
trap cleanup EXIT

wait_healthy() { # wait_healthy <name> <addr>
	local i
	for i in $(seq 1 100); do
		if curl -fsS -o /dev/null "http://$2/healthz" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	echo "q4: $1 at $2 not healthy after 10 s" >&2
	return 1
}

commit=$(git rev-parse --short HEAD)
if ! git diff --quiet HEAD -- . ':(exclude)eval/results' ||
	[[ -n $(git ls-files --others --exclude-standard -- . ':(exclude)eval/results') ]]; then
	commit+=-dirty
fi
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build -o bin/switchboard ./cmd/switchboard

bin/switchboard fake-provider --listen "$fake_addr" --latency "$fake_latency" \
	--latency-sigma "$fake_sigma" --slow-fraction "$fake_slow_fraction" --slow-factor "$fake_slow_factor" \
	--seed "$fake_seed" --tokens "$fake_tokens" --token-interval "$fake_interval" >"$logs/fake-provider.log" 2>&1 &
pids+=($!)
wait_healthy fake-provider "$fake_addr"

mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.prom
cat >"$out/meta.json" <<JSON
{
  "commit": "$commit",
  "date": "$date",
  "fake_provider": {
    "latency": "$fake_latency",
    "latency_sigma": $fake_sigma,
    "slow_fraction": $fake_slow_fraction,
    "slow_factor": $fake_slow_factor,
    "tokens": $fake_tokens,
    "token_interval": "$fake_interval",
    "seed": $fake_seed
  },
  "rate": $rate,
  "duration": "$duration",
  "warmup": "$warmup",
  "command": "$command"
}
JSON

for arm in $arms; do
	curl -fsS -X POST "http://$fake_addr/ledger/reset"
	SWITCHBOARD_CONFIG=eval/hedge/$arm.yaml bin/switchboard serve >"$logs/gateway-$arm.log" 2>&1 &
	gateway=$!
	wait_healthy gateway "$gateway_addr"
	bin/switchboard bench --url "http://$gateway_addr" --format openai --model fake --key sk-bench \
		--stream --samples --rate "$rate" --duration "$duration" --warmup "$warmup" \
		--label "hedge-$arm" --out "$out/$arm.json"
	# Cancelled attempts close their upstream connections asynchronously;
	# give the provider a moment to record them before reading the ledger.
	sleep 1
	curl -fsS "http://$fake_addr/ledger" | bin/switchboard eval hedge-ledger >"$out/$arm-ledger.json"
	curl -fsS "http://$admin_addr/metrics" | grep -E '^switchboard_provider_(hedges|retries|first_token_timeouts)_total' >"$out/$arm-metrics.prom" || true
	curl -fsS "http://$admin_addr/providers" >"$out/$arm-providers.json"
	stop_gateway
done

echo "q4: results in $out; server logs in $logs. Run 'make results' to render RESULTS.md." >&2
