#!/bin/sh
# Build the macOS app, Release, signed per
# Config/Local.xcconfig (ad hoc when there is none). The app lands in
# apple/build/Build/Products/Release/.
#
#   apple/scripts/build.sh                 # generate the project, then build
#   apple/scripts/build.sh --install       # also copy to /Applications and launch
#   apple/scripts/build.sh --install-user  # also copy to ~/Applications (made
#                                          # when missing) and launch
#
# The build number is the UTC time to the minute, so every build is higher
# than the last on any machine. MARKETING_VERSION stays in Names.xcconfig.
set -eu
case ${1:-} in
"") dest_dir= ;;
--install) dest_dir=/Applications ;;
--install-user) dest_dir=$HOME/Applications ;;
*) echo "usage: $0 [--install | --install-user]" >&2; exit 64 ;;
esac
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

if [ -n "$dest_dir" ]; then
  mkdir -p "$dest_dir"
  dest="$dest_dir/$(basename "$app")"
  osascript -e "quit app \"$(basename "$app" .app)\"" >/dev/null 2>&1 || true
  rm -rf "$dest"
  ditto "$app" "$dest"
  open "$dest"
  echo "installed: $dest"
fi
