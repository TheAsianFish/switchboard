#!/usr/bin/env bash
# Runs a command with a throwaway redis:8 container, its URL exported as
# SWITCHBOARD_TEST_REDIS_URL. Used by `make test-integration` when no Redis
# is given; CI provides one as a service container instead.
#
#   scripts/with-redis.sh go test -tags integration ./...
set -euo pipefail

name=switchboard-test-redis-$$
docker run -d --rm --name "$name" -p 127.0.0.1::6379 redis:8 >/dev/null
trap 'docker stop "$name" >/dev/null 2>&1 || true' EXIT
port=$(docker port "$name" 6379/tcp | head -n1 | sed 's/.*://')
for _ in $(seq 1 50); do
	if docker exec "$name" redis-cli ping >/dev/null 2>&1; then
		break
	fi
	sleep 0.1
done
export SWITCHBOARD_TEST_REDIS_URL=redis://127.0.0.1:$port/0
"$@"
