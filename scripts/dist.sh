#!/usr/bin/env bash
# Release archives: switchboard for linux and darwin on amd64 and arm64, each
# a .tar.gz with the binary, LICENSE, README.md and switchboard.example.yaml,
# plus SHA256SUMS. Written to dist/ (or $DIST).
#
#   VERSION=v1.0.0 scripts/dist.sh
#
# VERSION defaults to `git describe`. The binaries are pure Go (CGO off), so
# every target cross-compiles from one machine. The archives are
# reproducible: the same commit, VERSION and Go toolchain give the same
# bytes (trimmed paths, no build id, no VCS stamp, so untracked files do
# not matter, fixed file times, owners and modes, so the umask does not
# matter, gzip without a name or time stamp). A different Go patch release
# gives a different binary. Needs GNU tar (gtar on macOS).
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
DIST=${DIST:-dist}
TARGETS=${TARGETS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"}
# File times in the archives: the commit's time, so a rebuild is identical.
EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}

TAR=${TAR:-tar}
if ! "$TAR" --version 2>/dev/null | grep -q 'GNU tar'; then
	if command -v gtar >/dev/null; then TAR=gtar; else
		echo "dist.sh: needs GNU tar (on macOS: brew install gnu-tar)" >&2
		exit 1
	fi
fi

rm -rf "$DIST"
mkdir -p "$DIST"
for target in $TARGETS; do
	os=${target%/*} arch=${target#*/}
	name="switchboard_${VERSION#v}_${os}_${arch}"
	stage="$DIST/$name"
	mkdir -p "$stage"
	echo "build $target"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
		-ldflags "-s -w -buildid= -X main.buildVersion=$VERSION" \
		-o "$stage/switchboard" ./cmd/switchboard
	cp LICENSE README.md switchboard.example.yaml "$stage/"
	"$TAR" --sort=name --mtime="@$EPOCH" --owner=0 --group=0 --numeric-owner \
		--mode=u+rwX,go=rX \
		-C "$DIST" -cf - "$name" | gzip -n -9 >"$DIST/$name.tar.gz"
	rm -rf "$stage"
done
(cd "$DIST" && sha256sum ./*.tar.gz | sed 's| \./| |' >SHA256SUMS)
cat "$DIST/SHA256SUMS"
