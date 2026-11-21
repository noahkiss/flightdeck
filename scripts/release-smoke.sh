#!/usr/bin/env bash
# Smoke-test a release build on the platform it was built for (docs/specs/ci-release.md 7.4).
#
#   scripts/release-smoke.sh <dir> <version>
#
# <dir> holds gff, gff-hab and gff-hab-holder (a Linux archive), or gff, GFF Hab.app and
# GFF Holder.app (a darwin archive). The checks:
#
#   - LICENSE and NOTICE sit in <dir> and, on macOS, in each bundle's Contents/Resources, and
#     NOTICE has an entry for libghostty, Ghostty and golang.org/x/sys
#   - capcom/ holds the Capcom engine: gff-hab-capcom.mjs, package.json, package-lock.json and
#     NOTICE, with the SDKs in the manifest and lockfile and in no NOTICE entry (never bundled);
#     the bundle parses (node --check) and carries <version>
#   - each binary's `version` prints "<name> <version>"
#   - gff-hab and gff carry the ghostty build tag (go version -m), so they parse with libghostty
#   - Linux: gff-hab links no libghostty and needs no glibc newer than 2.31; gff-hab-holder is
#     static. macOS: gff-hab and gff link only system libraries and name macOS 13.0 as the minimum
#   - a holder and gff-hab start in a temporary folder and `gff-hab api call hab.hello` answers
#
# Needs go (for go version -m) and node (for node --check) on PATH. Runs the binaries;
# touches nothing outside its own temporary folder, and stops only the two processes it started.
set -euo pipefail

say() { printf 'release-smoke: %s\n' "$*"; }
fail() {
	say "FAIL: $*" >&2
	exit 1
}

[ $# -eq 2 ] || { sed -n '4p' "$0" | sed 's/^# *//' >&2; exit 64; }
dir=$(cd "$1" && pwd)
want=$2
os=$(uname -s)

if [ "$os" = Darwin ]; then
	hab="$dir/GFF Hab.app/Contents/MacOS/gff-hab"
	holder="$dir/GFF Holder.app/Contents/MacOS/gff-hab-holder"
else
	hab=$dir/gff-hab
	holder=$dir/gff-hab-holder
fi
gff=$dir/gff
for b in "$hab" "$holder" "$gff"; do
	[ -x "$b" ] || fail "no executable $b"
done

# 0. The licence and the third-party notices (7.8): beside the binaries, and in each bundle.
notice_dirs=("$dir")
if [ "$os" = Darwin ]; then
	notice_dirs+=("$dir/GFF Hab.app/Contents/Resources" "$dir/GFF Holder.app/Contents/Resources")
fi
for d in "${notice_dirs[@]}"; do
	for f in LICENSE NOTICE; do
		[ -s "$d/$f" ] || fail "no $f in $d"
	done
	for c in go.mitchellh.com/libghostty Ghostty golang.org/x/sys; do
		grep -qx "$c" "$d/NOTICE" || fail "$d/NOTICE has no entry for $c"
	done
done
say "LICENSE and NOTICE are in place"

# 0b. The Capcom engine folder (7.2, capcom-service.md 3.1). The SDKs stay out of it: gff-hab
# install downloads them with npm ci, so the manifest and the lockfile must name them and the
# notices must not (a NOTICE entry would mean esbuild bundled one).
eng=$dir/capcom
for f in gff-hab-capcom.mjs package.json package-lock.json NOTICE; do
	[ -s "$eng/$f" ] || fail "no $f in $eng"
done
[ ! -e "$eng/node_modules" ] || fail "$eng carries node_modules; the SDKs must not ship"
for p in @anthropic-ai/claude-agent-sdk @openai/codex-sdk; do
	grep -q "\"$p\"" "$eng/package.json" || fail "$eng/package.json does not name $p"
	grep -q "\"node_modules/$p\"" "$eng/package-lock.json" || fail "$eng/package-lock.json does not pin $p"
	! grep -qx "$p" "$eng/NOTICE" || fail "$eng/NOTICE has an entry for $p: the SDK is bundled"
done
command -v node >/dev/null || fail "no node on PATH for node --check"
node --check "$eng/gff-hab-capcom.mjs" || fail "$eng/gff-hab-capcom.mjs does not parse"
grep -qF "\"$want\"" "$eng/gff-hab-capcom.mjs" || fail "$eng/gff-hab-capcom.mjs does not carry version $want"
say "the Capcom engine folder is in place"

# 1. The version each binary reports.
for b in "$gff" "$hab" "$holder"; do
	name=$(basename "$b")
	got=$("$b" version)
	[ "$got" = "$name $want" ] || fail "$name version printed '$got', want '$name $want'"
done
say "every binary reports $want"

# 2. The ghostty tag: without it gff-hab runs the test parser.
for b in "$hab" "$gff"; do
	go version -m "$b" | grep -Eq '^[[:space:]]+build[[:space:]]+-tags=(.*,)?ghostty(,.*)?$' ||
		fail "$(basename "$b") was built without -tags ghostty"
done
say "gff-hab and gff carry the ghostty tag"

# 3. What each binary links.
if [ "$os" = Darwin ]; then
	for b in "$hab" "$gff"; do
		libs=$(otool -L "$b" | tail -n +2 | awk '{print $1}' | grep -Ev '^(/usr/lib/|/System/Library/)' || true)
		[ -z "$libs" ] || fail "$(basename "$b") links libraries outside the system: ${libs//$'\n'/ }"
		minos=$(vtool -show-build "$b" | awk '$1 == "minos" {print $2; exit}')
		[ "$minos" = 13.0 ] || fail "$(basename "$b") names macOS $minos as its minimum, want 13.0"
	done
	say "gff-hab and gff link only system libraries, minimum macOS 13.0"
else
	if ldd "$hab" | grep -qi ghostty; then
		fail "gff-hab links libghostty dynamically"
	fi
	for b in "$hab" "$gff"; do
		newest=$(objdump -T "$b" | grep -o 'GLIBC_[0-9][0-9.]*' | sed 's/GLIBC_//' | sort -uV | tail -n 1)
		[ -n "$newest" ] || fail "$(basename "$b"): no glibc symbol versions found"
		[ "$(printf '%s\n2.31\n' "$newest" | sort -V | tail -n 1)" = 2.31 ] ||
			fail "$(basename "$b") needs glibc $newest, newer than the 2.31 floor"
	done
	file "$holder" | grep -q 'statically linked' || fail "gff-hab-holder is not statically linked"
	say "gff-hab links no libghostty, glibc floor 2.31 holds, gff-hab-holder is static"
fi

# 4. A holder and gff-hab start and answer.
tmp=$(mktemp -d "/tmp/gffsmoke.XXXXXX")
pids=()
cleanup() {
	for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
	for p in "${pids[@]}"; do wait "$p" 2>/dev/null || true; done
	rm -rf "$tmp"
}
trap cleanup EXIT
mkdir -m 700 "$tmp/holders" "$tmp/run"
"$holder" -dir "$tmp/holders" >"$tmp/holder.log" 2>&1 &
pids+=($!)
GFF_HAB_DETECT=off "$hab" serve -socket "$tmp/run/hab.sock" -hook-socket "$tmp/run/hook.sock" \
	-state "$tmp/state" -holders "$tmp/holders" -peers "$tmp/peers.toml" -config "$tmp/hab.toml" \
	-worktrees "$tmp/worktrees" -host-id smoke >"$tmp/hab.log" 2>&1 &
pids+=($!)
hello=
for _ in $(seq 1 100); do
	if [ -S "$tmp/run/hab.sock" ] && hello=$("$hab" api call -socket "$tmp/run/hab.sock" hab.hello 2>/dev/null); then
		break
	fi
	hello=
	sleep 0.2
done
if [ -z "$hello" ]; then
	sed 's/^/  holder: /' "$tmp/holder.log" >&2
	sed 's/^/  gff-hab: /' "$tmp/hab.log" >&2
	fail "gff-hab did not answer hab.hello within 20 s"
fi
say "gff-hab answers hab.hello"
say "ok"
