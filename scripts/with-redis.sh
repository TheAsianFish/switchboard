#!/usr/bin/env bash
# Runs a command with two throwaway redis:8 containers. Used by
# `make test-integration` when no Redis is given, locally and in CI.
#
#   SWITCHBOARD_TEST_REDIS_URL          the server every integration test
#                                       shares (go test runs packages in
#                                       parallel against it)
#   SWITCHBOARD_TEST_REDIS_LOADING_URL  a server of its own for the LOADING
#                                       tests (internal/redistest), started
#                                       with --enable-debug-command yes:
#                                       they reload it with DEBUG RELOAD,
#                                       which refuses every client while it
#                                       runs, so it cannot be the shared one
#
#   scripts/with-redis.sh go test -tags integration ./...
set -euo pipefail

shared=switchboard-test-redis-$$
loading=switchboard-test-redis-loading-$$
trap 'docker stop "$shared" "$loading" >/dev/null 2>&1 || true' EXIT
docker run -d --rm --name "$shared" -p 127.0.0.1::6379 redis:8 >/dev/null
docker run -d --rm --name "$loading" -p 127.0.0.1::6379 redis:8 \
	redis-server --enable-debug-command yes >/dev/null

url() {
	local port
	port=$(docker port "$1" 6379/tcp | head -n1 | sed 's/.*://')
	echo "redis://127.0.0.1:$port/0"
}

for name in "$shared" "$loading"; do
	ok=
	for _ in $(seq 1 50); do
		if docker exec "$name" redis-cli ping >/dev/null 2>&1; then
			ok=1
			break
		fi
		sleep 0.1
	done
	if [ -z "$ok" ]; then
		echo "with-redis.sh: $name did not answer PING" >&2
		exit 1
	fi
done

SWITCHBOARD_TEST_REDIS_URL=$(url "$shared")
SWITCHBOARD_TEST_REDIS_LOADING_URL=$(url "$loading")
export SWITCHBOARD_TEST_REDIS_URL SWITCHBOARD_TEST_REDIS_LOADING_URL
"$@"
