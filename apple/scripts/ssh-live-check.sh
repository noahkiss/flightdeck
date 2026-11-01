#!/bin/sh
# Live check of the app's SSH carrier (swift-nio-ssh) against a real sshd, all in a
# temporary directory that is removed on exit:
#   a gff-hab-holder and a gff-hab serve with their own socket and state,
#   a throwaway sshd with its own host key and authorized_keys on a high port,
#   then SSHLiveTests (probe, pinning, one-shot ping, hello, subscribe, a data channel).
# It never touches the system sshd config, ~/.ssh or any firewall setting.
#
# Usage: ssh-live-check.sh            run the test against 127.0.0.1
#        ssh-live-check.sh serve      start everything and wait (Ctrl-C ends it), for a
#                                     device run: add the device's public key line with
#                                     FLIGHTDECK_SSH_LIVE_PUBKEY=<file>, or append it to the
#                                     authorized_keys file it prints
# Env:   FLIGHTDECK_SSH_LIVE_PORT     port for the sshd (default 2745)
#        FLIGHTDECK_SSH_LIVE_ADDR     address it binds (default 127.0.0.1); serve mode may name
#                                     one LAN address of this machine. Never 0.0.0.0
set -eu

MODE=${1:-test}
PORT=${FLIGHTDECK_SSH_LIVE_PORT:-2745}
ADDR=${FLIGHTDECK_SSH_LIVE_ADDR:-127.0.0.1}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
DIR=$(mktemp -d)
chmod 700 "$DIR"
PIDS=

case "$ADDR" in
0.0.0.0 | :: | "*") echo "bind one address, not all of them" >&2; exit 1 ;;
esac

cleanup() {
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	rm -rf "$DIR"
}
trap cleanup EXIT INT TERM

if nc -z "$ADDR" "$PORT" 2>/dev/null; then
	echo "port $PORT on $ADDR is in use; set FLIGHTDECK_SSH_LIVE_PORT" >&2
	exit 1
fi

echo "== build gff-hab and gff-hab-holder"
(cd "$REPO" && go build -o "$DIR/gff-hab" ./cmd/gff-hab && go build -o "$DIR/gff-hab-holder" ./cmd/gff-hab-holder)

echo "== holder and hab"
mkdir -m 700 "$DIR/run" "$DIR/state" "$DIR/holders"
"$DIR/gff-hab-holder" -dir "$DIR/holders" >"$DIR/holder.log" 2>&1 &
PIDS="$PIDS $!"
"$DIR/gff-hab" serve -socket "$DIR/run/hab.sock" -state "$DIR/state" -holders "$DIR/holders" -host-id live-check >"$DIR/hab.log" 2>&1 &
PIDS="$PIDS $!"
i=0
while [ ! -S "$DIR/run/hab.sock" ]; do
	i=$((i + 1)); [ $i -gt 100 ] && { cat "$DIR/hab.log"; exit 1; }
	sleep 0.1
done

echo "== throwaway sshd on $ADDR:$PORT"
ssh-keygen -q -t ed25519 -N '' -f "$DIR/host_ed25519"
: >"$DIR/authorized_keys"
if [ -n "${FLIGHTDECK_SSH_LIVE_PUBKEY:-}" ]; then
	cat "$FLIGHTDECK_SSH_LIVE_PUBKEY" >>"$DIR/authorized_keys"
fi
chmod 600 "$DIR/authorized_keys"
cat >"$DIR/sshd_config" <<EOF
Port $PORT
ListenAddress $ADDR
HostKey $DIR/host_ed25519
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

CMD="$DIR/gff-hab connect -socket $DIR/run/hab.sock"

if [ "$MODE" = serve ]; then
	echo "host key: $(ssh-keygen -lf "$DIR/host_ed25519.pub" | awk '{print $2}')"
	echo "address:  $(id -un)@$ADDR:$PORT"
	echo "command:  $CMD"
	echo "keys:     $DIR/authorized_keys (sshd reads it at each login; append a device's key line)"
	echo "Ctrl-C stops everything and removes $DIR."
	tail -f "$DIR/sshd.log" | grep --line-buffered -E 'Accepted|Failed|refused|Connection from' &
	PIDS="$PIDS $!"
	wait
	exit 0
fi

echo "== SSHLiveTests"
cd "$REPO/apple/FlightdeckKit"
FLIGHTDECK_LIVE_NIO_HOST=$ADDR \
FLIGHTDECK_LIVE_NIO_PORT=$PORT \
FLIGHTDECK_LIVE_NIO_HOSTKEY=$DIR/host_ed25519.pub \
FLIGHTDECK_LIVE_NIO_AUTHKEYS=$DIR/authorized_keys \
FLIGHTDECK_LIVE_NIO_COMMAND=$CMD \
FLIGHTDECK_LIVE_NIO_PANE='echo flightdeck-nio-live; sleep 30' \
	swift test --filter SSHLiveTests 2>&1 | grep -E 'live-nio:|error|passed|failed|skipped'

echo "== sshd log (auth lines)"
grep -E 'Accepted|Failed|refused' "$DIR/sshd.log" | sed "s|$DIR|<tmp>|g" | head -10
