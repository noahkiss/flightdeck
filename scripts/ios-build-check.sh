#!/bin/sh
# Build the shared Go client library (internal/merge and what it imports: hosts, habconn,
# control, api) for iOS as a C archive, to prove it builds there. macOS with Xcode only.
#
# Usage: scripts/ios-build-check.sh [-sim] [-o out.a]
#   -sim   build for the iOS simulator (arm64) instead of a device
#   -o     where to write the archive; default build/ios/libgffclient.a
#
# The archive is a build check, not a product: the app links no Go yet. A small main package
# with one exported function is written under build/ for the build and removed after.
set -eu

sdk=iphoneos
out=build/ios/libgffclient.a
while [ $# -gt 0 ]; do
	case $1 in
	-sim) sdk=iphonesimulator ;;
	-o) out=$2; shift ;;
	*) echo "usage: $0 [-sim] [-o out.a]" >&2; exit 2 ;;
	esac
	shift
done

[ "$(uname -s)" = Darwin ] || { echo "ios-build-check: needs macOS with Xcode" >&2; exit 2; }
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

sysroot=$(xcrun --sdk "$sdk" --show-sdk-path)
cc=$(xcrun --sdk "$sdk" --find clang)
min=17.0 # apple/project.yml deploymentTarget iOS
if [ "$sdk" = iphonesimulator ]; then
	target="-target arm64-apple-ios$min-simulator"
else
	target="-target arm64-apple-ios$min"
fi

probe=build/_iosprobe
mkdir -p "$probe" "$(dirname "$out")"
trap 'rm -rf "$root/$probe"' EXIT INT TERM
cat >"$probe/main.go" <<'EOF'
// Command _iosprobe exists only while scripts/ios-build-check.sh runs.
package main

import "C"

import (
	"github.com/noahkiss/flightdeck/internal/hosts"
	"github.com/noahkiss/flightdeck/internal/merge"
)

//export GffMergeHosts
func GffMergeHosts() C.int {
	return C.int(len(merge.Build([]hosts.Host{{Label: "probe", Status: hosts.Offline}}).Hosts))
}

func main() {}
EOF

GOOS=ios GOARCH=arm64 CGO_ENABLED=1 CC="$cc" \
	CGO_CFLAGS="-isysroot $sysroot $target" CGO_LDFLAGS="-isysroot $sysroot $target" \
	go build -buildmode=c-archive -trimpath -o "$out" "./$probe"

# The archive must hold arm64 objects for the chosen platform and export the probe symbol.
lipo -info "$out"
nm -gU "$out" 2>/dev/null | grep -q '_GffMergeHosts$' || { echo "ios-build-check: symbol missing" >&2; exit 1; }
plat=$(otool -l "$out" | awk '/LC_BUILD_VERSION/{f=1} f&&/platform/{print $2; exit}')
echo "ios-build-check: ok: $out ($sdk, platform $plat)"
