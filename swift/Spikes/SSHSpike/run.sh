#!/bin/sh
# Run the SSH spike end to end in a temporary directory:
#   a throwaway sshd on a loopback port with its own host key and authorized_keys,
#   a gff-hab serve with a temporary socket and state, and sshspike against both.
# It never touches the system sshd config or ~/.ssh. Everything stops on exit.
#
# Usage: run.sh [se|soft]        (key kind, default se)
# Env:   SSHSPIKE_PORT           (loopback port for the throwaway sshd, default 2743)
#        SSHSPIKE_BURST          (extra channels for the session-limit probe, default 10)
set -eu

KIND=${1:-se}
PORT=${SSHSPIKE_PORT:-2743}
BURST=${SSHSPIKE_BURST:-10}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
DIR=$(mktemp -d)
chmod 700 "$DIR"
SSHD_PID=
HAB_PID=

cleanup() {
	[ -n "$SSHD_PID" ] && kill "$SSHD_PID" 2>/dev/null || true
	[ -n "$HAB_PID" ] && kill "$HAB_PID" 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "$DIR"
}
trap cleanup EXIT INT TERM

if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
	echo "port $PORT is in use; set SSHSPIKE_PORT" >&2
	exit 1
fi

echo "== build"
(cd "$REPO" && go build -o "$DIR/gff-hab" ./cmd/gff-hab)
(cd "$HERE" && swift build -q)
SPIKE="$HERE/.build/debug/sshspike"

echo "== gff-hab serve"
mkdir -m 700 "$DIR/run" "$DIR/state"
"$DIR/gff-hab" serve -socket "$DIR/run/hab.sock" -state "$DIR/state" >"$DIR/hab.log" 2>&1 &
HAB_PID=$!
i=0
while [ ! -S "$DIR/run/hab.sock" ]; do
	i=$((i + 1)); [ $i -gt 100 ] && { cat "$DIR/hab.log"; exit 1; }
	sleep 0.1
done

echo "== keys"
ssh-keygen -q -t ed25519 -N '' -f "$DIR/host_ed25519"
ssh-keygen -q -t ed25519 -N '' -f "$DIR/other_host_ed25519"
"$SPIKE" key --kind "$KIND" --out "$DIR/client.key" >"$DIR/authorized_keys"
chmod 600 "$DIR/authorized_keys"
cat "$DIR/authorized_keys"

echo "== throwaway sshd on 127.0.0.1:$PORT"
cat >"$DIR/sshd_config" <<EOF
Port $PORT
ListenAddress 127.0.0.1
HostKey $DIR/host_ed25519
AuthorizedKeysFile $DIR/authorized_keys
PidFile $DIR/sshd.pid
UsePAM no
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
LogLevel VERBOSE
EOF
/usr/sbin/sshd -D -e -f "$DIR/sshd_config" 2>"$DIR/sshd.log" &
SSHD_PID=$!
i=0
until nc -z 127.0.0.1 "$PORT" 2>/dev/null; do
	i=$((i + 1)); [ $i -gt 100 ] && { cat "$DIR/sshd.log"; exit 1; }
	sleep 0.1
done

CMD="$DIR/gff-hab connect -socket $DIR/run/hab.sock"

echo "== connect, pinned host key"
"$SPIKE" connect --port "$PORT" --key "$DIR/client.key" --host-key "$DIR/host_ed25519.pub" \
	--command "$CMD" --burst "$BURST"

echo "== second connect (warm)"
"$SPIKE" connect --port "$PORT" --key "$DIR/client.key" --host-key "$DIR/host_ed25519.pub" \
	--command "$CMD" | grep -E 'connect to first reply|handshake|round trip'

echo "== connect with a different pinned host key (must be refused)"
if "$SPIKE" connect --port "$PORT" --key "$DIR/client.key" --host-key "$DIR/other_host_ed25519.pub" \
	--command "$CMD"; then
	echo "FAIL: a changed host key was accepted" >&2
	exit 1
fi
echo "refused, as required"

echo "== sshd log (auth lines)"
grep -E 'Accepted|Failed|refused|MaxSessions|session' "$DIR/sshd.log" | sed "s|$DIR|<tmp>|g" | head -20
