#!/usr/bin/env bash
# Q1: what the gateway adds to a request. Runs the same open-loop load
# directly against the fake provider and through the gateway, per rate and
# mode, and writes one bench result per run plus meta.json under
# eval/results/q1-overhead/. `make results` renders them into RESULTS.md.
#
#   eval/bench/q1.sh                              # the full run (make bench)
#   RATES=50 DURATION=3s WARMUP=1s eval/bench/q1.sh   # a quick smoke
set -euo pipefail

cd "$(dirname "$0")/../.."

rates=${RATES:-50 200 500}
duration=${DURATION:-60s}
warmup=${WARMUP:-5s}
fake_latency=20ms
fake_tokens=30
fake_interval=5ms
fake_addr=127.0.0.1:9101
gateway_addr=127.0.0.1:8080
out=eval/results/q1-overhead
command="RATES='$rates' DURATION=$duration WARMUP=$warmup eval/bench/q1.sh"

logs=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q1.XXXXXX")
pids=()
cleanup() {
	local status=$?
	for pid in ${pids[@]+"${pids[@]}"}; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in ${pids[@]+"${pids[@]}"}; do
		wait "$pid" 2>/dev/null || true
	done
	if [[ $status -ne 0 ]]; then
		echo "q1: failed; server logs in $logs" >&2
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
	echo "q1: $1 at $2 not healthy after 10 s" >&2
	return 1
}

# Commit of the code under test; results from earlier runs do not count as
# a dirty tree.
commit=$(git rev-parse --short HEAD)
if ! git diff --quiet HEAD -- . ':(exclude)eval/results' ||
	[[ -n $(git ls-files --others --exclude-standard -- . ':(exclude)eval/results') ]]; then
	commit+=-dirty
fi
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build -o bin/switchboard ./cmd/switchboard

bin/switchboard fake-provider --listen "$fake_addr" --latency "$fake_latency" \
	--tokens "$fake_tokens" --token-interval "$fake_interval" >"$logs/fake-provider.log" 2>&1 &
pids+=($!)
SWITCHBOARD_CONFIG=eval/bench/switchboard.yaml bin/switchboard serve >"$logs/gateway.log" 2>&1 &
pids+=($!)
wait_healthy fake-provider "$fake_addr"
wait_healthy gateway "$gateway_addr"

mkdir -p "$out"
rm -f "$out"/*.json
cat >"$out/meta.json" <<JSON
{
  "commit": "$commit",
  "date": "$date",
  "fake_provider": {
    "latency": "$fake_latency",
    "tokens": $fake_tokens,
    "token_interval": "$fake_interval"
  },
  "command": "$command"
}
JSON

run() { # run <target> <url> <mode> <rate>
	local target=$1 url=$2 mode=$3 rate=$4 stream=()
	if [[ $mode == stream ]]; then
		stream=(--stream)
	fi
	bin/switchboard bench --url "$url" --format openai --model fake --key sk-bench \
		${stream[@]+"${stream[@]}"} --rate "$rate" --duration "$duration" --warmup "$warmup" \
		--label "$target-$mode-$rate" --out "$out/$target-$mode-$rate.json"
}

i=0
for rate in $rates; do
	# Alternate which side goes first so slow drift (thermal, background
	# load) does not always favour the same one.
	if ((i % 2 == 0)); then
		order=(direct gateway)
	else
		order=(gateway direct)
	fi
	for mode in nonstream stream; do
		for target in "${order[@]}"; do
			if [[ $target == direct ]]; then
				run direct "http://$fake_addr" "$mode" "$rate"
			else
				run gateway "http://$gateway_addr" "$mode" "$rate"
			fi
		done
	done
	i=$((i + 1))
done

echo "q1: results in $out; server logs in $logs. Run 'make results' to render RESULTS.md." >&2
