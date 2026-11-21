#!/usr/bin/env bash
# Cross-platform build script for remotetool.
# Usage: ./build.sh [release|package]
#   default: build for the current platform into dist/
#   release: cross-compile windows/linux/android into dist/release/
#   package: release + one versioned zip per platform + SHA256SUMS
#            (needs zip/sha256sum; used by the CI release workflow)
set -e
cd "$(dirname "$0")"

VERSION=$(git describe --tags 2>/dev/null || echo "0.1.0")
LDFLAGS="-s -w -X main.version=$VERSION"

build() { # GOOS GOARCH outdir
	local GOOS=$1 GOARCH=$2 OUT=$3
	mkdir -p "$OUT"
	local EXT=""
	[ "$GOOS" = "windows" ] && EXT=".exe"
	echo "==> $GOOS/$GOARCH"
	GOOS=$GOOS GOARCH=$GOARCH CGO_ENABLED=0 \
		go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/rtd$EXT" ./cmd/rtd
	GOOS=$GOOS GOARCH=$GOARCH CGO_ENABLED=0 \
		go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/rt$EXT" ./cmd/rt
	GOOS=$GOOS GOARCH=$GOARCH CGO_ENABLED=0 \
		go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/hub$EXT" ./cmd/hub
}

package() { # GOOS GOARCH outdir
	local GOOS=$1 GOARCH=$2 DIR=$3
	local NAME="remotetool-$VERSION-$GOOS-$GOARCH"
	local STAGE="dist/release/.stage/$NAME"
	mkdir -p "$STAGE"
	cp "$DIR"/rtd* "$DIR"/rt* "$DIR"/hub* "$STAGE/"
	cp README.md LICENSE docs/getting-started.md "$STAGE/"
	(cd dist/release/.stage && zip -q -r "../$NAME.zip" "$NAME")
	rm -rf "$STAGE"
	echo "==> packaged $NAME.zip"
}

# All release targets: keep in sync with the platform matrix in docs/platform-matrix.md.
release_targets() {
	build windows amd64 dist/release/windows-amd64
	build linux amd64 dist/release/linux-amd64
	build linux arm64 dist/release/linux-arm64
	build android arm64 dist/release/android-arm64
}

if [ "$1" = "package" ]; then
	release_targets
	package windows amd64 dist/release/windows-amd64
	package linux amd64 dist/release/linux-amd64
	package linux arm64 dist/release/linux-arm64
	package android arm64 dist/release/android-arm64
	rmdir dist/release/.stage 2>/dev/null || true
	if command -v sha256sum >/dev/null 2>&1; then
		(cd dist/release && sha256sum ./*.zip > SHA256SUMS)
	else
		(cd dist/release && shasum -a 256 ./*.zip > SHA256SUMS)
	fi
	echo "done: dist/release/*.zip + SHA256SUMS"
elif [ "$1" = "release" ]; then
	release_targets
	echo "done: dist/release/*"
else
	build "$(go env GOOS)" "$(go env GOARCH)" dist
	echo "done: dist/"
fi
