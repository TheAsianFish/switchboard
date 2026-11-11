#!/usr/bin/env bash
# Checks the `go install` path the README gives, in a clean golang container:
# no checkout, no module cache, nothing from this machine but a bare copy of
# the repository. Go fetches the module the way it would from GitHub (its
# direct VCS mode, which builds the module zip itself, so the embedded admin
# page and every file rule apply), from a bare clone that git's insteadOf
# puts in place of https://github.com/TheAsianFish/switchboard. The commit
# is tagged v0.0.0-installcheck in that copy only.
#
#   scripts/check-install.sh [ref]     (default HEAD; must be committed)
#
# Then the installed binary must report the tag as its version, serve the
# example config, answer a chat completion from the fake model and serve the
# admin page.
set -euo pipefail
cd "$(dirname "$0")/.."

REF=${1:-HEAD}
IMAGE=${GO_IMAGE:-golang:1.27}
MODULE=github.com/TheAsianFish/switchboard
TAG=v0.0.0-installcheck
commit=$(git rev-parse --verify "$REF^{commit}")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git clone -q --bare . "$work/switchboard.git"
git -C "$work/switchboard.git" tag -f "$TAG" "$commit" >/dev/null
cp switchboard.example.yaml "$work/switchboard.yaml"

echo "go install $MODULE/cmd/switchboard@$TAG (commit ${commit:0:12}) in $IMAGE"
docker run --rm -v "$work:/w:ro" -e GOPRIVATE="$MODULE" -e GOFLAGS= "$IMAGE" bash -euo pipefail -c '
	git config --global url."file:///w/switchboard.git".insteadOf "https://'"$MODULE"'"
	git config --global safe.directory "*"
	go install '"$MODULE"'/cmd/switchboard@'"$TAG"'
	got=$(switchboard version)
	echo "switchboard version: $got"
	test "$got" = '"$TAG"'
	switchboard serve --config /w/switchboard.yaml >/tmp/serve.log 2>&1 &
	for _ in $(seq 1 50); do curl -sf -o /dev/null http://127.0.0.1:9090/ && break; sleep 0.2; done
	reply=$(curl -sf http://127.0.0.1:8080/v1/chat/completions \
		-H "Authorization: Bearer sk-switchboard-dev" -H "Content-Type: application/json" \
		-d "{\"model\":\"fake\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}") \
		|| { cat /tmp/serve.log; exit 1; }
	echo "chat completion: $reply"
	echo "$reply" | grep -q "\"object\":\"chat.completion\""
	curl -sf http://127.0.0.1:9090/ | grep -q "<div id=\"root\">" && echo "admin page: served"
'
echo "install check passed"
