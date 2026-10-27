#!/usr/bin/env bash
# Q2: how many requests slip past a rate limit that three replicas share
# through Redis, with Redis up and with Redis killed mid-run, and whether a
# budget shared by the same three replicas holds and is accounted to the
# cent. Starts the fleet in compose (three gateways behind nginx, Redis, the
# fake provider), runs `switchboard bench` against the load balancer per
# scenario, and has `switchboard eval limits|budget` compare what reached
# the fake provider's ledger with the limit. Raw results go to
# eval/results/q2-limits/; `make results` renders them into RESULTS.md.
#
#   eval/limits/run.sh                                  # the full run (make test-limits, ~12 min)
#   DURATION=30 OUTAGE_AT=10 OUTAGE_FOR=10 eval/limits/run.sh   # a quick smoke
#   ALGORITHMS=gcra eval/limits/run.sh                  # one algorithm only
set -euo pipefail

cd "$(dirname "$0")/../.."

duration=${DURATION:-180}
outage_at=${OUTAGE_AT:-60}
outage_for=${OUTAGE_FOR:-60}
rate=${RATE:-5} # 300 requests per minute: 3× the limit
algorithms=${ALGORITHMS:-fixed_window gcra}
budget_rate=${BUDGET_RATE:-20}
budget_duration=${BUDGET_DURATION:-30}
out=eval/results/q2-limits
command="DURATION=$duration OUTAGE_AT=$outage_at OUTAGE_FOR=$outage_for RATE=$rate ALGORITHMS='$algorithms' eval/limits/run.sh"
if [[ -z ${DURATION:-}${OUTAGE_AT:-}${OUTAGE_FOR:-}${RATE:-}${ALGORITHMS:-} ]]; then
	command="make test-limits"
fi

project=switchboard-limits
compose=(docker compose -f deploy/compose.yaml -f eval/limits/compose.yaml -p "$project" --profile fleet)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q2.XXXXXX")

cleanup() {
	local status=$?
	if [[ $status -ne 0 ]]; then
		echo "q2: failed; replica logs follow, work files in $tmp" >&2
		"${compose[@]}" logs --tail 30 switchboard-1 switchboard-2 switchboard-3 >&2 || true
	fi
	if [[ -z ${KEEP:-} ]]; then
		"${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
	fi
	exit "$status"
}
trap cleanup EXIT

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
	echo "q2: $what not ready after 30 s" >&2
	return 1
}

replicas_running() {
	local n
	n=$("${compose[@]}" ps --status running --format '{{.Service}}' | grep -c '^switchboard-' || true)
	[[ $n -eq 3 ]]
}

# start_fleet <algorithm> <redis url, empty for none>: (re)creates the three
# replicas and the load balancer; fails if a replica exits.
start_fleet() {
	ALGORITHM=$1 REDIS_URL=$2 "${compose[@]}" up -d --force-recreate --no-deps \
		switchboard-1 switchboard-2 switchboard-3 >/dev/null 2>&1
	"${compose[@]}" up -d --no-deps lb >/dev/null 2>&1
	sleep 1
	if ! replicas_running; then
		return 1
	fi
	local port
	for port in 9091 9092 9093; do
		wait_for "replica admin on $port" curl -fsS "http://127.0.0.1:$port/metrics"
	done
	wait_for "load balancer" curl -fsS http://127.0.0.1:8080/healthz
	# nginx keeps upstream connections to the replicas it saw last; restart
	# it so every scenario starts with fresh ones.
	"${compose[@]}" restart lb >/dev/null 2>&1
	wait_for "load balancer" curl -fsS http://127.0.0.1:8080/healthz
}

reset_state() {
	wait_for redis "${compose[@]}" exec -T redis redis-cli ping
	"${compose[@]}" exec -T redis redis-cli FLUSHALL >/dev/null
	curl -fsS -X POST http://127.0.0.1:9101/ledger/reset
}

# run_limits <scenario> <algorithm> <store: redis|none> <outage: yes|no>
run_limits() {
	local scenario=$1 algorithm=$2 store=$3 outage=$4 url=redis://redis:6379/0
	[[ $store == none ]] && url=
	echo "q2: $scenario" >&2
	if ! start_fleet "$algorithm" "$url"; then
		echo "q2: $scenario skipped: the replicas did not start with algorithm $algorithm" >&2
		"${compose[@]}" logs --tail 5 switchboard-1 >&2 || true
		return 0
	fi
	reset_state
	local start end times=$tmp/$scenario.outage outage_flags=()
	start=$(now)
	if [[ $outage == yes ]]; then
		(
			sleep "$outage_at"
			echo "$(now)" >"$times"
			"${compose[@]}" kill redis >/dev/null 2>&1
			sleep "$outage_for"
			"${compose[@]}" start redis >/dev/null 2>&1
			echo "$(now)" >>"$times"
		) &
	fi
	bin/switchboard bench --url http://127.0.0.1:8080 --format openai --key sk-limits --model fake-limits \
		--rate "$rate" --duration "${duration}s" --warmup 0s --label "$scenario" --out "$tmp/$scenario.bench.json"
	end=$(now)
	if [[ $outage == yes ]]; then
		wait
		mapfile -t marks <"$times"
		outage_flags=(--outage-from "${marks[0]}" --outage-to "${marks[1]}")
	fi
	bin/switchboard eval limits --scenario "$scenario" --algorithm "$algorithm" --store "$store" \
		--model fake-limits --rate 100 --per 1m --burst 20 --start "$start" --end "$end" \
		${outage_flags[@]+"${outage_flags[@]}"} --bench "$tmp/$scenario.bench.json" \
		--commit "$commit" --command "$command" --replicas 3 --out "$out/$scenario.json"
}

run_budget() {
	echo "q2: budget" >&2
	start_fleet fixed_window redis://redis:6379/0
	reset_state
	bin/switchboard bench --url http://127.0.0.1:8080 --format openai --key sk-budget --model fake-budget \
		--rate "$budget_rate" --duration "${budget_duration}s" --warmup 0s --max-tokens 64 \
		--label budget --out "$tmp/budget.bench.json"
	# Settlement runs as each reply finishes; give the last ones a moment.
	sleep 1
	bin/switchboard eval budget --scenario budget --key budget --model fake-budget --limit 0.10 --period total \
		--input-price 3 --output-price 15 --redis redis://127.0.0.1:6379/0 \
		--metrics http://127.0.0.1:9091/metrics,http://127.0.0.1:9092/metrics,http://127.0.0.1:9093/metrics \
		--bench "$tmp/budget.bench.json" --commit "$commit" --command "$command" --replicas 3 --out "$out/budget.json"
}

go build -o bin/switchboard ./cmd/switchboard
"${compose[@]}" build switchboard-1 >/dev/null
"${compose[@]}" up -d redis fake-provider >/dev/null 2>&1
wait_for "fake provider" curl -fsS http://127.0.0.1:9101/healthz

mkdir -p "$out"
rm -f "$out"/*.json
for algorithm in $algorithms; do
	name=${algorithm//_/-}
	run_limits "$name" "$algorithm" redis no
	run_limits "$name-redis-outage" "$algorithm" redis yes
done
run_limits no-shared-store fixed_window none no
run_budget

echo "q2: results in $out. Run 'make results' to render RESULTS.md." >&2
