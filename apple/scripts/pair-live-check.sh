#!/bin/sh
# Live check of QR pairing from the app's side (transport.md 6.1, app.md 18.8), all in a
# temporary directory that is removed on exit:
#   a gff-hab-holder and a gff-hab serve with their own socket and state,
#   a throwaway sshd with its own host key, reading the authorized_keys file in that
#   directory, on a high port,
#   a private `gff-hab pair -no-tty -listen 127.0.0.1` that writes to that file,
#   then LivePairTests: the Kit pairs with the window over swift-nio-ssh, shows the
#   four characters, which this script types at the window's prompt, stores the answer,
#   and runs one hab.ping over the throwaway sshd through the forced command.
# It never touches the system sshd, ~/.ssh or the installed hab.
#
# Env:   FLIGHTDECK_PAIR_LIVE_PORT   port for the sshd (default 2747)
set -eu

PORT=${FLIGHTDECK_PAIR_LIVE_PORT:-2747}
ADDR=127.0.0.1
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DIR=$(mktemp -d)
chmod 700 "$DIR"
PIDS=

cleanup() {
	exec 3>&- 2>/dev/null || true
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$DIR"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if nc -z "$ADDR" "$PORT" 2>/dev/null; then
	echo "port $PORT on $ADDR is in use; set FLIGHTDECK_PAIR_LIVE_PORT" >&2
	exit 1
fi

echo "== build gff-hab and gff-hab-holder"
(cd "$REPO" && go build -o "$DIR/gff-hab" ./cmd/gff-hab && go build -o "$DIR/gff-hab-holder" ./cmd/gff-hab-holder)

echo "== holder and hab"
mkdir -m 700 "$DIR/run" "$DIR/state" "$DIR/pairstate" "$DIR/holders" "$DIR/sshdkeys"
"$DIR/gff-hab-holder" -dir "$DIR/holders" >"$DIR/holder.log" 2>&1 &
PIDS="$PIDS $!"
"$DIR/gff-hab" serve -socket "$DIR/run/hab.sock" -state "$DIR/state" -holders "$DIR/holders" -host-id pair-live >"$DIR/hab.log" 2>&1 &
PIDS="$PIDS $!"
i=0
while [ ! -S "$DIR/run/hab.sock" ]; do
	i=$((i + 1)); [ $i -gt 100 ] && { cat "$DIR/hab.log"; exit 1; }
	sleep 0.1
done

echo "== throwaway sshd on $ADDR:$PORT"
ssh-keygen -q -t ed25519 -N '' -f "$DIR/sshdkeys/ssh_host_ed25519_key"
: >"$DIR/authorized_keys"
chmod 600 "$DIR/authorized_keys"
cat >"$DIR/sshd_config" <<EOF
Port $PORT
ListenAddress $ADDR
HostKey $DIR/sshdkeys/ssh_host_ed25519_key
AuthorizedKeysFile $DIR/authorized_keys
PidFile $DIR/sshd.pid
UsePAM no
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AllowTcpForwarding no
X11Forwarding no
LogLevel VERBOSE
EOF
/usr/sbin/sshd -D -e -f "$DIR/sshd_config" 2>"$DIR/sshd.log" &
PIDS="$PIDS $!"
i=0
until nc -z "$ADDR" "$PORT" 2>/dev/null; do
	i=$((i + 1)); [ $i -gt 100 ] && { cat "$DIR/sshd.log"; exit 1; }
	sleep 0.1
done

echo "== pairing window"
mkfifo "$DIR/pair.in"
"$DIR/gff-hab" pair -no-tty -link -listen "$ADDR" -timeout 2m -label live-box \
	-state "$DIR/pairstate" -authorized-keys "$DIR/authorized_keys" \
	-socket "$DIR/run/hab.sock" -ssh-port "$PORT" -sshd-keys "$DIR/sshdkeys" \
	<"$DIR/pair.in" >"$DIR/pair.out" 2>"$DIR/pair.err" &
PAIR=$!
PIDS="$PIDS $PAIR"
# Hold the window's stdin open.
exec 3>"$DIR/pair.in"
i=0
until grep -q '^flightdeck://pair?' "$DIR/pair.out" 2>/dev/null; do
	i=$((i + 1)); [ $i -gt 100 ] && { cat "$DIR/pair.out" "$DIR/pair.err"; exit 1; }
	sleep 0.1
done
LINK=$(grep '^flightdeck://pair?' "$DIR/pair.out" | head -1)

echo "== LivePairTests"
cd "$REPO/apple/FlightdeckKit"
FLIGHTDECK_LIVE_PAIR_LINK=$LINK \
FLIGHTDECK_LIVE_PAIR_CODE_FILE=$DIR/code \
FLIGHTDECK_LIVE_PAIR_SSHD_HOSTKEY=$DIR/sshdkeys/ssh_host_ed25519_key.pub \
FLIGHTDECK_LIVE_PAIR_SSHD_ENDPOINT=$ADDR:$PORT \
	swift test ${FLIGHTDECK_SWIFT_TEST_FLAGS:-} --filter LivePairTests >"$DIR/test.log" 2>&1 &
TEST=$!
PIDS="$PIDS $TEST"

# Type the four characters at the window's prompt once the app shows them.
i=0
until [ -s "$DIR/code" ] && grep -q 'Enter alone declines' "$DIR/pair.out"; do
	i=$((i + 1))
	if [ $i -gt 6000 ] || ! kill -0 "$TEST" 2>/dev/null; then break; fi
	sleep 0.1
done
if [ -s "$DIR/code" ]; then
	cat "$DIR/code" >&3
	echo >&3
fi

rc=0
wait "$TEST" || rc=$?
prc=0
wait "$PAIR" || prc=$?
grep -E 'live-pair:|error:|passed|failed|skipped' "$DIR/test.log" || true
echo "== window terminal"
sed "s|$DIR|<tmp>|g" "$DIR/pair.out" | grep -v '^flightdeck://pair?' | grep -vE '^[ ▀▄█]+$' || true
echo "== authorized_keys"
sed "s|$DIR|<tmp>|g" "$DIR/authorized_keys"
echo "== sshd log (auth lines)"
grep -E 'Accepted|Failed|refused' "$DIR/sshd.log" | sed "s|$DIR|<tmp>|g" | head -10 || true
if [ "$rc" -ne 0 ]; then
	echo "FAIL: swift test exited $rc" >&2
	cat "$DIR/pair.err" >&2
	exit "$rc"
fi
if [ "$prc" -ne 0 ]; then
	echo "FAIL: gff-hab pair exited $prc" >&2
	cat "$DIR/pair.err" >&2
	exit "$prc"
fi
grep -q 'flightdeck: ' "$DIR/authorized_keys" || { echo "FAIL: no key line written" >&2; exit 1; }
echo "PASS"
