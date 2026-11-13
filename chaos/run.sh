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
# SCHEDULE=orphan is M12's run (`make chaos-orphan RUN=before|after`,
# PLAN.md M12): the same fleet with chaos/orphan.compose.yaml on top
# (chaos/orphan.yaml: hold TTL 30 s, provider timeout 20 s, a $5 budget;
# the fake provider streams the budget model at 68 ms a token, about 2 s
# a reply). Only the budgeted key sends, streamed, 10 requests per second
# with max_tokens 64, for 60 s, so about seven of its requests are in
# flight on each replica at any time:
#
#   40 s  kill replica 2 (SIGKILL, Redis up; not started again)
#   60 s  load stops                   100 s  last snapshot, ledger read
#
# Every 5 s from 5 s to 100 s, and just before the kill, it reads the
# budgeted key's balance twice: a raw HMGET (what Redis holds) and then
# replica 1's GET /api/overview, which reads it through the ledger's
# balance script, so holds that have expired are purged (and, after the
# fix, charged) before spent is read; once the load stops nothing else
# touches the key. RUN=after runs the working tree; RUN=before builds the
# replicas' image from BEFORE_REF (default c22dd75, the commit before the
# fix) in a git worktree and tags it switchboard:m12-before. The fake
# provider is the working tree's build in both arms; its ledger records
# each request's caller IP, which meta.json maps to a replica, and the
# gateway's hold for it. `switchboard chaos orphan` writes summary.json.
# The results go to eval/results/m12-orphan/<RUN>/; SCALE does not apply
# (the hold TTL is fixed).
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
# RETRY_INTERVAL=1s|250ms (any Go duration) is M13's switch (PLAN.md
# M13): every replica's ratelimit.retry_interval, which also sets how often
# the budget and the cache probe an unreachable Redis. With it, a q6 run's
# results go to eval/results/m13-outage/retry-<interval>/ unless OUT is
# set (`make chaos-retry` runs each arm three times into run-1..3).
#
# REPLICA_REF=<commit> builds the three replicas' image from that commit
# in a git worktree (tagged switchboard:replica-<commit>) and runs them
# through chaos/replica.compose.yaml; the fake provider and the load
# generator stay the working tree's. `make chaos-probe RUN=before` uses it
# for M13's before arm. CHAOS_COMMAND, if set, is the command recorded in
# meta.json.
#
#   chaos/run.sh                                  # the full run (make chaos, about 3 minutes)
#   REDIS_PERSIST=everysec chaos/run.sh           # M9's everysec arm
#   SCALE=0.25 OUT=/tmp/q6 chaos/run.sh           # a 30 s smoke: schedule and duration × 0.25
#   KEEP=1 chaos/run.sh                           # leave the fleet up afterwards
#   SCHEDULE=flap OUT=eval/results/m8-flap/before chaos/run.sh   # make chaos-flap RUN=before
#   SCHEDULE=orphan RUN=before chaos/run.sh       # make chaos-orphan RUN=before
set -euo pipefail

cd "$(dirname "$0")/.."

scale=${SCALE:-1}
sched=${SCHEDULE:-q6}
case $sched in
q6 | flap) ;;
orphan)
	orphan_run=${RUN:-}
	if [[ $orphan_run != before && $orphan_run != after ]]; then
		echo "q6: SCHEDULE=orphan needs RUN=before or RUN=after, got '$orphan_run'" >&2
		exit 2
	fi
	if [[ -n ${SCALE:-}${REDIS_PERSIST:-}${MAIN_RATE:-}${LIMITED_RATE:-} ]]; then
		echo "q6: SCHEDULE=orphan takes no SCALE, REDIS_PERSIST, MAIN_RATE or LIMITED_RATE" >&2
		exit 2
	fi
	before_ref=${BEFORE_REF:-c22dd75}
	;;
*)
	echo "q6: SCHEDULE must be q6, flap or orphan, got $sched" >&2
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
retry=${RETRY_INTERVAL:-}
if [[ -n $retry ]]; then
	if [[ $sched == orphan ]] || ! [[ $retry =~ ^[0-9]+(\.[0-9]+)?(ms|s)$ ]]; then
		echo "q6: RETRY_INTERVAL must be a duration such as 1s or 250ms, and not with SCHEDULE=orphan; got '$retry'" >&2
		exit 2
	fi
fi
export RETRY_INTERVAL=$retry
replica_ref=${REPLICA_REF:-}
if [[ -n $replica_ref && $sched == orphan ]]; then
	echo "q6: SCHEDULE=orphan takes BEFORE_REF, not REPLICA_REF" >&2
	exit 2
fi
main_rate=${MAIN_RATE:-200}
limited_rate=${LIMITED_RATE:-30}
budget_rate=${BUDGET_RATE:-10}
default_out=eval/results/q6-chaos
command="make chaos"
if [[ -n $persist ]]; then
	default_out=eval/results/m9-persist/chaos-$persist
	command="REDIS_PERSIST=$persist make chaos"
fi
if [[ -n $retry ]]; then
	default_out=eval/results/m13-outage/retry-$retry
	command="RETRY_INTERVAL=$retry $command"
	if [[ -n $persist ]]; then
		command="RETRY_INTERVAL=$retry REDIS_PERSIST=$persist make chaos"
	fi
fi
out=${OUT:-$default_out}
if [[ $sched == flap ]]; then
	command="make chaos-flap RUN=$(basename "$out")"
fi
if [[ $sched == orphan ]]; then
	out=${OUT:-eval/results/m12-orphan/$orphan_run}
	command="make chaos-orphan RUN=$orphan_run"
	if [[ -n ${BEFORE_REF:-} && $orphan_run == before ]]; then
		command="BEFORE_REF=$before_ref $command"
	fi
fi
if [[ $sched != orphan && -n ${SCALE:-}${MAIN_RATE:-}${LIMITED_RATE:-}${BUDGET_RATE:-} ]]; then
	command="SCHEDULE=$sched SCALE=$scale MAIN_RATE=$main_rate LIMITED_RATE=$limited_rate BUDGET_RATE=$budget_rate${persist:+ REDIS_PERSIST=$persist} chaos/run.sh"
fi
if [[ -n ${CHAOS_COMMAND:-} ]]; then
	command=$CHAOS_COMMAND
fi
scaled() { awk -v s="$scale" -v v="$1" 'BEGIN { printf "%.3f", s * v }'; }
if [[ $sched == flap ]]; then
	duration=$(scaled 150)
elif [[ $sched == orphan ]]; then
	# The budget stream's load; the run goes on to 100 s.
	duration=60
	if [[ -n ${BUDGET_RATE:-} ]]; then
		command="BUDGET_RATE=$budget_rate $command"
	fi
else
	duration=$(scaled 120)
fi

project=switchboard-chaos
compose=(docker compose -f chaos/compose.yaml -p "$project")
if [[ $sched == orphan ]]; then
	compose=(docker compose -f chaos/compose.yaml -f chaos/orphan.compose.yaml -p "$project")
elif [[ -n $replica_ref ]]; then
	compose=(docker compose -f chaos/compose.yaml -f chaos/replica.compose.yaml -p "$project")
fi
lb=http://127.0.0.1:28080
fake=http://127.0.0.1:29101
tmp=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q6.XXXXXX")
bin=$tmp/switchboard
pids=()
worktree=

cleanup() {
	local status=$?
	if [[ -n $worktree ]]; then
		git worktree remove --force "$worktree" >/dev/null 2>&1 || true
	fi
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
	if [[ $sched == orphan ]]; then
		orphan_balance "$1" "$t" "${spent:-0}" "${reserved:-0}"
		return 0
	fi
	printf '{"label": "%s", "unix": %s, "spent_nanodollars": %s, "reserved_nanodollars": %s}\n' \
		"$1" "$t" "${spent:-0}" "${reserved:-0}" >>"$tmp/snapshots.jsonl"
}

# orphan_balance <label> <unix> <raw spent> <raw reserved>: after the raw
# HMGET, the same balance read through the ledger (replica 1's admin API
# calls Ledger.Balance, which runs the balance script and so purges
# expired holds first). The budget object of the overview is kept as it
# came; null if the read failed.
orphan_balance() {
	local t2 body balance
	t2=$(now)
	balance=null
	if body=$(curl -fsS --max-time 2 -H 'Host: localhost' http://127.0.0.1:29091/api/overview 2>/dev/null); then
		balance=$(grep -o '"budget":{[^}]*}' <<<"$body" | head -n 1 | sed 's/^"budget"://')
		balance=${balance:-null}
	else
		echo "q6: balance read at $1 failed" >&2
	fi
	printf '{"label": "%s", "unix": %s, "spent_nanodollars": %s, "reserved_nanodollars": %s, "ledger_unix": %s, "ledger_balance": %s}\n' \
		"$1" "$2" "$3" "$4" "$t2" "$balance" >>"$tmp/snapshots.jsonl"
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

# schedule_orphan: a snapshot every 5 s to 100 s, the kill at 40 s (act
# takes its own snapshot 0.5 s before).
schedule_orphan() {
	local at
	for at in $(seq 5 5 100); do
		if [[ $at == 40 ]]; then
			act 40 kill switchboard-2 "replica 2 down"
			sleep_until "$(awk -v t="$t0" 'BEGIN { printf "%.6f", t + 41 }')"
			snapshot "t=41"
			continue
		fi
		sleep_until "$(awk -v t="$t0" -v o="$at" 'BEGIN { printf "%.6f", t + o }')"
		snapshot "t=$at"
	done
}

schedule() {
	if [[ $sched == orphan ]]; then
		schedule_orphan
		return
	fi
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

# orphan_finish writes the orphan run's files and its summary.
orphan_finish() {
	jsonl_to_array "$tmp/snapshots.jsonl" >"$tmp/snapshots.json"
	jsonl_to_array "$tmp/events.jsonl" >"$tmp/events.json"
	mkdir -p "$tmp/logs"
	for svc in switchboard-1 switchboard-2 switchboard-3 redis fake-provider; do
		"${compose[@]}" logs --no-color --no-log-prefix --timestamps "$svc" 2>/dev/null | gzip -9n >"$tmp/logs/$svc.log.gz"
	done
	cat >"$tmp/meta.json" <<JSON
{
  "commit": "$commit",
  "gateway_commit": "$gateway_commit",
  "fake_provider_commit": "$commit",
  "run": "$orphan_run",
  "date": "$(date -u -d "@${t0%.*}" +%Y-%m-%dT%H:%M:%SZ)",
  "command": "$command",
  "schedule": "orphan",
  "replicas": 3,
  "replica_ips": {$replica_ips},
  "start_unix": $t0,
  "load_seconds": $duration,
  "observe_seconds": 100,
  "kill": {"target": "switchboard-2", "planned_seconds": 40, "restarted": false},
  "balance_read": "GET /api/overview on switchboard-1: Ledger.Balance, the balance script",
  "budget": {"model": "fake-budget", "usd": "5", "input_price": "3", "output_price": "15", "rate": $budget_rate, "stream": true, "max_tokens": 64, "reservation_ttl": "30s", "provider_timeout": "20s"},
  "fake_provider": {"latency": "20ms", "tokens": 30, "token_interval": "5ms", "budget_token_interval": "68ms"}
}
JSON
	mkdir -p "$out"
	rm -f "$out"/*.json "$out"/*.json.gz "$out"/*.jsonl.gz
	rm -rf "$out/logs"
	cp "$tmp"/meta.json "$tmp"/events.json "$tmp"/snapshots.json "$tmp"/ledger.json.gz "$tmp"/budget.json "$tmp"/budget.jsonl.gz "$out"/
	cp -r "$tmp/logs" "$out/"
	"$bin" chaos orphan --dir "$out"
	echo "q6: results in $out." >&2
}

jsonl_to_array() {
	echo "["
	sed '$!s/$/,/' "$1"
	echo "]"
}

go build -o "$bin" ./cmd/switchboard
if [[ $sched == orphan ]]; then
	# The fake provider (and, for the after arm, the replicas) from the
	# working tree; the before arm's replicas from BEFORE_REF.
	"${compose[@]}" build fake-provider >/dev/null
	gateway_commit=$commit
	export ORPHAN_REPLICA_IMAGE=switchboard:dev
	if [[ $orphan_run == before ]]; then
		gateway_commit=$(git rev-parse --short "$before_ref")
		worktree=$tmp/before-src
		git worktree add --detach "$worktree" "$before_ref" >/dev/null 2>&1
		docker build -q -t switchboard:m12-before "$worktree" >/dev/null
		git worktree remove --force "$worktree"
		worktree=
		ORPHAN_REPLICA_IMAGE=switchboard:m12-before
	fi
elif [[ -n $replica_ref ]]; then
	# The fake provider from the working tree (switchboard:dev); the
	# replicas from REPLICA_REF.
	"${compose[@]}" build fake-provider >/dev/null
	gateway_commit=$(git rev-parse --short "$replica_ref")
	worktree=$tmp/replica-src
	git worktree add --detach "$worktree" "$replica_ref" >/dev/null 2>&1
	docker build -q -t "switchboard:replica-$gateway_commit" "$worktree" >/dev/null
	git worktree remove --force "$worktree"
	worktree=
	export REPLICA_IMAGE=switchboard:replica-$gateway_commit
else
	"${compose[@]}" build switchboard-1 >/dev/null
fi
gateway_commit=${gateway_commit:-$commit}
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

# Each replica's address on the compose network, which the fake
# provider's ledger records as the caller.
replica_ips=
for svc in switchboard-1 switchboard-2 switchboard-3; do
	ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$(container "$svc")")
	replica_ips+="${replica_ips:+, }\"$svc\": \"$ip\""
done

echo "q6: ${duration} s of load, schedule $sched × $scale" >&2
t0=$(now)
if [[ $sched == orphan ]]; then
	"$bin" bench --url "$lb" --key sk-chaos-budget --model fake-budget --stream --rate "$budget_rate" --max-tokens 64 \
		--duration "${duration}s" --warmup 0s --label budget --log "$tmp/budget.jsonl.gz" --out "$tmp/budget.json" 2>"$tmp/budget.err" &
	pids+=($!)
	schedule
	wait "${pids[@]}"
	pids=()
	cat "$tmp"/budget.err >&2
	curl -fsS "$fake/ledger" | gzip -9n >"$tmp/ledger.json.gz"
	snapshot end
	orphan_finish
	exit 0
fi
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
  "gateway_commit": "$gateway_commit",
  "date": "$(date -u -d "@${t0%.*}" +%Y-%m-%dT%H:%M:%SZ)",
  "command": "$command",
  "replicas": 3,
  "retry_interval": "${retry:-1s}",
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
