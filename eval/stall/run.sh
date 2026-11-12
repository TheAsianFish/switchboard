#!/usr/bin/env bash
# M10: does a provider that stops answering ever open the breaker? Drives
# one gateway with an open-loop streaming load against the fake provider
# (Q4's latency shape) through three phases: steady, stalled (the fake
# accepts every request and never answers it) and recovered. Each arm gets
# a fresh gateway and fake provider:
#
#   before  the gateway built from BEFORE_REF (the commit before the
#           first-token timeout existed) with eval/stall/before.yaml
#   after   this tree's gateway with eval/stall/after.yaml (a 2 s
#           first-token timeout)
#   off     this tree's gateway with before.yaml (the setting off; not in
#           the default arms)
#
# While the load runs, the provider's breaker state is read from the
# admin listener's /providers every 50 ms. Raw files go to
# eval/results/m10-stall/<arm>/; `switchboard eval stall` summarises them
# and `make results` renders RESULTS.md.
#
#   eval/stall/run.sh                                   # make bench-stall
#   STEADY=5 STALL=15 RECOVER=10 ARMS=after OUT=/tmp/s eval/stall/run.sh   # a quick smoke
set -euo pipefail

cd "$(dirname "$0")/../.."

rate=${RATE:-20}
steady=${STEADY:-20}
stall=${STALL:-60}
recover=${RECOVER:-40}
timeout=${TIMEOUT:-30s}
arms=${ARMS:-before after}
before_ref=${BEFORE_REF:-ed0dedf}
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
poll_interval=0.05
out=${OUT:-eval/results/m10-stall}
duration=$((steady + stall + recover))
command="${ARMS:+ARMS='$arms' }${OUT:+OUT=$out }RATE=$rate STEADY=$steady STALL=$stall RECOVER=$recover TIMEOUT=$timeout SEED=$fake_seed eval/stall/run.sh"

work=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-stall.XXXXXX")
pids=()
worktree=
cleanup() {
	local status=$?
	for pid in ${pids[@]+"${pids[@]}"}; do
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	if [[ -n $worktree ]]; then
		git worktree remove --force "$worktree" 2>/dev/null || true
	fi
	if [[ $status -ne 0 ]]; then
		echo "stall: failed; scratch files in $work" >&2
	fi
	exit "$status"
}
trap cleanup EXIT

stop() { # stop <pid>
	kill "$1" 2>/dev/null || true
	wait "$1" 2>/dev/null || true
}

wait_healthy() { # wait_healthy <name> <addr>
	for _ in $(seq 1 100); do
		if curl -fsS -o /dev/null "http://$2/healthz" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	echo "stall: $1 at $2 not healthy after 10 s" >&2
	return 1
}

# poll <file>: one JSON line per read of /providers, stamped with the
# wall clock in Unix seconds, until killed.
poll() {
	local t body
	while true; do
		t=$(date +%s.%N)
		if body=$(curl -fsS --max-time 1 "http://$admin_addr/providers" 2>/dev/null); then
			printf '{"t":%s,"body":%s}\n' "$t" "$body"
		fi
		sleep "$poll_interval"
	done >"$1"
}

commit=$(git rev-parse --short HEAD)
if ! git diff --quiet HEAD -- . ':(exclude)eval/results' ||
	[[ -n $(git ls-files --others --exclude-standard -- . ':(exclude)eval/results') ]]; then
	commit+=-dirty
fi
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build -o bin/switchboard ./cmd/switchboard
if [[ " $arms " == *" before "* ]]; then
	before_commit=$(git rev-parse --short "$before_ref")
	worktree="$work/before-src"
	git worktree add --detach "$worktree" "$before_ref" >/dev/null 2>&1
	(cd "$worktree" && go build -o "$work/switchboard-before" ./cmd/switchboard)
	git worktree remove --force "$worktree"
	worktree=
fi

mkdir -p "$out"
cat >"$out/meta.json" <<JSON
{
  "commit": "$commit",
  "date": "$date",
  "before_ref": "${before_commit:-}",
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
  "steady_seconds": $steady,
  "stall_seconds": $stall,
  "recover_seconds": $recover,
  "client_timeout": "$timeout",
  "poll_interval_seconds": $poll_interval,
  "arms": "$arms",
  "command": "$command"
}
JSON

for arm in $arms; do
	case $arm in
	before) bin="$work/switchboard-before" config=eval/stall/before.yaml gateway_commit=${before_commit} ;;
	after) bin=bin/switchboard config=eval/stall/after.yaml gateway_commit=$commit ;;
	off) bin=bin/switchboard config=eval/stall/before.yaml gateway_commit=$commit ;;
	*)
		echo "stall: unknown arm $arm" >&2
		exit 2
		;;
	esac
	dir="$out/$arm"
	rm -rf "$dir"
	mkdir -p "$dir"
	echo "stall: arm $arm ($duration s: $steady steady, $stall stalled, $recover recovered)" >&2

	bin/switchboard fake-provider --listen "$fake_addr" --latency "$fake_latency" \
		--latency-sigma "$fake_sigma" --slow-fraction "$fake_slow_fraction" --slow-factor "$fake_slow_factor" \
		--seed "$fake_seed" --tokens "$fake_tokens" --token-interval "$fake_interval" >"$work/fake-$arm.log" 2>&1 &
	fake=$!
	pids+=("$fake")
	wait_healthy fake-provider "$fake_addr"
	SWITCHBOARD_CONFIG=$config "$bin" serve >"$dir/gateway.log" 2>&1 &
	gateway=$!
	pids+=("$gateway")
	wait_healthy gateway "$gateway_addr"
	poll "$dir/breaker.jsonl" &
	poller=$!
	pids+=("$poller")

	bin/switchboard bench --url "http://$gateway_addr" --format openai --model fake --key sk-bench \
		--stream --rate "$rate" --duration "${duration}s" --warmup 0s --timeout "$timeout" \
		--label "stall-$arm" --log "$dir/requests.jsonl.gz" --out "$dir/bench.json" &
	load=$!
	pids+=("$load")
	sleep "$steady"
	curl -fsS -X POST "http://$fake_addr/stall/on"
	sleep "$stall"
	curl -fsS -X POST "http://$fake_addr/stall/off"
	wait "$load"

	stop "$poller"
	curl -fsS "http://$fake_addr/stall" >"$dir/stall.json"
	curl -fsS "http://$fake_addr/ledger" | gzip -n >"$dir/ledger.json.gz"
	curl -fsS "http://$admin_addr/metrics" | grep -E '^switchboard_provider_' >"$dir/metrics.prom" || true
	stop "$gateway"
	stop "$fake"
	gzip -n "$dir/breaker.jsonl"
	gzip -n "$dir/gateway.log"
	cat >"$dir/meta.json" <<JSON
{
  "arm": "$arm",
  "gateway_commit": "$gateway_commit",
  "config": "$config"
}
JSON
	cp "$config" "$dir/gateway.yaml"
done

echo "stall: results in $out. Run 'switchboard eval stall' via 'make results' to render RESULTS.md." >&2
