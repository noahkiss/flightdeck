#!/bin/sh
# Build the macOS app, Release, signed per
# Config/Local.xcconfig (ad hoc when there is none). The app lands in
# apple/build/Build/Products/Release/.
#
#   apple/scripts/build.sh            # generate the project, then build
#   apple/scripts/build.sh --install  # also copy to /Applications and launch
#
# The build number is the UTC time to the minute, so every build is higher
# than the last on any machine. MARKETING_VERSION stays in Names.xcconfig.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
"$here/scripts/generate.sh"
log="$here/build/xcodebuild.log"
mkdir -p "$here/build"
if ! xcodebuild -project "$here/Flightdeck.xcodeproj" -scheme FlightdeckMac -configuration Release \
  -derivedDataPath "$here/build" -destination 'generic/platform=macOS' \
  CURRENT_PROJECT_VERSION="$(date -u +%Y%m%d%H%M)" build >"$log" 2>&1; then
  grep -E 'error' "$log" | head -40 >&2
  echo "build failed; full log: $log" >&2
  exit 1
fi

app=$(find "$here/build/Build/Products/Release" -maxdepth 1 -name '*.app' | head -1)
[ -n "$app" ] || { echo "build failed: no .app" >&2; exit 1; }
codesign --verify --deep --strict "$app"
echo "built: $app"

if [ "${1:-}" = "--install" ]; then
  dest="/Applications/$(basename "$app")"
  osascript -e "quit app \"$(basename "$app" .app)\"" >/dev/null 2>&1 || true
  rm -rf "$dest"
  ditto "$app" "$dest"
  open "$dest"
fi
