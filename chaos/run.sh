#!/usr/bin/env bash
# Q6: what a replica kill and a Redis kill cost a fleet under load. Starts
# the chaos fleet (chaos/compose.yaml: three gateways behind nginx, Redis
# 8, the fake provider), drives three `switchboard bench` streams at the
# load balancer and runs PLAN.md's kill schedule against it:
#
#   30 s  kill replica 2 (SIGKILL)     60 s  kill Redis (SIGKILL)
#   75 s  start Redis (empty)          90 s  start replica 2
#
# The streams: the main workload (200 streamed requests per second, 30% of
# them repeats of 50 prompts, exact cache), a key limited to 10 requests
# per second offered 30, and a key with a $0.20 budget offered 10 requests
# per second, which runs out before Redis is killed. Every request is
# logged with its scheduled time, every docker action with when it ran;
# `switchboard chaos report` turns them and the fake provider's ledger into
# eval/results/q6-chaos/summary.json, and `make results` into RESULTS.md.
#
#   chaos/run.sh                                  # the full run (make chaos, about 3 minutes)
#   SCALE=0.25 OUT=/tmp/q6 chaos/run.sh           # a 30 s smoke: schedule and duration × 0.25
#   KEEP=1 chaos/run.sh                           # leave the fleet up afterwards
set -euo pipefail

cd "$(dirname "$0")/.."

scale=${SCALE:-1}
main_rate=${MAIN_RATE:-200}
limited_rate=${LIMITED_RATE:-30}
budget_rate=${BUDGET_RATE:-10}
out=${OUT:-eval/results/q6-chaos}
command="make chaos"
if [[ -n ${SCALE:-}${MAIN_RATE:-}${LIMITED_RATE:-}${BUDGET_RATE:-} ]]; then
	command="SCALE=$scale MAIN_RATE=$main_rate LIMITED_RATE=$limited_rate BUDGET_RATE=$budget_rate chaos/run.sh"
fi
scaled() { awk -v s="$scale" -v v="$1" 'BEGIN { printf "%.3f", s * v }'; }
duration=$(scaled 120)

project=switchboard-chaos
compose=(docker compose -f chaos/compose.yaml -p "$project")
lb=http://127.0.0.1:28080
fake=http://127.0.0.1:29101
tmp=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q6.XXXXXX")
bin=$tmp/switchboard
pids=()

cleanup() {
	local status=$?
	for pid in ${pids[@]+"${pids[@]}"}; do
		kill "$pid" 2>/dev/null || true
	done
	if [[ $status -ne 0 ]]; then
		echo "q6: failed; replica logs follow, work files in $tmp" >&2
		"${compose[@]}" logs --tail 20 switchboard-1 switchboard-2 switchboard-3 lb >&2 || true
	fi
	if [[ -z ${KEEP:-} ]]; then
		"${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
	fi
	exit "$status"
}
trap cleanup EXIT

# Commit of the code under test; results from earlier runs do not count as
# a dirty tree.
commit=$(git rev-parse --short HEAD)
if ! git diff --quiet HEAD -- . ':(exclude)eval/results' ||
	[[ -n $(git ls-files --others --exclude-standard -- . ':(exclude)eval/results') ]]; then
	commit+=-dirty
fi

now() { date +%s.%N; }

wait_for() { # wait_for <what> <command...>
	local what=$1 i
	shift
	for i in $(seq 1 150); do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.2
	done
	echo "q6: $what not ready after 30 s" >&2
	return 1
}

# sleep_until <unix seconds>
sleep_until() {
	local d
	d=$(awk -v t="$1" -v n="$(now)" 'BEGIN { d = t - n; printf "%.3f", (d > 0 ? d : 0) }')
	sleep "$d"
}

# act <planned offset> <action> <service> <phase it starts>: runs the
# docker action at t0 + offset and appends it to the events file.
act() {
	local planned=$1 action=$2 target=$3 phase=$4 issued done
	sleep_until "$(awk -v t="$t0" -v o="$(scaled "$planned")" 'BEGIN { printf "%.6f", t + o }')"
	issued=$(now)
	if [[ $action == kill ]]; then
		"${compose[@]}" kill -s SIGKILL "$target" >/dev/null 2>&1
	else
		"${compose[@]}" start "$target" >/dev/null 2>&1
	fi
	done=$(now)
	echo "q6: $(awk -v t="$t0" -v d="$done" 'BEGIN { printf "%6.2f", d - t }') s  $action $target" >&2
	printf '{"name": "%s %s", "action": "%s", "target": "%s", "phase": "%s", "planned_seconds": %s, "issued_unix": %s, "done_unix": %s}\n' \
		"$action" "$target" "$action" "$target" "$phase" "$(scaled "$planned")" "$issued" "$done" >>"$tmp/events.jsonl"
}

# overview <name>: one replica's view of the fleet from the admin API, if
# it has one; best effort.
overview() {
	curl -fsS --max-time 2 -H 'Host: localhost' http://127.0.0.1:29091/api/overview >"$tmp/overview-$1.json" 2>/dev/null ||
		rm -f "$tmp/overview-$1.json"
}

schedule() {
	act 30 kill switchboard-2 "replica down"
	act 60 kill redis "replica and Redis down"
	sleep_until "$(awk -v t="$t0" -v o="$(scaled 67.5)" 'BEGIN { printf "%.6f", t + o }')"
	overview redis-down
	act 75 start redis "replica down, Redis back"
	act 90 start switchboard-2 "all back"
}

go build -o "$bin" ./cmd/switchboard
"${compose[@]}" build switchboard-1 >/dev/null
"${compose[@]}" up -d --force-recreate >/dev/null 2>&1
wait_for "fake provider" curl -fsS "$fake/healthz"
for port in 29091 29092 29093; do
	wait_for "replica admin on $port" curl -fsS "http://127.0.0.1:$port/metrics"
done
wait_for "load balancer" curl -fsS "$lb/healthz"
curl -fsS -X POST "$fake/ledger/reset" >/dev/null
: >"$tmp/events.jsonl"

echo "q6: ${duration} s of load, schedule × $scale" >&2
t0=$(now)
"$bin" bench --url "$lb" --key sk-chaos-main --model fake-chaos --stream --rate "$main_rate" \
	--duration "${duration}s" --warmup 0s --repeat 0.3 --repeat-pool 50 --seed 1 --cache exact \
	--label main --log "$tmp/main.jsonl.gz" --out "$tmp/main.json" 2>"$tmp/main.err" &
pids+=($!)
"$bin" bench --url "$lb" --key sk-chaos-limited --model fake-limited --rate "$limited_rate" \
	--duration "${duration}s" --warmup 0s --label limited --log "$tmp/limited.jsonl.gz" --out "$tmp/limited.json" 2>"$tmp/limited.err" &
pids+=($!)
"$bin" bench --url "$lb" --key sk-chaos-budget --model fake-budget --rate "$budget_rate" --max-tokens 64 \
	--duration "${duration}s" --warmup 0s --label budget --log "$tmp/budget.jsonl.gz" --out "$tmp/budget.json" 2>"$tmp/budget.err" &
pids+=($!)
schedule
for pid in "${pids[@]}"; do
	wait "$pid"
done
pids=()
cat "$tmp"/main.err "$tmp"/limited.err "$tmp"/budget.err >&2
# Settlement and the ledger's last entries land as the final replies end.
sleep 1
overview end
curl -fsS "$fake/ledger" | gzip -9n >"$tmp/ledger.json.gz"

{
	echo "["
	sed '$!s/$/,/' "$tmp/events.jsonl"
	echo "]"
} >"$tmp/events.json"
cat >"$tmp/meta.json" <<JSON
{
  "commit": "$commit",
  "date": "$(date -u -d "@${t0%.*}" +%Y-%m-%dT%H:%M:%SZ)",
  "command": "$command",
  "replicas": 3,
  "duration_seconds": $duration,
  "time_scale": $scale,
  "limit": {"model": "fake-limited", "rate": 10, "period_seconds": 1},
  "budget": {"model": "fake-budget", "usd": "0.20", "grace_usd": "0.01", "input_price": "3", "output_price": "15"},
  "fake_provider": {"latency": "20ms", "tokens": 30, "token_interval": "5ms"}
}
JSON

mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.json.gz "$out"/*.jsonl.gz
cp "$tmp"/meta.json "$tmp"/events.json "$tmp"/ledger.json.gz "$out"/
cp "$tmp"/main.json "$tmp"/limited.json "$tmp"/budget.json "$tmp"/*.jsonl.gz "$out"/
cp "$tmp"/overview-*.json "$out"/ 2>/dev/null || true
"$bin" chaos report --dir "$out"
echo "q6: results in $out. Run 'make results' to render RESULTS.md." >&2
