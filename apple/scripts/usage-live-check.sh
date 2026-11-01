#!/bin/sh
# Live check of usage in the app against a real gff-hab, all in a temporary directory that is
# removed on exit:
#   a gff-hab-holder and a gff-hab serve with their own socket and state,
#   a hab pane running a script that waits for a trigger file, then pipes a Claude Code
#   status line with the 5h window full into `gff-hab statusline claude`, and 8 s later one
#   with room,
#   then LiveTests.testLiveUsage: the warning, "alert me when free" at the reset time, the
#   alert at once on room, the widget snapshot.
# The status lines are hand-written; only their reset times follow the clock.
#
# Usage: usage-live-check.sh
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DIR=$(mktemp -d)
chmod 700 "$DIR"
PIDS=

cleanup() {
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$DIR"
}
trap cleanup EXIT INT TERM

echo "== build gff-hab and gff-hab-holder"
(cd "$REPO" && go build -o "$DIR/gff-hab" ./cmd/gff-hab && go build -o "$DIR/gff-hab-holder" ./cmd/gff-hab-holder)

echo "== holder and hab"
mkdir -m 700 "$DIR/run" "$DIR/state" "$DIR/holders"
"$DIR/gff-hab-holder" -dir "$DIR/holders" >"$DIR/holder.log" 2>&1 &
PIDS="$PIDS $!"
"$DIR/gff-hab" serve -socket "$DIR/run/hab.sock" -state "$DIR/state" -holders "$DIR/holders" -host-id usage-check >"$DIR/hab.log" 2>&1 &
PIDS="$PIDS $!"
i=0
while [ ! -S "$DIR/run/hab.sock" ]; do
	i=$((i + 1)); [ $i -gt 100 ] && { cat "$DIR/hab.log"; exit 1; }
	sleep 0.1
done

echo "== status lines"
now=$(date +%s)
five=$((now + 3600))
seven=$((now + 3 * 86400))
status() {
	# $1: the 5h window's used percentage.
	cat <<EOF
{"hook_event_name":"Status","session_id":"sess-usage-check","transcript_path":"$DIR/claude/projects/-work-app/sess-usage-check.jsonl","cwd":"/work/app","model":{"id":"model-x","display_name":"Model X"},"version":"2.1.316","rate_limits":{"five_hour":{"used_percentage":$1,"resets_at":$five},"seven_day":{"used_percentage":64.2,"resets_at":$seven}}}
EOF
}
status 100 >"$DIR/full.json"
status 30 >"$DIR/room.json"
cat >"$DIR/pane.sh" <<EOF
#!/bin/sh
while :; do
	if [ -e "$DIR/go" ]; then
		rm -f "$DIR/go"
		"$DIR/gff-hab" statusline claude <"$DIR/full.json"
		sleep 8
		"$DIR/gff-hab" statusline claude <"$DIR/room.json"
	fi
	sleep 1
done
EOF
chmod +x "$DIR/pane.sh"
"$DIR/gff-hab" api call -socket "$DIR/run/hab.sock" session.create "{\"argv\":[\"$DIR/pane.sh\"],\"name\":\"usage-check\"}" >/dev/null

echo "== LiveTests.testLiveUsage"
rc=0
cd "$REPO/apple/FlightdeckKit"
FLIGHTDECK_LIVE_SOCKET=$DIR/run/hab.sock \
FLIGHTDECK_LIVE_USAGE=$DIR/go \
	swift test --filter LiveTests.testLiveUsage >"$DIR/test.log" 2>&1 || rc=$?
grep -E 'error|passed|failed|skipped' "$DIR/test.log" | sed "s|$DIR|<tmp>|g" || true
if [ "$rc" -ne 0 ]; then
	echo "FAIL: swift test exited $rc" >&2
	exit "$rc"
fi
