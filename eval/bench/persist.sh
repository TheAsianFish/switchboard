#!/usr/bin/env bash
# M9: what an append-only file adds to a request (`make bench-persist`,
# PLAN.md M9). Runs Q1's limits-and-budget configuration (the breakdown's
# "budget" config: both limiters and a budget reserve and settle on every
# request, through Redis) at 200 requests per second, streaming and not,
# against a fresh Redis per persistence mode, each mode with its own
# direct runs before and after as in Q1 (eval/bench/q1.sh does the work).
#
#   none      --save '' --appendonly no
#   everysec  --save '' --appendonly yes --appendfsync everysec
#   always    --save '' --appendonly yes --appendfsync always
#
# Raw files go to eval/results/m9-persist/bench/<mode>/; `make results`
# renders the added p50 and p99 per mode, and each mode against none.
# About 11 minutes.
#
#   eval/bench/persist.sh                         # make bench-persist
#   MODES='none always' BREAKDOWN_DURATION=5s eval/bench/persist.sh   # a quick smoke
set -euo pipefail

cd "$(dirname "$0")/../.."

modes=${MODES:-none everysec always}
root=${OUT_ROOT:-eval/results/m9-persist/bench}
for mode in $modes; do
	case $mode in
	none | everysec | always) ;;
	*)
		echo "persist: unknown mode $mode (want none, everysec, always)" >&2
		exit 2
		;;
	esac
done
for mode in $modes; do
	echo "persist: Redis append-only file $mode" >&2
	RATES= FEATURES=budget BREAKDOWN_RATE=200 REDIS_PERSIST=$mode OUT="$root/$mode" eval/bench/q1.sh
done
echo "persist: results in $root. Run 'make results' to render RESULTS.md." >&2
