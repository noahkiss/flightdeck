#!/usr/bin/env bash
# Wrap gff-hab-holder and gff-hab into minimal app bundles, sign them, notarize, staple and verify
# (docs/specs/macos-identity.md 3.3, 4.2, 5.1 to 5.3). Runs on macOS only.
#
#   scripts/macos-bundle.sh [-dev] [-bin-dir dir] [-speech-helper path] [-build id] [-version x.y.z] [-no-notarize] -o out
#
# Output, in <out>/gff-macos/:
#
#   GFF Holder.app   bundle id <prefix>.hab-holder, executable Contents/MacOS/gff-hab-holder
#   GFF Hab.app      bundle id <prefix>.hab, executable Contents/MacOS/gff-hab, and the speech
#                    helper Contents/MacOS/gff-hab-speech (identifier <prefix>.hab-speech) when
#                    there is one
#   gff              the bare client, identifier <prefix>.gff (only when cmd/gff exists)
#
# and <out>/gff-macos.zip, the stapled set for distribution.
#
# Release mode signs with a Developer ID identity, hardened runtime and a secure timestamp,
# submits one zip to the notary service, staples both bundles and runs every 5.3 check.
# -dev signs ad-hoc (or with $GFF_DEV_SIGN_IDENTITY) under the .dev bundle ids, named
# "GFF Holder Dev" and "GFF Hab Dev" (3.5), and never notarizes. A dev build holds privacy grants
# only until its next rebuild.
#
# The binaries come from -bin-dir (gff-hab and gff-hab-holder, and gff when present), else this
# script builds them for this Mac with libghostty (make libghostty, go build -tags ghostty).
# The speech helper comes from -speech-helper, else gff-hab-speech in -bin-dir, else (with no
# -bin-dir) build/gff-hab-speech when `make speech-helper` made it; with none, GFF Hab.app holds
# gff-hab alone. A helper that links a library outside the system is refused, or, in a dev build
# when it was not named, left out with a warning. The script never builds the helper: it needs
# sherpa-onnx and libopus.
#
# Settings come from the environment. Nothing names a person, a team or a key:
#
#   GFF_BUNDLE_PREFIX     reverse-DNS prefix of the bundle ids (required)
#   GFF_SIGN_IDENTITY     release: the signing identity, "Developer ID Application: ..." (required)
#   GFF_TEAM_ID           release: the team id the designated requirement must name (required)
#   GFF_NOTARY_PROFILE    release: the notarytool keychain profile (required unless -no-notarize)
#   GFF_SIGN_KEYCHAIN     optional: the keychain that holds the identity (codesign --keychain)
#   GFF_NOTARY_KEYCHAIN   optional: the keychain that holds the notary profile (notarytool --keychain);
#                         a CI job or an SSH session, where the login keychain is locked, needs it
#   GFF_DEV_SIGN_IDENTITY -dev: sign with this identity instead of ad-hoc
#   GO                    the go command (default go)
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
gocmd=${GO:-go}
dev=0
bindir=
speech=
build=
version=0.0.0
notarize=1
out=

usage() {
	sed -n '4p' "$0" | sed 's/^# *//' >&2
	exit 64
}
say() { printf 'macos-bundle: %s\n' "$*"; }
fail() {
	say "FAIL: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case $1 in
	-dev) dev=1 ;;
	-bin-dir) bindir=${2:?}; shift ;;
	-speech-helper) speech=${2:?}; shift ;;
	-build) build=${2:?}; shift ;;
	-version) version=${2:?}; shift ;;
	-no-notarize) notarize=0 ;;
	-o) out=${2:?}; shift ;;
	*) usage ;;
	esac
	shift
done
[ -n "$out" ] || usage
[ "$(uname -s)" = Darwin ] || fail "runs on macOS only"
prefix=${GFF_BUNDLE_PREFIX:-}
[ -n "$prefix" ] || fail "set GFF_BUNDLE_PREFIX to the reverse-DNS prefix of the bundle ids"
printf '%s' "$prefix" | grep -Eq '^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$' ||
	fail "GFF_BUNDLE_PREFIX '$prefix' is not reverse-DNS (letters, digits, '-', dots)"
printf '%s' "$version" | grep -Eq '^[0-9]+(\.[0-9]+){0,2}$' || fail "-version '$version': want x, x.y or x.y.z"

suffix=
label= # added to the name System Settings shows, so a dev row never reads as the release's
if [ "$dev" = 1 ]; then
	suffix=.dev
	label=" Dev"
	identity=${GFF_DEV_SIGN_IDENTITY:--}
	notarize=0
else
	identity=${GFF_SIGN_IDENTITY:-}
	team=${GFF_TEAM_ID:-}
	[ -n "$identity" ] || fail "set GFF_SIGN_IDENTITY (release), or pass -dev"
	[ -n "$team" ] || fail "set GFF_TEAM_ID (release), or pass -dev"
	if [ "$notarize" = 1 ] && [ -z "${GFF_NOTARY_PROFILE:-}" ]; then
		fail "set GFF_NOTARY_PROFILE (a notarytool keychain profile), or pass -no-notarize"
	fi
fi
holder_id=$prefix.hab-holder$suffix
hab_id=$prefix.hab$suffix
gff_id=$prefix.gff$suffix
speech_id=$prefix.hab-speech$suffix
[ -n "$build" ] || build=$(git -C "$repo" describe --always --dirty 2>/dev/null || echo dev)

mkdir -p "$out"
out=$(cd "$out" && pwd)
dist=$out/gff-macos
work=$(mktemp -d "${TMPDIR:-/tmp}/gffbundle.XXXXXX")
trap 'rm -rf "$work"' EXIT
rm -rf "$dist" "$out/gff-macos.zip"
mkdir -p "$dist"

# 1. The binaries.
own_build=0 # this script builds the binaries; only then does build/gff-hab-speech count
if [ -z "$bindir" ]; then
	own_build=1
	bindir=$work/bin
	mkdir -p "$bindir"
	make -C "$repo" libghostty >/dev/null
	pkgs="./cmd/gff-hab ./cmd/gff-hab-holder"
	[ -d "$repo/cmd/gff" ] && pkgs="$pkgs ./cmd/gff"
	say "building $pkgs (build $build, -tags ghostty)"
	# shellcheck disable=SC2086
	(cd "$repo" && PKG_CONFIG_PATH=$repo/build/libghostty-vt/share/pkgconfig CGO_ENABLED=1 \
		"$gocmd" build -tags ghostty -trimpath -ldflags "-X main.build=$build" -o "$bindir/" $pkgs)
fi
for b in gff-hab gff-hab-holder; do
	[ -x "$bindir/$b" ] || fail "no $b in $bindir"
done
speech_given=0
if [ -n "$speech" ]; then
	[ -f "$speech" ] && [ -x "$speech" ] || fail "-speech-helper $speech is not an executable file"
	speech_given=1
elif [ -x "$bindir/gff-hab-speech" ]; then
	speech=$bindir/gff-hab-speech
elif [ "$own_build" = 1 ] && [ -x "$repo/build/gff-hab-speech" ]; then
	speech=$repo/build/gff-hab-speech
fi
# Under the hardened runtime the helper may load only system libraries: library validation
# refuses a library signed by another team or ad hoc, so such a helper never starts, on this Mac
# or another. One that was named is an error; in a dev build one that was found is left out.
if [ -n "$speech" ]; then
	libs=$(otool -L "$speech" | tail -n +2 | awk '{print $1}' | grep -Ev '^(/usr/lib/|/System/Library/)' || true)
	if [ -n "$libs" ]; then
		why="$speech links libraries outside the system ($(echo $libs)), which the hardened runtime refuses; rebuild it with make speech-helper, which links libopus statically"
		if [ "$dev" = 1 ] && [ "$speech_given" = 0 ]; then
			say "warning: leaving the speech helper out: $why"
			speech=
		else
			fail "$why"
		fi
	fi
fi

# 2. The bundles (3.3): Info.plist, the executable and, in GFF Hab.app, the speech helper when
# there is one; nothing else.
plist_string() { printf '\t<key>%s</key>\n\t<string>%s</string>\n' "$1" "$2"; }

# write_bundle <name> <executable> <bundle id> <usage keys...>: usage strings come from usage().
usage_text() {
	case $1 in
	NSAppleEventsUsageDescription) echo "A program running in a Flightdeck pane wants to control another app." ;;
	NSLocalNetworkUsageDescription) echo "A program running in a Flightdeck pane wants to connect to devices on your local network." ;;
	NSMicrophoneUsageDescription) echo "A program running in a Flightdeck pane wants to use the microphone." ;;
	NSCameraUsageDescription) echo "A program running in a Flightdeck pane wants to use the camera." ;;
	NSContactsUsageDescription) echo "A program running in a Flightdeck pane wants to read your contacts." ;;
	NSCalendarsUsageDescription) echo "A program running in a Flightdeck pane wants to read your calendars." ;;
	NSRemindersUsageDescription) echo "A program running in a Flightdeck pane wants to read your reminders." ;;
	NSLocationUsageDescription) echo "A program running in a Flightdeck pane wants to know your location." ;;
	NSPhotoLibraryUsageDescription) echo "A program running in a Flightdeck pane wants to read your photo library." ;;
	*) fail "no usage text for $1" ;;
	esac
}

write_bundle() {
	local name=$1 exe=$2 id=$3
	shift 3
	local b=$dist/$name.app
	mkdir -p "$b/Contents/MacOS"
	cp "$bindir/$exe" "$b/Contents/MacOS/$exe"
	chmod 755 "$b/Contents/MacOS/$exe"
	{
		printf '<?xml version="1.0" encoding="UTF-8"?>\n'
		printf '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
		printf '<plist version="1.0">\n<dict>\n'
		plist_string CFBundleDevelopmentRegion en
		plist_string CFBundleExecutable "$exe"
		plist_string CFBundleIdentifier "$id"
		plist_string CFBundleInfoDictionaryVersion 6.0
		plist_string CFBundleName "$name$label"
		[ -n "$label" ] && plist_string CFBundleDisplayName "$name$label"
		plist_string CFBundlePackageType APPL
		plist_string CFBundleShortVersionString "$version"
		plist_string CFBundleVersion "$version"
		plist_string LSMinimumSystemVersion 13.0
		printf '\t<key>LSUIElement</key>\n\t<true/>\n'
		for k in "$@"; do
			plist_string "$k" "$(usage_text "$k")"
		done
		printf '</dict>\n</plist>\n'
	} >"$b/Contents/Info.plist"
	plutil -lint -s "$b/Contents/Info.plist" || fail "$name Info.plist does not lint"
}

write_bundle "GFF Holder" gff-hab-holder "$holder_id" \
	NSAppleEventsUsageDescription NSLocalNetworkUsageDescription NSMicrophoneUsageDescription \
	NSCameraUsageDescription NSContactsUsageDescription NSCalendarsUsageDescription \
	NSRemindersUsageDescription NSLocationUsageDescription NSPhotoLibraryUsageDescription
write_bundle "GFF Hab" gff-hab "$hab_id" NSLocalNetworkUsageDescription
# The speech helper sits beside gff-hab, where gff-hab looks for it (voice.md). It is nested
# code, signed before the bundle that seals it.
helper=
if [ -n "$speech" ]; then
	helper="$dist/GFF Hab.app/Contents/MacOS/gff-hab-speech"
	cp "$speech" "$helper"
	chmod 755 "$helper"
fi
has_gff=0
if [ -x "$bindir/gff" ]; then
	cp "$bindir/gff" "$dist/gff"
	has_gff=1
fi

# The holder's entitlements (4.2): what a terminal app carries, so a program in a pane can ask.
# Full Disk Access needs no entitlement. gff-hab, the speech helper and gff carry none.
cat >"$work/holder.entitlements" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>com.apple.security.automation.apple-events</key>
	<true/>
	<key>com.apple.security.device.audio-input</key>
	<true/>
	<key>com.apple.security.device.camera</key>
	<true/>
	<key>com.apple.security.personal-information.addressbook</key>
	<true/>
	<key>com.apple.security.personal-information.calendars</key>
	<true/>
	<key>com.apple.security.personal-information.location</key>
	<true/>
	<key>com.apple.security.personal-information.photos-library</key>
	<true/>
</dict>
</plist>
EOF
plutil -lint -s "$work/holder.entitlements" || fail "holder entitlements do not lint"

# 3. Sign inside out (5.1): the speech helper, each bundle, then the bare client. No --deep. The
# bundle identifier is the signing identifier.
sign_args=(--force --options runtime)
if [ "$identity" = - ]; then
	sign_args+=(--timestamp=none)
else
	sign_args+=(--timestamp)
fi
[ -n "${GFF_SIGN_KEYCHAIN:-}" ] && sign_args+=(--keychain "$GFF_SIGN_KEYCHAIN")
codesign "${sign_args[@]}" --entitlements "$work/holder.entitlements" --sign "$identity" "$dist/GFF Holder.app"
if [ -n "$helper" ]; then
	codesign "${sign_args[@]}" -i "$speech_id" --sign "$identity" "$helper"
fi
codesign "${sign_args[@]}" --sign "$identity" "$dist/GFF Hab.app"
if [ "$has_gff" = 1 ]; then
	codesign "${sign_args[@]}" -i "$gff_id" --sign "$identity" "$dist/gff"
fi
say "signed ($([ "$identity" = - ] && echo ad-hoc || echo "with the configured identity"))"

# 4. Notarize one zip, then staple each bundle (5.2). A bare binary cannot be stapled.
if [ "$notarize" = 1 ]; then
	zip=$work/notary.zip
	notary=(--keychain-profile "$GFF_NOTARY_PROFILE")
	[ -n "${GFF_NOTARY_KEYCHAIN:-}" ] && notary+=(--keychain "$GFF_NOTARY_KEYCHAIN")
	ditto -c -k --keepParent "$dist" "$zip"
	say "submitting to the notary service and waiting"
	xcrun notarytool submit "$zip" "${notary[@]}" --wait \
		--output-format json >"$work/notary.json" || true
	status=$(plutil -extract status raw -o - "$work/notary.json" 2>/dev/null || echo unknown)
	sub=$(plutil -extract id raw -o - "$work/notary.json" 2>/dev/null || echo)
	if [ "$status" != Accepted ]; then
		[ -n "$sub" ] && xcrun notarytool log "$sub" "${notary[@]}" >&2 || true
		fail "notarization status $status"
	fi
	say "notarization Accepted"
	xcrun stapler staple -q "$dist/GFF Holder.app"
	xcrun stapler staple -q "$dist/GFF Hab.app"
fi

# 5. Verify (5.3).
req_template() {
	printf 'identifier "%s" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */ and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */ and certificate leaf[subject.OU] = "%s"' "$1" "$team"
}

# norm: one line, no quotes, so codesign's quoting choice does not matter.
norm() { tr -d '"\n' | sed 's/^designated => //; s/^# designated => //'; }

verify() {
	local item=$1 id=$2 kind=$3 info
	codesign --verify --strict --verbose=2 "$item" >"$work/v.txt" 2>&1 || {
		cat "$work/v.txt" >&2
		fail "$item: codesign --verify --strict failed"
	}
	if grep -q 'does not satisfy its designated Requirement' "$work/v.txt"; then
		fail "$item: does not satisfy its designated requirement"
	fi
	info=$(codesign -dv --verbose=4 "$item" 2>&1)
	printf '%s\n' "$info" | grep -qx "Identifier=$id" || fail "$item: identifier is not $id"
	printf '%s\n' "$info" | grep -Eq '^CodeDirectory .*flags=0x[0-9a-f]*\(.*runtime.*\)' ||
		fail "$item: no hardened runtime flag"
	local dr
	dr=$(codesign -d -r- "$item" 2>/dev/null | grep 'designated =>' | norm)
	if [ "$dev" = 1 ]; then
		if [ "$identity" = - ]; then
			case $dr in cdhash*) ;; *) fail "$item: ad-hoc requirement is not a code hash: $dr" ;; esac
		fi
	else
		printf '%s\n' "$info" | grep -qx "TeamIdentifier=$team" || fail "$item: team is not the configured team"
		[ "$dr" = "$(req_template "$id" | norm)" ] ||
			fail "$item: designated requirement differs from the release template; every grant would be lost"
		codesign --verify -R="$(req_template "$id")" "$item" || fail "$item: does not satisfy the release requirement"
	fi
	if [ "$kind" = bundle ]; then
		if [ "$notarize" = 1 ]; then
			spctl --assess --type execute -vv "$item" >"$work/s.txt" 2>&1 || {
				cat "$work/s.txt" >&2
				fail "$item: Gatekeeper rejects it"
			}
			grep -q 'source=Notarized Developer ID' "$work/s.txt" || fail "$item: Gatekeeper source is not Notarized Developer ID"
			xcrun stapler validate -q "$item" || fail "$item: no stapled ticket"
		fi
	elif [ "$kind" = bare ] && [ "$notarize" = 1 ]; then
		# Printed, not gated (5.3): a bare binary carries no ticket. Nested code is assessed
		# with its bundle.
		spctl --assess --type install -vv "$item" 2>&1 | sed 's/^/  spctl: /' || true
	fi
	say "verified $(basename "$item") ($id)"
}

verify "$dist/GFF Holder.app" "$holder_id" bundle
verify "$dist/GFF Hab.app" "$hab_id" bundle
if [ -n "$helper" ]; then
	verify "$helper" "$speech_id" nested
	# The signed helper starts from the bundle: no library refused, no signature fault.
	"$helper" -info >"$work/speech.json" 2>&1 || {
		cat "$work/speech.json" >&2
		fail "the signed speech helper does not start (gff-hab-speech -info)"
	}
	say "speech helper starts from GFF Hab.app"
fi
[ "$has_gff" = 1 ] && verify "$dist/gff" "$gff_id" bare

ditto -c -k --keepParent "$dist" "$out/gff-macos.zip"
say "done: $dist and $out/gff-macos.zip"
