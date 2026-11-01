#!/bin/sh
# Archive FlightdeckiOS and upload it to App Store Connect for TestFlight.
# Needs Config/Local.xcconfig (Apple Distribution and the App Store profiles)
# and Config/Local.env (the API key; see Local.example.env).
#
#   apple/scripts/testflight.sh
#
# The build number is the UTC time to the minute, so every upload is higher
# than the last on any machine. MARKETING_VERSION stays in Names.xcconfig.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
scheme=FlightdeckiOS
die() { echo "testflight: $*" >&2; exit 1; }

[ -f "$here/Config/Local.xcconfig" ] ||
  die "no Config/Local.xcconfig; copy Config/Local.example.xcconfig and set the iOS signing"
grep -Eq '^[[:space:]]*CODE_SIGN_IDENTITY\[sdk=iphoneos\*\]' "$here/Config/Local.xcconfig" ||
  die "Config/Local.xcconfig sets no iOS signing identity; see Local.example.xcconfig step 2"
[ -f "$here/Config/Local.env" ] ||
  die "no Config/Local.env; copy Config/Local.example.env and fill in the API key"
. "$here/Config/Local.env"
: "${ASC_KEY_ID:?set ASC_KEY_ID in Config/Local.env}"
: "${ASC_ISSUER_CMD:?set ASC_ISSUER_CMD in Config/Local.env}"
: "${ASC_KEY_CMD:?set ASC_KEY_CMD in Config/Local.env}"

tmp=$(mktemp -d)
trap '/bin/rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
key="$tmp/AuthKey_$ASC_KEY_ID.p8"
(umask 077 && sh -c "$ASC_KEY_CMD" >"$key") || die "ASC_KEY_CMD failed"
chmod 600 "$key"
issuer=$(sh -c "$ASC_ISSUER_CMD") || die "ASC_ISSUER_CMD failed"
[ -s "$key" ] && [ -n "$issuer" ] || die "empty API key or issuer ID"

"$here/scripts/generate.sh"
build=$(date -u +%Y%m%d%H%M)
out="$here/build/ios"
archive="$out/$scheme.xcarchive"
mkdir -p "$out"
/bin/rm -rf "$archive" "$out/export"

echo "archiving $scheme, build $build"
if ! xcodebuild -project "$here/Flightdeck.xcodeproj" -scheme "$scheme" -configuration Release \
  -destination 'generic/platform=iOS' -derivedDataPath "$out" -archivePath "$archive" \
  CURRENT_PROJECT_VERSION="$build" archive >"$out/archive.log" 2>&1; then
  grep -E 'error' "$out/archive.log" | head -40 >&2
  die "archive failed; full log: $out/archive.log"
fi

# Export options: every signed bundle in the archive with the profile it embeds.
app=$(find "$archive/Products/Applications" -maxdepth 1 -name '*.app' | head -1)
plist="$out/ExportOptions.plist"
{
  cat <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>method</key><string>app-store-connect</string>
	<key>destination</key><string>upload</string>
	<key>signingStyle</key><string>manual</string>
	<key>signingCertificate</key><string>Apple Distribution</string>
	<key>uploadSymbols</key><true/>
	<key>manageAppVersionAndBuildNumber</key><false/>
	<key>provisioningProfiles</key>
	<dict>
PLIST
  for bundle in "$app" "$app"/PlugIns/*.appex; do
    [ -f "$bundle/embedded.mobileprovision" ] || continue
    id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$bundle/Info.plist")
    security cms -D -i "$bundle/embedded.mobileprovision" >"$tmp/profile.plist" 2>/dev/null
    name=$(/usr/libexec/PlistBuddy -c 'Print :Name' "$tmp/profile.plist")
    printf '\t\t<key>%s</key><string>%s</string>\n' "$id" "$name"
  done
  printf '\t</dict>\n</dict>\n</plist>\n'
} >"$plist"

echo "uploading build $build"
# /usr/bin first: a Homebrew rsync on PATH breaks the IPA step with "Copy failed".
if ! PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH" xcodebuild -exportArchive \
  -archivePath "$archive" -exportPath "$out/export" -exportOptionsPlist "$plist" \
  -authenticationKeyPath "$key" -authenticationKeyID "$ASC_KEY_ID" \
  -authenticationKeyIssuerID "$issuer" >"$out/upload.log" 2>&1; then
  grep -E 'error|Error' "$out/upload.log" | head -40 >&2
  die "upload failed; full log: $out/upload.log"
fi
grep -q 'Upload succeeded' "$out/upload.log" || die "no upload confirmation; see $out/upload.log"
echo "uploaded $scheme $(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Info.plist") ($build); App Store Connect is processing it"
