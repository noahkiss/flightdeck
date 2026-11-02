#!/bin/sh
# Generate apple/Flightdeck.xcodeproj from apple/project.yml.
# Uses xcodegen from PATH when present; otherwise downloads the pinned release
# into apple/.build/ (ignored by git) and runs that.
set -eu
XCODEGEN_VERSION=2.46.0
here=$(cd "$(dirname "$0")/.." && pwd)
bin=$(command -v xcodegen || true)
if [ -z "$bin" ]; then
  bin="$here/.build/xcodegen-$XCODEGEN_VERSION/xcodegen/bin/xcodegen"
  if [ ! -x "$bin" ]; then
    dir="$here/.build/xcodegen-$XCODEGEN_VERSION"
    mkdir -p "$dir"
    curl -fsSL -o "$dir/xcodegen.zip" \
      "https://github.com/yonaskolb/XcodeGen/releases/download/$XCODEGEN_VERSION/xcodegen.zip"
    unzip -qo "$dir/xcodegen.zip" -d "$dir"
  fi
fi
"$bin" generate --spec "$here/project.yml" --project "$here"
# The project's only package is FlightdeckKit, so its pins are the project's:
# xcodebuild then resolves the same versions as `swift test`, and the CI cache
# key (the Kit's Package.resolved) stays true.
resolved="$here/Flightdeck.xcodeproj/project.xcworkspace/xcshareddata/swiftpm"
mkdir -p "$resolved"
cp "$here/FlightdeckKit/Package.resolved" "$resolved/Package.resolved"
