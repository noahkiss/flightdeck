#!/usr/bin/env bash
# Core tests 2 and 13 (docs/testing.md): the init system restarts the logic job, and no program
# in a pane notices; uninstall removes every file install wrote.
#
#   1. Build gff-hab and gff-hab-holder (build A) and run `gff-hab install` with its own job
#      prefix, a temporary state directory, holder directory and socket.
#   2. Start a long-running child in a pane through session.create. The child writes its pid and
#      a rising count, and prints a line every 0.1 s.
#   3. Restart the logic job GFF_SVC_LOOPS times (default 20): `systemctl --user restart` on
#      Linux, `launchctl kickstart -k` on macOS. After each restart: a new gff-hab answers, the
#      child has the same pid, its count still rises, and its pane's output offset still grows.
#   4. Build B, install it over A, and check again: the child stays on A's holder, B's gff-hab
#      starts a holder of its own build, and a new pane lands there.
#   5. Uninstall, and check that the child is gone, no job is loaded, no file install wrote is
#      left, and no process from the test runs.
#
# It is safe on a developer's machine: the job names carry a unique prefix, every path is in a
# temporary directory, and a failure uninstalls and removes all of it. Settings come from the
# environment:
#
#   GFF_SVC_LOOPS            restarts before the reinstall (default 20)
#   GFF_SVC_PREFIX           job name prefix (default gffsvctest-<pid>)
#   GFF_SVC_TMP              where the temporary directory goes (default /tmp; keep it short,
#                            macOS caps a socket path at 104 bytes)
#   GFF_TEST_TIMEOUT_SCALE   whole number that multiplies every wait bound (default 1)
#   GO                       the go command (default go)
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
loops=${GFF_SVC_LOOPS:-20}
prefix=${GFF_SVC_PREFIX:-gffsvctest-$$}
scale=${GFF_TEST_TIMEOUT_SCALE:-1}
gocmd=${GO:-go}
uid=$(id -u)

case $(uname -s) in
Linux)
	init=systemd
	unitdir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
	logic=$prefix-gff-hab.service
	;;
Darwin)
	init=launchd
	unitdir=$HOME/Library/LaunchAgents
	logic=$prefix.gff-hab
	;;
*)
	echo "service-check: no service install on $(uname -s)" >&2
	exit 2
	;;
esac

work=$(mktemp -d "${GFF_SVC_TMP:-/tmp}/gffsvc.XXXXXX")
state=$work/state
holders=$work/holders
sock=$work/hab.sock
passed=0
child=

say() { printf 'service-check: %s\n' "$*"; }
fail() {
	say "FAIL: $*"
	exit 1
}

# wait_until <what> <seconds> <command...>: poll the command until it succeeds, at most
# seconds × GFF_TEST_TIMEOUT_SCALE.
wait_until() {
	local what=$1 secs=$2
	shift 2
	local deadline=$(($(date +%s) + secs * scale))
	until "$@" >/dev/null 2>&1; do
		if [ "$(date +%s)" -ge "$deadline" ]; then
			fail "timed out after ${secs}s waiting for: $what"
		fi
		sleep 0.1
	done
}

hab_install() { "$1/gff-hab" install -prefix "$prefix" -state "$state" -holders "$holders" -socket "$sock"; }
hab_uninstall() { "$1/gff-hab" uninstall -yes -prefix "$prefix" -state "$state" -holders "$holders" -socket "$sock"; }
api() { "$work/a/gff-hab" api call -socket "$sock" -timeout 5s "$@"; }

welcome_field() { api hab.hello | sed -n "s/.*\"$1\":\"\\([^\"]*\\)\".*/\\1/p"; }

# pane_field <pane id> <field>: a field of the pane from structure.get. A pane object has no
# nested object, so splitting at '{' puts each pane on one line.
pane_field() {
	api structure.get | tr '{' '\n' | grep "\"id\":\"$1\"" |
		sed -n -e "s/.*\"$2\":\"\\([^\"]*\\)\".*/\\1/p" -e "s/.*\"$2\":\\([0-9][0-9]*\\).*/\\1/p"
}

new_epoch() { local e; e=$(welcome_field epoch) && [ -n "$e" ] && [ "$e" != "$1" ]; }
offset_above() { local o; o=$(pane_field "$1" offset) && [ -n "$o" ] && [ "$o" -gt "$2" ]; }
count_above() { local c; c=$(cat "$work/count" 2>/dev/null) && [ -n "$c" ] && [ "$c" -gt "$1" ]; }
gone() { ! kill -0 "$1" 2>/dev/null; }
holder_of_build() { grep -lE "\"build\": *\"$1\"" "$holders"/*.json; }
no_test_process() { ! pgrep -f "$work" >/dev/null; }

restart_logic() {
	if [ "$init" = systemd ]; then
		systemctl --user restart "$logic"
	else
		launchctl kickstart -k "gui/$uid/$logic"
	fi
}

# check_child <pane> <epoch before>: a new gff-hab answers; the child keeps its pid and runs on.
check_child() {
	local pane=$1 before=$2
	wait_until "a new gff-hab epoch" 30 new_epoch "$before"
	kill -0 "$child" 2>/dev/null || fail "child $child is gone"
	[ "$(cat "$work/child.pid")" = "$child" ] || fail "child pid changed: $(cat "$work/child.pid") != $child"
	local c
	c=$(cat "$work/count")
	wait_until "the child's count to rise past $c" 10 count_above "$c"
	local o
	o=$(pane_field "$pane" offset)
	[ -n "$o" ] || fail "pane $pane is not listed"
	wait_until "pane $pane's output offset to grow past $o" 10 offset_above "$pane" "$o"
}

cleanup() {
	local status=$?
	set +e
	if [ "$passed" != 1 ]; then
		say "cleaning up after a failure (status $status)"
		if [ "$init" = systemd ]; then
			journalctl --user --no-pager -n 60 -u "$prefix-*" 2>/dev/null
		else
			tail -n 60 "$state"/logs/*.log 2>/dev/null
		fi
		for b in "$work/b" "$work/a"; do
			if [ -x "$b/gff-hab" ]; then
				hab_uninstall "$b" && break
			fi
		done
		# What an uninstall that failed may have left.
		if [ "$init" = systemd ]; then
			systemctl --user disable --now "$logic" 2>/dev/null
			systemctl --user stop "$prefix-gff-hab-holder@*.service" 2>/dev/null
			rm -f "$unitdir/$logic" "$unitdir/$prefix-gff-hab-holder@.service" "$unitdir/default.target.wants/$logic"
			systemctl --user daemon-reload
			systemctl --user reset-failed "$prefix-*" 2>/dev/null
		else
			for p in "$unitdir/$prefix".gff-hab*.plist; do
				[ -e "$p" ] || continue
				launchctl bootout "gui/$uid/$(basename "$p" .plist)" 2>/dev/null
				rm -f "$p"
			done
		fi
		[ -n "$child" ] && kill "$child" 2>/dev/null
		pkill -f "$work" 2>/dev/null
	fi
	case $work in
	*/gffsvc.*) rm -rf "$work" ;;
	esac
	exit $status
}
trap cleanup EXIT
trap 'exit 130' INT TERM

say "prefix $prefix, work directory $work, $loops restarts"
for b in a b; do
	mkdir -p "$work/$b"
	(cd "$repo" && CGO_ENABLED=0 "$gocmd" build -ldflags "-X main.build=svc-$b-$$" -o "$work/$b/" \
		./cmd/gff-hab ./cmd/gff-hab-holder)
done

hab_install "$work/a"
wait_until "gff-hab to answer" 30 api hab.ping
epoch=$(welcome_field epoch)

cat >"$work/child.sh" <<'EOF'
echo $$ > "$1/child.pid"
i=0
while :; do
	i=$((i + 1))
	echo "tick $i"
	echo $i > "$1/count"
	sleep 0.1
done
EOF
created=$(api session.create "{\"argv\":[\"/bin/sh\",\"$work/child.sh\",\"$work\"]}")
pane=$(printf '%s' "$created" | sed -n 's/.*"pane":"\([^"]*\)".*/\1/p')
[ -n "$pane" ] || fail "session.create gave no pane: $created"
wait_until "the child's pid file" 10 test -s "$work/child.pid"
child=$(cat "$work/child.pid")
build_a=$(pane_field "$pane" holder_build)
[ "$build_a" = "svc-a-$$" ] || fail "pane on holder build '$build_a', want svc-a-$$"
say "child $child in pane $pane on holder build $build_a"

for i in $(seq 1 "$loops"); do
	epoch=$(welcome_field epoch)
	restart_logic
	check_child "$pane" "$epoch"
	say "restart $i/$loops: child $child alive, output advancing"
done

epoch=$(welcome_field epoch)
hab_install "$work/b"
check_child "$pane" "$epoch"
[ "$(welcome_field build)" = "svc-b-$$" ] || fail "gff-hab build after reinstall: $(welcome_field build)"
[ "$(pane_field "$pane" holder_build)" = "svc-a-$$" ] || fail "the child's pane moved holders"
wait_until "a holder of build B" 30 holder_of_build "svc-b-$$"
created=$(api session.create '{"argv":["/bin/sh","-c","echo second; sleep 600"]}')
pane2=$(printf '%s' "$created" | sed -n 's/.*"pane":"\([^"]*\)".*/\1/p')
[ "$(pane_field "$pane2" holder_build)" = "svc-b-$$" ] || fail "a new pane after reinstall is not on build B's holder"
say "reinstall: gff-hab build B runs, child $child still on build A's holder, new panes on build B"

hab_uninstall "$work/b"
wait_until "the child to end" 30 gone "$child"
wait_until "every process of the test to end" 30 no_test_process
if [ "$init" = systemd ]; then
	left=$(systemctl --user list-units --all --plain --no-legend "$prefix-*" 2>/dev/null)
	[ -z "$left" ] || fail "units still loaded: $left"
else
	launchctl list | grep -F "$prefix." && fail "jobs still loaded"
fi
for f in "$unitdir/$prefix"[.-]gff-hab* "$unitdir/default.target.wants/$logic" "$state/bin" "$state/logs"; do
	[ -e "$f" ] || [ -L "$f" ] && fail "uninstall left $f"
done
ls "$holders"/*.json >/dev/null 2>&1 && fail "holder registry files left: $(ls "$holders")"
passed=1
say "PASS: $loops restarts and one reinstall kept child $child; uninstall removed every installed file"
