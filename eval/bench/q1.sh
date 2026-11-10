#!/usr/bin/env bash
# Q1: what the gateway adds to a request, and which feature adds it.
#
# Part 1, passthrough: the same open-loop load directly against the fake
# provider and through a gateway with every feature off, per rate and mode.
# Part 2, breakdown: the gateway with features switched on cumulatively
# (none, + limits, + budget, + exact cache, + tracing, + semantic cache,
# + router), each against direct runs at the same rate. Tracing comes before
# the configs that embed so it runs at BREAKDOWN_RATE, where its cost (tens
# of microseconds) can be resolved; on top of an embedding at EMBED_RATE it
# would be lost in milliseconds of embedding variance. Every gateway run
# also records the gateway process's CPU and peak RSS.
#
# Raw files go to eval/results/q1-overhead/ (passthrough) and its
# breakdown/ subdirectory; `make results` renders them into RESULTS.md.
#
#   eval/bench/q1.sh                                  # the full run (make bench, about 26 minutes)
#   RATES=50 DURATION=3s WARMUP=1s eval/bench/q1.sh   # a quick smoke, every config
#   RATES= FEATURES='none exact' eval/bench/q1.sh     # two breakdown configs only
#
# Settings (environment):
#   RATES               passthrough rates, req/s (default "50 200 500"; empty skips)
#   DURATION, WARMUP    passthrough window and warm-up (default 60s, 5s)
#   FEATURES            breakdown configs to run (default all, in the order
#                       above; empty skips). A config always includes every
#                       feature before it, so "exact" means limits + budget
#                       + exact whichever others are picked.
#   BREAKDOWN_RATE      rate for configs that do not embed (default 200)
#   EMBED_RATE          rate for configs that embed: semantic, router
#                       (default 5; see below)
#   BREAKDOWN_DURATION, BREAKDOWN_WARMUP   breakdown window and warm-up
#                       (default 30s, 2s, or DURATION and WARMUP when set)
#   OLLAMA_URL          a running Ollama to embed with; otherwise q1.sh starts
#                       ollama/ollama in Docker (named volume switchboard-bench-ollama)
#
# Durations take a single unit: 500ms, 30s or 2m.
#
# Why two breakdown rates: the semantic cache and the router embed the
# prompt with nomic-embed-text on Ollama (one call per request with both on:
# the gateway memoizes the embedding of the last user turn per request), and
# on a 4-core CPU Ollama served 15 to 27 embeddings a second while the rest
# of the bench ran (q1.sh measures it before the
# breakdown and records it in meta.json). At 200 req/s the embedding queue
# grows without bound, the cache's 250 ms timeout fires and the gateway then
# bypasses the semantic layer for a while (most lookups skip it), so the run
# measures the timeouts and the bypass rather than the feature. The
# embedding configs therefore run at EMBED_RATE, well below that capacity,
# against direct runs at that same rate.
#
# Baseline: each breakdown rate gets two direct runs per mode, one before
# and one after its configs, and every config's overhead is taken against
# the two pooled. Pairing every config with its own adjacent direct run
# would double the direct runs and push `make bench` well past 25 minutes;
# the bracketing pair shows the baseline's run-to-run spread and drift over
# the group, which a single adjacent run would not, and the renderer reports
# that spread next to a bootstrap interval for each difference (breakdown
# runs keep their samples for it).
set -euo pipefail

cd "$(dirname "$0")/../.."

rates=${RATES-50 200 500}
duration=${DURATION:-60s}
warmup=${WARMUP:-5s}
all_configs=(none limits budget exact tracing semantic router)
features=${FEATURES-${all_configs[*]}}
breakdown_rate=${BREAKDOWN_RATE:-200}
embed_rate=${EMBED_RATE:-5}
breakdown_duration=${BREAKDOWN_DURATION:-${DURATION:-30s}}
breakdown_warmup=${BREAKDOWN_WARMUP:-${WARMUP:-2s}}

fake_latency=20ms
fake_tokens=30
fake_interval=5ms
# Ports clear of deploy/compose (8080, 9090-9095, 9101, 6379, 16686, 4318,
# 11434) and of the chaos fleet (28080, 29101, 29091-29093). Containers get
# random host ports.
fake_addr=127.0.0.1:19101
gateway_addr=127.0.0.1:18080
admin_addr=127.0.0.1:19090
bench_key=sk-bench
# Sent on every request, direct ones included, so every run sends the same
# bytes. The key's cache mode is the ceiling, so the directive turns the
# cache on only in the configs whose key has it on, and there it makes the
# request cacheable without a temperature.
cache_directive=semantic
embed_model=nomic-embed-text
redis_image=redis:8
jaeger_image=jaegertracing/jaeger:2.23.0
ollama_image=ollama/ollama:0.40.11
ollama_volume=switchboard-bench-ollama
# Embedder capacity probe: requests and concurrency.
probe_n=120
probe_par=16

out=eval/results/q1-overhead
bdir=$out/breakdown
command="RATES='$rates' DURATION=$duration WARMUP=$warmup FEATURES='$features' BREAKDOWN_RATE=$breakdown_rate EMBED_RATE=$embed_rate BREAKDOWN_DURATION=$breakdown_duration BREAKDOWN_WARMUP=$breakdown_warmup eval/bench/q1.sh"

for f in $features; do
	if [[ " ${all_configs[*]} " != *" $f "* ]]; then
		echo "q1: FEATURES: unknown config $f (want some of: ${all_configs[*]})" >&2
		exit 2
	fi
done

for addr in "$fake_addr" "$gateway_addr" "$admin_addr"; do
	if (exec 3<>"/dev/tcp/${addr%:*}/${addr##*:}") 2>/dev/null; then
		echo "q1: $addr is already in use; stop whatever listens there" >&2
		exit 1
	fi
done

logs=$(mktemp -d "${TMPDIR:-/tmp}/switchboard-q1.XXXXXX")
pids=()
containers=()
cleanup() {
	local status=$?
	for pid in ${pids[@]+"${pids[@]}"}; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in ${pids[@]+"${pids[@]}"}; do
		wait "$pid" 2>/dev/null || true
	done
	for c in ${containers[@]+"${containers[@]}"}; do
		docker stop "$c" >/dev/null 2>&1 || true
	done
	if [[ $status -ne 0 ]]; then
		echo "q1: failed; server logs in $logs" >&2
	fi
	exit "$status"
}
trap cleanup EXIT

log() { echo "q1: $*" >&2; }

wait_healthy() { # wait_healthy <name> <url>
	local i
	for i in $(seq 1 100); do
		if curl -fsS -o /dev/null "$2" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	log "$1 at $2 not healthy after 10 s"
	return 1
}

secs() { # secs <duration>: seconds, for sleep and arithmetic
	case $1 in
	*ms) awk -v v="${1%ms}" 'BEGIN { printf "%.3f", v / 1000 }' ;;
	*s) echo "${1%s}" ;;
	*m) awk -v v="${1%m}" 'BEGIN { printf "%.3f", v * 60 }' ;;
	*)
		log "cannot parse duration $1 (use ms, s or m)"
		return 1
		;;
	esac
}

now() { date +%s.%N; }

# cpu_ticks <pid>: utime + stime of a process in clock ticks. The command
# name in /proc/<pid>/stat is in parentheses and may hold spaces, so the
# fields are counted from after the last ')': utime and stime are fields
# 14 and 15, the 12th and 13th after it.
cpu_ticks() {
	local s
	s=$(<"/proc/$1/stat")
	s=${s##*) }
	local -a f
	read -r -a f <<<"$s"
	echo $((f[11] + f[12]))
}
clk_tck=$(getconf CLK_TCK)

peak_rss_kb() { awk '/^VmHWM:/ { print $2 }' "/proc/$1/status"; }

# container_cpu_usec <name>: CPU time a container has used, from its
# cgroup's cpu.stat (cgroup v2); empty if it cannot be read.
container_cpu_usec() {
	docker exec "$1" cat /sys/fs/cgroup/cpu.stat 2>/dev/null | awk '/^usage_usec/ { print $2 }' || true
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

# The cache configs need bench's -cache flag; without it no request is
# cacheable (the generator sends no temperature) and the cache stays out of
# the path, so those configs are skipped rather than measured wrong.
bench_cache=()
bench_help=$(bin/switchboard bench -help 2>&1 || true)
if grep -q -- '^  -cache ' <<<"$bench_help"; then
	bench_cache=(--cache "$cache_directive")
fi

# Which features are available, and why not.
declare -A unavailable=()
if [[ ${#bench_cache[@]} -eq 0 ]]; then
	for f in exact semantic; do
		unavailable[$f]="switchboard bench has no -cache flag, so no request would be cacheable"
	done
fi

selected() { [[ " $features " == *" $1 "* ]]; }

# features_of <config>: the features a config switches on: its own and
# every available one before it.
features_of() {
	local c list=()
	for c in "${all_configs[@]}"; do
		if [[ $c != none && -z ${unavailable[$c]:-} ]]; then
			list+=("$c")
		fi
		[[ $c == "$1" ]] && break
	done
	echo "${list[*]}"
}
has() { [[ " $1 " == *" $2 "* ]]; } # has <features> <feature>

# What the selected configs need.
need_redis=false need_ollama=false need_jaeger=false
for c in $features; do
	fs=$(features_of "$c")
	[[ -n $fs ]] && need_redis=true
	has "$fs" semantic || has "$fs" router && need_ollama=true
	has "$fs" tracing && need_jaeger=true
done

start_container() { # start_container <name> <docker run args...>
	local name=$1
	shift
	docker run -d --rm --name "$name" "$@" >/dev/null
	containers+=("$name")
}
host_port() { docker port "$1" "$2/tcp" | head -n1 | sed 's/.*://'; }

redis_name="" redis_url=""
if $need_redis; then
	redis_name=switchboard-q1-redis-$$
	start_container "$redis_name" -p 127.0.0.1::6379 "$redis_image"
	redis_url=redis://127.0.0.1:$(host_port "$redis_name" 6379)/0
	for _ in $(seq 1 50); do
		docker exec "$redis_name" redis-cli ping >/dev/null 2>&1 && break
		sleep 0.1
	done
fi

jaeger_name="" jaeger_otlp="" jaeger_query=""
if $need_jaeger; then
	jaeger_name=switchboard-q1-jaeger-$$
	start_container "$jaeger_name" -p 127.0.0.1::4318 -p 127.0.0.1::16686 "$jaeger_image"
	jaeger_otlp=http://127.0.0.1:$(host_port "$jaeger_name" 4318)
	jaeger_query=http://127.0.0.1:$(host_port "$jaeger_name" 16686)
	wait_healthy jaeger "$jaeger_query/"
fi

# Ollama: the one at $OLLAMA_URL if it answers, else a container. Any
# failure makes semantic and router unavailable, recorded in meta.json.
ollama_url="" ollama_name="" ollama_source="" ollama_version="" capacity=""
ollama_fail() {
	log "Ollama not available, skipping semantic and router: $1"
	unavailable[semantic]="Ollama not available: $1"
	unavailable[router]="Ollama not available: $1"
	ollama_url=""
}
if $need_ollama; then
	if [[ -n ${OLLAMA_URL:-} ]] && curl -fsS -m 3 -o /dev/null "$OLLAMA_URL/api/version" 2>/dev/null; then
		ollama_url=${OLLAMA_URL%/}
		ollama_source=local
	elif start_container "switchboard-q1-ollama-$$" -e OLLAMA_KEEP_ALIVE=-1 \
		-v "$ollama_volume:/root/.ollama" -p 127.0.0.1::11434 "$ollama_image" 2>"$logs/ollama-start.log"; then
		ollama_name=switchboard-q1-ollama-$$
		ollama_url=http://127.0.0.1:$(host_port "$ollama_name" 11434)
		ollama_source="docker $ollama_image"
		wait_healthy ollama "$ollama_url/api/version" || ollama_fail "container did not answer"
	else
		ollama_fail "could not start $ollama_image ($(tail -n1 "$logs/ollama-start.log"))"
	fi
fi
if [[ -n $ollama_url ]]; then
	ollama_version=$(curl -fsS "$ollama_url/api/version" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
	log "pulling $embed_model into Ollama ($ollama_source)"
	if ! curl -fsS -m 1200 -o "$logs/ollama-pull.log" "$ollama_url/api/pull" \
		-d "{\"model\":\"$embed_model\",\"stream\":false}"; then
		ollama_fail "ollama pull $embed_model failed"
	fi
fi
embed_once() { # embed_once <text>: loads the model if it was unloaded
	curl -fsS -m 60 -o /dev/null "$ollama_url/api/embed" -d "{\"model\":\"$embed_model\",\"input\":\"$1\",\"keep_alive\":\"30m\"}"
}
if [[ -n $ollama_url ]]; then
	if ! embed_once "q1 warm-up"; then
		ollama_fail "embedding with $embed_model failed"
	else
		# Sustained embedding throughput: probe_n short prompts, probe_par at
		# a time, the same shape as the gateway's calls.
		t0=$(now)
		seq 1 "$probe_n" | xargs -P "$probe_par" -I{} curl -fsS -m 60 -o /dev/null "$ollama_url/api/embed" \
			-d "{\"model\":\"$embed_model\",\"input\":\"q1 capacity probe {}: say something\",\"keep_alive\":\"30m\"}"
		t1=$(now)
		capacity=$(awk -v n="$probe_n" -v a="$t0" -v b="$t1" 'BEGIN { printf "%.1f", n / (b - a) }')
		log "embedder capacity: $capacity embeddings/s"
	fi
fi

# gateway_yaml <config> <features>: the gateway configuration for a config.
# Addresses of the throwaway containers are not in the file; the gateway
# gets them from SWITCHBOARD_* variables (see gateway_env).
gateway_yaml() {
	local name=$1 fs=$2 cache=off
	has "$fs" exact && cache=exact
	has "$fs" semantic && cache=semantic
	cat <<YAML
# Q1 gateway config "$name", generated by eval/bench/q1.sh.
# Features on: ${fs:-none}.
listen: $gateway_addr
admin_listen: $admin_addr
log:
  format: json
  level: info
providers:
  # The fake provider runs as its own process and is reached the way the
  # gateway reaches Ollama, through the OpenAI-compatible provider.
  - name: fake
    type: openaicompat
    base_url: http://$fake_addr/v1
    timeout: 30s
YAML
	if [[ -n $fs ]]; then
		cat <<YAML
redis:
  url: redis://127.0.0.1:6379/0   # replaced by SWITCHBOARD_REDIS_URL: q1.sh's throwaway $redis_image
YAML
	fi
	if has "$fs" semantic; then
		cat <<YAML
cache:
  semantic:
    embed_url: http://127.0.0.1:11434   # replaced by SWITCHBOARD_CACHE_EMBED_URL
    # model, threshold, index and match_numbers: the shipped defaults
YAML
	fi
	if has "$fs" router; then
		cat <<YAML
router:
  enabled: true
  embed_url: http://127.0.0.1:11434     # replaced by SWITCHBOARD_ROUTER_EMBED_URL
  # weights: the built-in ones, choosing among the qwen2.5 models below
YAML
	fi
	if has "$fs" tracing; then
		cat <<YAML
tracing:
  endpoint: http://127.0.0.1:4318       # replaced by SWITCHBOARD_TRACING_ENDPOINT: q1.sh's $jaeger_image
  sample_ratio: 1
YAML
	fi
	local price=""
	has "$fs" budget && price="    price: {input: 1, output: 1}   # a budget needs a priced model"
	echo "models:"
	local m
	for m in fake qwen2.5:0.5b qwen2.5:1.5b qwen2.5:3b; do
		if [[ $m != fake ]] && ! has "$fs" router; then
			continue
		fi
		[[ $m == qwen2.5:0.5b ]] && echo "  # The router's candidates, all served by the fake provider."
		echo "  - name: $m"
		echo "    provider: fake"
		[[ -n $price ]] && echo "$price"
	done
	cat <<YAML
keys:
  - name: bench
    key: $bench_key
YAML
	if has "$fs" limits; then
		cat <<YAML
    # High enough never to refuse at these rates; every request still
    # passes through both limiters in Redis. The gateway refuses a refill
    # faster than one unit per microsecond, and a request without
    # max_tokens is charged its prompt plus the model's 4096-token output
    # estimate, so the token limit is a million a second over an hour.
    limits:
      requests: {rate: 1000000, per: 1s, burst: 1000000}
      tokens: {rate: 3600000000, per: 1h, burst: 3600000000}
YAML
	fi
	if has "$fs" budget; then
		cat <<YAML
    # Large enough never to refuse; every request still reserves and settles.
    budget: {usd: "1000000", period: total}
YAML
	fi
	echo "    cache: \"$cache\""
}

# gateway_env <features>: the SWITCHBOARD_* overrides a config needs.
gateway_env() {
	local fs=$1
	[[ -n $fs ]] && echo "SWITCHBOARD_REDIS_URL=$redis_url"
	has "$fs" semantic && echo "SWITCHBOARD_CACHE_EMBED_URL=$ollama_url"
	has "$fs" router && echo "SWITCHBOARD_ROUTER_EMBED_URL=$ollama_url"
	has "$fs" tracing && echo "SWITCHBOARD_TRACING_ENDPOINT=$jaeger_otlp"
	return 0
}

config_rate() { # config_rate <features>
	if has "$1" semantic || has "$1" router; then
		echo "$embed_rate"
	else
		echo "$breakdown_rate"
	fi
}
config_model() { if has "$1" router; then echo auto; else echo fake; fi; }

# Decide each config's status now, before anything is measured.
declare -A status=() reason=()
for c in "${all_configs[@]}"; do
	if ! selected "$c"; then
		status[$c]="not selected"
		reason[$c]="not in FEATURES"
	elif [[ -n ${unavailable[$c]:-} ]]; then
		status[$c]=skipped
		reason[$c]=${unavailable[$c]}
	else
		status[$c]=measured
		reason[$c]=""
	fi
done

json_str() { # json_str <s>: s as a JSON string
	local s=${1//\\/\\\\}
	s=${s//\"/\\\"}
	printf '"%s"' "$s"
}
json_list() { # json_list <words>: a JSON array of strings
	local w first=1
	printf '['
	for w in $1; do
		((first)) || printf ', '
		json_str "$w"
		first=0
	done
	printf ']'
}

mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.txt
rm -rf "$bdir"
breakdown_selected=false
for c in $features; do
	[[ ${status[$c]} == measured ]] && breakdown_selected=true
done
$breakdown_selected && mkdir -p "$bdir"

{
	echo "{"
	echo "  \"commit\": \"$commit\","
	echo "  \"date\": \"$date\","
	echo "  \"fake_provider\": {"
	echo "    \"latency\": \"$fake_latency\","
	echo "    \"tokens\": $fake_tokens,"
	echo "    \"token_interval\": \"$fake_interval\""
	echo "  },"
	echo "  \"command\": $(json_str "$command"),"
	if [[ -z $features ]]; then
		echo "  \"breakdown\": null"
	else
		echo "  \"breakdown\": {"
		echo "    \"rate\": $breakdown_rate,"
		echo "    \"embed_rate\": $embed_rate,"
		echo "    \"duration\": \"$breakdown_duration\","
		echo "    \"warmup\": \"$breakdown_warmup\","
		echo "    \"cache_directive\": \"${bench_cache[1]:-}\","
		echo "    \"redis\": \"$($need_redis && echo "$redis_image")\","
		echo "    \"jaeger\": \"$($need_jaeger && echo "$jaeger_image")\","
		if [[ -n $ollama_url ]]; then
			echo "    \"ollama\": {"
			echo "      \"source\": $(json_str "$ollama_source"),"
			echo "      \"version\": $(json_str "$ollama_version"),"
			echo "      \"embed_model\": \"$embed_model\","
			echo "      \"capacity_rps\": ${capacity:-0},"
			echo "      \"capacity_requests\": $probe_n,"
			echo "      \"capacity_concurrency\": $probe_par"
			echo "    },"
		else
			echo "    \"ollama\": null,"
		fi
		echo "    \"configs\": ["
		first=1
		for c in "${all_configs[@]}"; do
			((first)) || echo ","
			first=0
			fs=$(features_of "$c")
			printf '      {"name": "%s", "features": %s, "rate": %s, "model": "%s", "status": "%s", "reason": %s}' \
				"$c" "$(json_list "$fs")" "$(config_rate "$fs")" "$(config_model "$fs")" "${status[$c]}" "$(json_str "${reason[$c]}")"
		done
		echo
		echo "    ]"
		echo "  }"
	fi
	echo "}"
} >"$out/meta.json"

bin/switchboard fake-provider --listen "$fake_addr" --latency "$fake_latency" \
	--tokens "$fake_tokens" --token-interval "$fake_interval" >"$logs/fake-provider.log" 2>&1 &
pids+=($!)
wait_healthy fake-provider "http://$fake_addr/healthz"

bench() { # bench <url> <model> <mode> <rate> <duration> <warmup> <out> [flags...]
	local url=$1 model=$2 mode=$3 rate=$4 dur=$5 warm=$6 file=$7 stream=()
	shift 7
	[[ $mode == stream ]] && stream=(--stream)
	bin/switchboard bench --url "$url" --format openai --model "$model" --key "$bench_key" \
		${stream[@]+"${stream[@]}"} ${bench_cache[@]+"${bench_cache[@]}"} --rate "$rate" \
		--duration "$dur" --warmup "$warm" --label "$(basename "$file" .json)" --out "$file" "$@"
}

# probe <model> <label> <dir>: one request before the load, recording what
# the gateway says it did: status, cache status, trace id, chosen model.
probe() {
	local model=$1 label=$2 dir=$3 code
	code=$(curl -sS -m 10 -o "$dir/probe.body" -D "$dir/probe.headers" -w '%{http_code}' \
		-H "Authorization: Bearer $bench_key" -H 'Content-Type: application/json' \
		-H "X-Switchboard-Cache: $cache_directive" \
		"http://$gateway_addr/v1/chat/completions" \
		-d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"q1 probe $label: say something\"}]}") || code=0
	probe_status=$code
	probe_cache=$(tr -d '\r' <"$dir/probe.headers" | awk -F': ' 'tolower($1) == "x-switchboard-cache-status" { print $2 }')
	probe_trace=$(tr -d '\r' <"$dir/probe.headers" | awk -F': ' 'tolower($1) == "x-trace-id" { print $2 }')
	probe_model=$(sed -n 's/.*"model": *"\([^"]*\)".*/\1/p' "$dir/probe.body" | head -n1)
}

# gateway_run <config> <features> <mode> <rate> <duration> <warmup> <out> [bench flags...]
# Starts a fresh gateway (so peak RSS is this run's and no cache entry or
# counter carries over; Redis is flushed first), probes it, runs the load,
# and writes <out> plus <out without .json>.gateway.json (process figures
# and probe) and .metrics.txt (the gateway's own counters afterwards).
gateway_run() {
	local name=$1 fs=$2 mode=$3 rate=$4 dur=$5 warm=$6 file=$7
	shift 7
	local base=${file%.json} yaml=$logs/$name.yaml model
	model=$(config_model "$fs")
	gateway_yaml "$name" "$fs" >"$yaml"
	[[ -n $redis_name ]] && docker exec "$redis_name" redis-cli FLUSHALL >/dev/null
	local -a env=(SWITCHBOARD_CONFIG="$yaml")
	local e
	while read -r e; do
		[[ -n $e ]] && env+=("$e")
	done < <(gateway_env "$fs")
	env "${env[@]}" bin/switchboard serve >>"$logs/gateway-$name.log" 2>&1 &
	local gw=$!
	pids+=("$gw")
	wait_healthy "gateway ($name)" "http://$gateway_addr/healthz"
	if has "$fs" semantic || has "$fs" router; then
		embed_once "q1 warm-up" # reload the model if Ollama unloaded it
	fi
	probe "$model" "$(basename "$base")" "$logs"

	bench "http://$gateway_addr" "$model" "$mode" "$rate" "$dur" "$warm" "$file" "$@" &
	local bp=$!
	# CPU over the measured window: from the end of the warm-up to the end
	# of the schedule. The run's tail (requests still in flight) is outside.
	sleep "$(secs "$warm")"
	local t0 c0 r0="" o0=""
	t0=$(now) c0=$(cpu_ticks "$gw")
	[[ -n $redis_name && -n $fs ]] && r0=$(container_cpu_usec "$redis_name")
	if [[ -n $ollama_name ]] && { has "$fs" semantic || has "$fs" router; }; then
		o0=$(container_cpu_usec "$ollama_name")
	fi
	sleep "$(secs "$dur")"
	local t1 c1 r1="" o1=""
	t1=$(now) c1=$(cpu_ticks "$gw")
	[[ -n $r0 ]] && r1=$(container_cpu_usec "$redis_name")
	[[ -n $o0 ]] && o1=$(container_cpu_usec "$ollama_name")
	wait "$bp"
	local rss
	rss=$(peak_rss_kb "$gw")
	curl -fsS "http://$admin_addr/metrics" | grep '^switchboard_' | grep -v '_bucket{' >"$base.metrics.txt" || true
	kill "$gw"
	wait "$gw" 2>/dev/null || true
	local trace_found=null
	if [[ -n $probe_trace && -n $jaeger_query ]] && has "$fs" tracing; then
		# The gateway flushes its spans on shutdown.
		trace_found=false
		for _ in 1 2 3 4 5; do
			if curl -fsS -o /dev/null "$jaeger_query/api/traces/$probe_trace" 2>/dev/null; then
				trace_found=true
				break
			fi
			sleep 1
		done
	fi
	local wall cpu redis_cpu=null ollama_cpu=null
	wall=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.3f", b - a }')
	cpu=$(awk -v a="$c0" -v b="$c1" -v t="$clk_tck" 'BEGIN { printf "%.2f", (b - a) / t }')
	[[ -n $r1 ]] && redis_cpu=$(awk -v a="$r0" -v b="$r1" 'BEGIN { printf "%.3f", (b - a) / 1e6 }')
	[[ -n $o1 ]] && ollama_cpu=$(awk -v a="$o0" -v b="$o1" 'BEGIN { printf "%.3f", (b - a) / 1e6 }')
	cat >"$base.gateway.json" <<JSON
{
  "config": "$name",
  "window_seconds": $wall,
  "gateway_cpu_seconds": $cpu,
  "gateway_peak_rss_kb": ${rss:-0},
  "redis_cpu_seconds": $redis_cpu,
  "ollama_cpu_seconds": $ollama_cpu,
  "probe": {
    "http_status": ${probe_status:-0},
    "cache_status": $(json_str "$probe_cache"),
    "trace_id": $(json_str "$probe_trace"),
    "trace_in_jaeger": $trace_found,
    "model": $(json_str "$probe_model")
  }
}
JSON
	log "$(basename "$base"): cpu $cpu s over $wall s, peak rss ${rss} kB, probe $probe_status cache=${probe_cache:--} model=${probe_model:--} trace=${probe_trace:+yes}"
}

# Part 1: passthrough.
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
				bench "http://$fake_addr" fake "$mode" "$rate" "$duration" "$warmup" "$out/direct-$mode-$rate.json"
			else
				gateway_run none "" "$mode" "$rate" "$duration" "$warmup" "$out/gateway-$mode-$rate.json"
			fi
		done
	done
	i=$((i + 1))
done

# Part 2: breakdown. Configs are grouped by rate (normally two groups); per
# mode, each group is a direct run, its configs in order, a direct run.
if $breakdown_selected; then
	for c in "${all_configs[@]}"; do
		[[ ${status[$c]} == measured ]] && gateway_yaml "$c" "$(features_of "$c")" >"$bdir/$c.yaml"
	done
	group_rates=()
	for c in "${all_configs[@]}"; do
		[[ ${status[$c]} == measured ]] || continue
		r=$(config_rate "$(features_of "$c")")
		[[ " ${group_rates[*]} " == *" $r "* ]] || group_rates+=("$r")
	done
	for mode in nonstream stream; do
		for r in "${group_rates[@]}"; do
			bench "http://$fake_addr" fake "$mode" "$r" "$breakdown_duration" "$breakdown_warmup" \
				"$bdir/direct-$mode-$r-1.json" --samples
			for c in "${all_configs[@]}"; do
				[[ ${status[$c]} == measured ]] || continue
				fs=$(features_of "$c")
				[[ $(config_rate "$fs") == "$r" ]] || continue
				gateway_run "$c" "$fs" "$mode" "$r" "$breakdown_duration" "$breakdown_warmup" \
					"$bdir/$c-$mode-$r.json" --samples
			done
			bench "http://$fake_addr" fake "$mode" "$r" "$breakdown_duration" "$breakdown_warmup" \
				"$bdir/direct-$mode-$r-2.json" --samples
		done
	done
fi

log "results in $out; server logs in $logs. Run 'make results' to render RESULTS.md."
