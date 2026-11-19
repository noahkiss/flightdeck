#!/usr/bin/env bash
# Remove what development and test runs of this repo left on this machine (`make clean-dev`):
#
#   - build output: build/ (with the read-only zig package cache), the Swift packages' .build
#     folders, capcom-engine/dist and the binaries go build writes at the repo root;
#   - the service check's launch agents or systemd user units (gffsvctest-*), their processes
#     and work folders (gffsvc.XXXXXX), unless a service check runs now;
#   - the Go tests' leftover temporary folders and the processes started from them. A folder
#     with a testtmp pid file goes once its test binary has ended; one without goes after
#     GFF_CLEAN_AGE minutes (default 60) with no change, so a test that runs now keeps its own.
#
# It touches only this repo's own test prefixes (scripts/dev-sweep.sh), never an installed
# service. -n prints what it would do and changes nothing.
#
#   GFF_SVC_TMP     where the service check puts its work folders (default /tmp)
#   GFF_CLEAN_AGE   minutes before a Go test folder without a pid file counts as left (default 60)
set -euo pipefail

usage() { echo "usage: scripts/clean-dev.sh [-n]" >&2; }
DRY_RUN=0
while getopts n opt; do
	case $opt in
	n) DRY_RUN=1 ;;
	*)
		usage
		exit 2
		;;
	esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || {
	usage
	exit 2
}
export DRY_RUN

repo=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=scripts/dev-sweep.sh
. "$repo/scripts/dev-sweep.sh"

# Build output.
for p in build swift/HabAPI/.build apple/FlightdeckKit/.build capcom-engine/dist gff gff-hab gff-hab-holder; do
	[ -e "$repo/$p" ] || continue
	sweep_say "clean-dev: removing $p"
	if [ -d "$repo/$p" ]; then sweep_do chmod -R u+w "$repo/$p"; fi
	sweep_do rm -rf "${repo:?}/$p"
done

# The service check.
svc_tmp=${GFF_SVC_TMP:-/tmp}
if owner=$(svc_lock_owner "$svc_tmp"); then
	sweep_say "clean-dev: a service check (pid $owner) runs now; its jobs and folders stay"
else
	svc_sweep "$svc_tmp"
fi

# The Go tests: os.TempDir is $TMPDIR, else /tmp; some tests use /tmp for short socket paths.
tmps=$(printf '%s\n' "${TMPDIR:-/tmp}" /tmp | sed 's|/*$||' | sort -u)
for t in $tmps; do
	go_sweep_tmp "$t" "${GFF_CLEAN_AGE:-60}"
done
sweep_say "clean-dev: done"
