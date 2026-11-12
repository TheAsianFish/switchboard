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
# SCHEDULE=flap is M8's run (`make chaos-flap`, PLAN.md M8): the same fleet
# and streams for 150 s, with Redis cut off from the compose network and
# reconnected eight times (`docker network disconnect` / `connect`, so it
# keeps its state) once the budget is spent, replica 2 killed 3 s into the
# fifth outage and started again 2 s after it ends:
#
#   50 + 12k s  disconnect Redis (k = 0..7)   58 + 12k s  connect Redis
#   101 s       kill replica 2                108 s       start replica 2
#
# Just before each action, and once after the run, it reads the budgeted
# key's balance from Redis (redis-cli inside the container, which works
# while it is off the network) into snapshots.json, so the report can set
# Redis's spent against the fake provider's ledger phase by phase.
#
# REDIS_PERSIST=none|everysec|always is M9's arm of the Q6 schedule
# (`make chaos-persist`, PLAN.md M9): Redis runs with the append-only file
# off, or on with that fsync policy, in a fresh /data volume, so the
# restart at 75 s replays what the file holds. Without REDIS_PERSIST the
# run is Q6's (no file) and writes eval/results/q6-chaos/; with it, the
# results go to eval/results/m9-persist/chaos-<mode>/.
#
# Every run keeps each replica's and Redis's log (docker's timestamps
# prefixed) under logs/ in the results directory, so the report can read
# when each replica's limiter, budget and cache gave up on Redis and when
# they found it again, and with what error.
#
#   chaos/run.sh                                  # the full run (make chaos, about 3 minutes)
#   REDIS_PERSIST=everysec chaos/run.sh           # M9's everysec arm
#   SCALE=0.25 OUT=/tmp/q6 chaos/run.sh           # a 30 s smoke: schedule and duration × 0.25
#   KEEP=1 chaos/run.sh                           # leave the fleet up afterwards
#   SCHEDULE=flap OUT=eval/results/m8-flap/before chaos/run.sh   # make chaos-flap RUN=before
set -euo pipefail

cd "$(dirname "$0")/.."

scale=${SCALE:-1}
sched=${SCHEDULE:-q6}
case $sched in
q6 | flap) ;;
*)
	echo "q6: SCHEDULE must be q6 or flap, got $sched" >&2
	exit 2
	;;
esac
persist=${REDIS_PERSIST:-}
case $persist in
"") appendonly=no fsync=everysec ;;
none) appendonly=no fsync=everysec ;;
everysec | always) appendonly=yes fsync=$persist ;;
*)
	echo "q6: REDIS_PERSIST must be none, everysec or always, got $persist" >&2
	exit 2
	;;
esac
export REDIS_APPENDONLY=$appendonly REDIS_APPENDFSYNC=$fsync
main_rate=${MAIN_RATE:-200}
limited_rate=${LIMITED_RATE:-30}
budget_rate=${BUDGET_RATE:-10}
default_out=eval/results/q6-chaos
command="make chaos"
if [[ -n $persist ]]; then
	default_out=eval/results/m9-persist/chaos-$persist
	command="REDIS_PERSIST=$persist make chaos"
fi
out=${OUT:-$default_out}
if [[ $sched == flap ]]; then
	command="make chaos-flap RUN=$(basename "$out")"
fi
if [[ -n ${SCALE:-}${MAIN_RATE:-}${LIMITED_RATE:-}${BUDGET_RATE:-} ]]; then
	command="SCHEDULE=$sched SCALE=$scale MAIN_RATE=$main_rate LIMITED_RATE=$limited_rate BUDGET_RATE=$budget_rate${persist:+ REDIS_PERSIST=$persist} chaos/run.sh"
fi
scaled() { awk -v s="$scale" -v v="$1" 'BEGIN { printf "%.3f", s * v }'; }
if [[ $sched == flap ]]; then
	duration=$(scaled 150)
else
	duration=$(scaled 120)
fi

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
		"${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
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

# snapshot <label>: the budgeted key's balance as Redis holds it, appended
# to the snapshots file. Read inside the container, so it works while Redis
# is off the network; a failed read is skipped, not invented.
budget_key='sb:bud:{budget:total}:bal'
snapshot() {
	local t vals spent reserved
	t=$(now)
	if ! vals=$("${compose[@]}" exec -T redis redis-cli --raw HMGET "$budget_key" spent reserved 2>/dev/null); then
		echo "q6: snapshot before $1 failed" >&2
		return 0
	fi
	spent=$(sed -n 1p <<<"$vals")
	reserved=$(sed -n 2p <<<"$vals")
	printf '{"label": "%s", "unix": %s, "spent_nanodollars": %s, "reserved_nanodollars": %s}\n' \
		"$1" "$t" "${spent:-0}" "${reserved:-0}" >>"$tmp/snapshots.jsonl"
}

# act <planned offset> <action> <service> <phase it starts>: runs the
# docker action at t0 + offset and appends it to the events file. A
# snapshot of Redis is taken 0.5 s before the action (none while Redis is
# killed: there is no process to ask).
act() {
	local planned=$1 action=$2 target=$3 phase=$4 issued done
	sleep_until "$(awk -v t="$t0" -v o="$(scaled "$planned")" 'BEGIN { printf "%.6f", t + o - 0.5 }')"
	snapshot "$action $target"
	sleep_until "$(awk -v t="$t0" -v o="$(scaled "$planned")" 'BEGIN { printf "%.6f", t + o }')"
	issued=$(now)
	case $action in
	kill) "${compose[@]}" kill -s SIGKILL "$target" >/dev/null 2>&1 ;;
	start) "${compose[@]}" start "$target" >/dev/null 2>&1 ;;
	disconnect) docker network disconnect "$network" "$(container "$target")" >/dev/null ;;
	connect) docker network connect --alias "$target" "$network" "$(container "$target")" >/dev/null ;;
	esac
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

network=${project}_default
container() { "${compose[@]}" ps -aq "$1"; }

schedule_flap() {
	local k at
	for k in 0 1 2 3 4 5 6 7; do
		at=$((50 + 12 * k))
		act "$at" disconnect redis "outage $((k + 1))"
		if [[ $k == 0 ]]; then
			sleep_until "$(awk -v t="$t0" -v o="$(scaled 54)" 'BEGIN { printf "%.6f", t + o }')"
			overview redis-down
		fi
		if [[ $k == 4 ]]; then
			act 101 kill switchboard-2 "outage 5, replica 2 down"
			act 106 connect redis "back 5, replica 2 down"
			act 108 start switchboard-2 "back 5"
		else
			act $((at + 8)) connect redis "back $((k + 1))"
		fi
	done
}

schedule() {
	if [[ $sched == flap ]]; then
		schedule_flap
		return
	fi
	act 30 kill switchboard-2 "replica down"
	act 60 kill redis "replica and Redis down"
	sleep_until "$(awk -v t="$t0" -v o="$(scaled 67.5)" 'BEGIN { printf "%.6f", t + o }')"
	overview redis-down
	act 75 start redis "replica down, Redis back"
	act 90 start switchboard-2 "all back"
}

go build -o "$bin" ./cmd/switchboard
"${compose[@]}" build switchboard-1 >/dev/null
# A fresh anonymous /data volume for Redis, so an append-only file from an
# earlier run is never replayed into this one.
"${compose[@]}" up -d --force-recreate --renew-anon-volumes >/dev/null 2>&1
wait_for "fake provider" curl -fsS "$fake/healthz"
for port in 29091 29092 29093; do
	wait_for "replica admin on $port" curl -fsS "http://127.0.0.1:$port/metrics"
done
wait_for "load balancer" curl -fsS "$lb/healthz"
curl -fsS -X POST "$fake/ledger/reset" >/dev/null
: >"$tmp/events.jsonl"
: >"$tmp/snapshots.jsonl"

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
snapshot end
{
	echo "["
	sed '$!s/$/,/' "$tmp/snapshots.jsonl"
	echo "]"
} >"$tmp/snapshots.json"
# Each service's whole log, both lives of a restarted container included,
# every line prefixed with docker's receive time.
mkdir -p "$tmp/logs"
for svc in switchboard-1 switchboard-2 switchboard-3 redis; do
	"${compose[@]}" logs --no-color --no-log-prefix --timestamps "$svc" 2>/dev/null | gzip -9n >"$tmp/logs/$svc.log.gz"
done
# Where the append-only file lives, and on what filesystem.
data_fs=$("${compose[@]}" exec -T redis stat -f -c %T /data 2>/dev/null || echo unknown)
aof_bytes=$("${compose[@]}" exec -T redis sh -c 'du -sb /data/appendonlydir 2>/dev/null | cut -f1' 2>/dev/null || true)

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
  "schedule": "$sched",
  "redis_persist": "${persist:-none}",
  "redis": {"appendonly": "$appendonly", "appendfsync": "$fsync", "data_fs": "$data_fs", "aof_bytes_at_end": ${aof_bytes:-0}},
  "limit": {"model": "fake-limited", "rate": 10, "period_seconds": 1},
  "budget": {"model": "fake-budget", "usd": "0.20", "grace_usd": "0.01", "grace_refill": "1h", "input_price": "3", "output_price": "15"},
  "fake_provider": {"latency": "20ms", "tokens": 30, "token_interval": "5ms"}
}
JSON

mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.json.gz "$out"/*.jsonl.gz
rm -rf "$out/logs"
cp "$tmp"/meta.json "$tmp"/events.json "$tmp"/ledger.json.gz "$out"/
cp "$tmp"/main.json "$tmp"/limited.json "$tmp"/budget.json "$tmp"/*.jsonl.gz "$out"/
cp "$tmp"/overview-*.json "$tmp"/snapshots.json "$out"/ 2>/dev/null || true
cp -r "$tmp/logs" "$out/"
"$bin" chaos report --dir "$out"
echo "q6: results in $out. Run 'make results' to render RESULTS.md." >&2
