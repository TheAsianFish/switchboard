#!/usr/bin/env bash
# Sends one request through a running gateway, reads its X-Trace-Id, fetches
# the trace from Jaeger's HTTP API and prints the span tree with durations.
# Fails if a pipeline stage is missing from the trace. Needs curl and jq.
#
#   scripts/trace-check.sh                     # against `make dev` (Jaeger in compose)
#   scripts/trace-check.sh --stream            # the same, streamed
#   scripts/trace-check.sh --local --save DIR  # start the fake provider and a
#       gateway from this tree (eval/trace/switchboard.yaml) against a Jaeger
#       already listening on localhost:4318 and :16686, send one streamed and
#       one non-streamed request, and write both raw traces and meta.json to DIR
#   scripts/trace-check.sh --local --stall --save DIR
#                                              # the same with the fake provider
#       stalled and eval/trace/stall.yaml (a 300 ms first-token timeout, one
#       retry): one streamed request whose first attempt is held and timed
#       out while the stall is lifted behind it, so the retry answers; the
#       trace must show the timed-out attempt
#
# Environment: GATEWAY (http://localhost:8080), JAEGER (http://localhost:16686),
# KEY (sk-switchboard-dev), MODEL (fake), EXPECT (the span names that must be
# present).
set -euo pipefail

cd "$(dirname "$0")/.."

gateway=${GATEWAY:-http://localhost:8080}
jaeger=${JAEGER:-http://localhost:16686}
key=${KEY:-sk-switchboard-dev}
model=${MODEL:-fake}
expect=${EXPECT:-auth route cache.lookup provider settle}
local=false
stall=false
streams=(false)
save=""

while [[ $# -gt 0 ]]; do
	case $1 in
	--stream) streams=(true) ;;
	--local) local=true ;;
	--stall) stall=true ;;
	--save)
		save=$2
		shift
		;;
	*)
		echo "usage: $0 [--stream] [--local [--stall]] [--save DIR]" >&2
		exit 2
		;;
	esac
	shift
done

for tool in curl jq; do
	command -v "$tool" >/dev/null || {
		echo "trace-check: $tool is required" >&2
		exit 1
	}
done

work=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-trace.XXXXXX")
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
		echo "trace-check: failed; scratch files in $work" >&2
	else
		rm -rf "$work"
	fi
	exit "$status"
}
trap cleanup EXIT

wait_healthy() { # wait_healthy <name> <url>
	for _ in $(seq 1 100); do
		if curl -fsS -o /dev/null "$2/healthz" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	echo "trace-check: $1 at $2 not healthy after 10 s" >&2
	return 1
}

if $local; then
	curl -fsS -o /dev/null "$jaeger/" || {
		echo "trace-check: no Jaeger at $jaeger; start one, for example" >&2
		echo "  docker run -d --rm --name jaeger -p 127.0.0.1:4318:4318 -p 127.0.0.1:16686:16686 jaegertracing/jaeger:2.23.0" >&2
		exit 1
	}
	key=sk-trace
	gateway=http://127.0.0.1:8080
	streams=(true false)
	expect="auth route ratelimit budget.reserve cache.lookup provider provider.attempt settle cache.fill"
	config=eval/trace/switchboard.yaml
	fake_flags=()
	if $stall; then
		config=eval/trace/stall.yaml
		fake_flags=(--stalled)
		streams=(true)
	fi
	go build -o bin/switchboard ./cmd/switchboard
	bin/switchboard fake-provider --listen 127.0.0.1:9101 --latency 20ms --tokens 30 --token-interval 5ms \
		${fake_flags[@]+"${fake_flags[@]}"} >"$work/fake-provider.log" 2>&1 &
	pids+=($!)
	SWITCHBOARD_CONFIG=$config bin/switchboard serve >"$work/gateway.log" 2>&1 &
	pids+=($!)
	wait_healthy fake-provider http://127.0.0.1:9101
	wait_healthy gateway "$gateway"
fi

# fetch <trace id> <out file>: polls until every expected span is there and
# the span count has stopped growing (spans are exported in batches, and the
# cache fill ends after the response).
fetch() {
	local id=$1 out=$2 prev=-1 n
	for _ in $(seq 1 30); do
		if curl -fsS "$jaeger/api/traces/$id" -o "$out" 2>/dev/null; then
			n=$(jq '[.data[0].spans[]?] | length' "$out")
			if [[ $n -gt 0 && $n -eq $prev ]] && missing "$out" >/dev/null; then
				return 0
			fi
			prev=$n
		fi
		sleep 1
	done
	echo "trace-check: trace $id incomplete in Jaeger after 30 s" >&2
	return 1
}

# missing <trace file>: succeeds when every expected span name is present,
# printing the ones that are not otherwise.
missing() {
	local names gone=()
	names=$(jq -r '.data[0].spans[].operationName' "$1")
	for want in $expect; do
		grep -qxF "$want" <<<"$names" || gone+=("$want")
	done
	if [[ ${#gone[@]} -gt 0 ]]; then
		echo "${gone[*]}"
		return 1
	fi
}

# tree <trace file>: the spans as an indented tree, children in start order,
# with durations and the gateway's own attributes.
tree() {
	jq -r '
		.data[0].spans as $s
		| ([$s[].spanID]) as $ids
		| def parent: ([.references[]? | select(.refType == "CHILD_OF") | .spanID][0]) as $p
			| if $p != null and ($ids | index($p)) != null then $p else null end;
		  def attrs: [.tags[] | select(.key | startswith("switchboard.") or . == "http.status_code")
			| "\(.key | ltrimstr("switchboard."))=\(.value)"] | join(" ");
		  def show($d): "\([range($d)] | map("  ") | join(""))\(.operationName)  \(.duration / 1000) ms  \(attrs)",
			(.spanID as $id | [$s[] | select(parent == $id)] | sort_by(.startTime)[] | show($d + 1));
		  [$s[] | select(parent == null)] | sort_by(.startTime)[] | show(0)
	' "$1"
}

# hedged <trace.json>: succeeds when some attempt span is a hedge.
hedged() {
	jq -e '[.data[0].spans[] | select(.operationName == "provider.attempt")
		| .tags[] | select(.key == "switchboard.hedge" and .value == true)] | length > 0' "$1" >/dev/null
}

# timed_out <trace.json>: succeeds when some attempt span ended on the
# first-token timeout.
timed_out() {
	jq -e '[.data[0].spans[] | select(.operationName == "provider.attempt")
		| .tags[] | select(.key == "switchboard.outcome" and .value == "first_token_timeout")] | length > 0' "$1" >/dev/null
}

# Each request asks something nobody has asked before, so the cache misses
# and every stage runs.
status=0
for stream in "${streams[@]}"; do
	nonce=$(date +%s%N)
	body=$(jq -nc --arg m "$model" --arg p "Trace check $nonce: name three primary colours." --argjson s "$stream" \
		'{model: $m, temperature: 0, max_tokens: 32, stream: $s, messages: [{role: "user", content: $p}]}')
	if $stall; then
		# The first attempt reaches a stalled provider and is held; the
		# stall is lifted behind it, before its timer fires, so only the
		# retry is answered.
		(sleep 0.15 && curl -fsS -X POST http://127.0.0.1:9101/stall/off) &
		lifter=$!
	fi
	curl -fsS -o "$work/body" -D "$work/headers" -H "Authorization: Bearer $key" \
		-H "Content-Type: application/json" -d "$body" "$gateway/v1/chat/completions"
	if $stall; then
		wait "$lifter"
	fi
	id=$(awk 'tolower($1) == "x-trace-id:" { print $2 }' "$work/headers" | tr -d '\r')
	if [[ -z $id ]]; then
		echo "trace-check: no X-Trace-Id in the response; is tracing on (tracing.endpoint)?" >&2
		exit 1
	fi
	label=$([[ $stream == true ]] && echo stream || echo complete)
	echo "== $label: X-Trace-Id $id"
	fetch "$id" "$work/trace.json" || true
	tree "$work/trace.json"
	if absent=$(missing "$work/trace.json"); then
		echo "all expected stages present: $expect"
	else
		echo "trace-check: missing spans: $absent" >&2
		status=1
	fi
	if $stall; then
		if timed_out "$work/trace.json"; then
			echo "an attempt timed out waiting for its first token"
		else
			echo "trace-check: no provider.attempt with outcome=first_token_timeout" >&2
			status=1
		fi
		label=first-token-timeout
	elif hedged "$work/trace.json"; then
		echo "a hedge attempt is present"
	else
		echo "trace-check: no provider.attempt with hedge=true; the hedge is not in the trace" >&2
		status=1
	fi
	if [[ -n $save ]]; then
		mkdir -p "$save"
		jq . "$work/trace.json" >"$save/trace-$label.json"
	fi
done

if [[ -n $save ]]; then
	commit=$(git rev-parse --short HEAD)
	if ! git diff --quiet HEAD -- . ':(exclude)eval/results' ||
		[[ -n $(git ls-files --others --exclude-standard -- . ':(exclude)eval/results') ]]; then
		commit+=-dirty
	fi
	jaeger_image=${JAEGER_IMAGE:-jaegertracing/jaeger:2.23.0}
	cat >"$save/meta.json" <<JSON
{
  "commit": "$commit",
  "date": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "command": "scripts/trace-check.sh --local$($stall && echo " --stall") --save $save",
  "jaeger_image": "$jaeger_image",
  "config": "${config:-eval/trace/switchboard.yaml}",
  "fake_provider": {"latency": "20ms", "tokens": 30, "token_interval": "5ms", "stalled_at_start": $stall},
  "expected_spans": "$expect"
}
JSON
	echo "trace-check: raw traces and meta.json in $save" >&2
fi
exit "$status"
