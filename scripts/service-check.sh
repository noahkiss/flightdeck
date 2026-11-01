#!/usr/bin/env bash
# Core tests 2 and 13 (docs/testing.md): the init system restarts the logic job, and no program
# in a pane notices; uninstall removes every file install wrote.
#
#   1. Build gff-hab and gff-hab-holder (build A; on macOS wrapped into dev helper bundles by
#      scripts/macos-bundle.sh -dev), list every path under the unit directory and the work
#      directory, and run `gff-hab install` with its own job prefix, a temporary state directory,
#      holder directory and socket. On macOS, LaunchServices must hold build A's bundles.
#   2. Start a long-running child in a pane through session.create. The child writes its pid, its
#      environment and a rising count, and prints a line every 0.1 s. Its environment has TERM,
#      COLORTERM and a locale, and none of the init system's job variables.
#   3. Restart the logic job GFF_SVC_LOOPS times (default 20): `systemctl --user restart` on
#      Linux, `launchctl kickstart -k` on macOS. After each restart: a new gff-hab answers, the
#      child has the same pid, its count still rises, its pane's output offset still grows, and
#      exactly one holder is registered.
#   4. Build B, install it over A, and check again: the child stays on A's holder, B's gff-hab
#      starts a holder of its own build, and a new pane lands there. On macOS each build runs
#      from its own bundle copy in <state>/builds, and A's stays while its holder runs.
#   5. End the child. A's holder, drained and empty, exits; its job goes (on macOS gff-hab boots
#      it out, deletes its plist, and unregisters and removes build A's bundle copies), and one
#      holder is left.
#   6. Uninstall, and check that the second pane's program is gone, no job is loaded (on macOS
#      `launchctl print gui/<uid>/<label>` for the logic job and every holder seen), no process
#      from the test runs, LaunchServices holds no build of the test (macOS), and the path list matches the one from step 1: nothing install wrote
#      is left, nothing else was removed, and only gff-hab's own state stays in the state
#      directory.
#
# It is safe on a developer's machine: the job names carry a unique prefix, every path is in a
# temporary directory, and a failure uninstalls and removes all of it. Settings come from the
# environment:
#
#   GFF_SVC_LOOPS            restarts before the reinstall (default 20)
#   GFF_SVC_PREFIX           job name prefix (default gffsvctest-<pid>)
#   GFF_SVC_TMP              where the temporary directory goes (default /tmp; keep it short,
#                            macOS caps a socket path at 104 bytes)
#   GFF_SVC_BUNDLE_PREFIX    macOS: bundle id prefix of the dev bundles (default test.gffsvc)
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
scratch=$work/t # the test's own files; left out of the path lists
passed=0
child=
child2=
seen=" " # holder instances seen, space separated

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

# hab_install <build dir>: on macOS the sources are the build's dev helper bundles.
hab_install() {
	local src=()
	if [ "$init" = launchd ]; then
		src=(-hab-bin "$1/gff-macos/GFF Hab.app" -holder-bin "$1/gff-macos/GFF Holder.app")
	fi
	"$1/gff-hab" install -prefix "$prefix" -state "$state" -holders "$holders" -socket "$sock" ${src[@]+"${src[@]}"}
}
hab_uninstall() { "$1/gff-hab" uninstall -yes -prefix "$prefix" -state "$state" -holders "$holders" -socket "$sock"; }
api() { "$work/a/gff-hab" api call -socket "$sock" -timeout 5s "$@"; }

welcome_field() { api hab.hello | sed -n "s/.*\"$1\":\"\\([^\"]*\\)\".*/\\1/p"; }

# pane_field <pane id> <field>: a field of the pane from structure.get. A pane object has no
# nested object, so splitting at '{' puts each pane on one line.
pane_field() {
	api structure.get | tr '{' '\n' | grep "\"id\":\"$1\"" |
		sed -n -e "s/.*\"$2\":\"\\([^\"]*\\)\".*/\\1/p" -e "s/.*\"$2\":\\([0-9][0-9]*\\).*/\\1/p"
}

# registered_builds: the build directories under <state>/builds that LaunchServices holds a
# record for (macOS). The database names a path by its real path (/tmp is /private/tmp).
lsregister=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
registered_builds() {
	local real
	real=$(cd "$state" && pwd -P)/builds/
	"$lsregister" -dump 2>/dev/null | sed -n "s|^path: *${real}\\([0-9a-f]\\{16\\}\\)/GFF Holder.app (.*|\\1|p" | sort -u
}

new_epoch() { local e; e=$(welcome_field epoch) && [ -n "$e" ] && [ "$e" != "$1" ]; }
offset_above() { local o; o=$(pane_field "$1" offset) && [ -n "$o" ] && [ "$o" -gt "$2" ]; }
count_above() { local c; c=$(cat "$scratch/count" 2>/dev/null) && [ -n "$c" ] && [ "$c" -gt "$1" ]; }
gone() { ! kill -0 "$1" 2>/dev/null; }
holder_of_build() { grep -lE "\"build\": *\"$1\"" "$holders"/*.json; }
no_test_process() { ! pgrep -f "$work" >/dev/null; }

# holder_count: the registered holders. holder_count_is <n>: exactly n.
holder_count() { find "$holders" -maxdepth 1 -name '*.json' | wc -l | tr -d ' '; }
holder_count_is() { [ "$(holder_count)" -eq "$1" ]; }

# note_holders: remember each registered holder instance, for the job checks after uninstall.
note_holders() {
	local f i
	for f in "$holders"/*.json; do
		[ -e "$f" ] || continue
		i=$(basename "$f" .json)
		case $seen in *" $i "*) ;; *) seen="$seen$i " ;; esac
	done
}

holder_job() {
	if [ "$init" = systemd ]; then
		printf '%s-gff-hab-holder@%s.service' "$prefix" "$1"
	else
		printf '%s.gff-hab-holder.%s' "$prefix" "$1"
	fi
}

# job_loaded <job>: the init system has the job. launchctl print names the GUI domain itself;
# `launchctl list` would list the caller's domain, which over SSH is not gui/<uid>.
job_loaded() {
	if [ "$init" = systemd ]; then
		[ -n "$(systemctl --user list-units --all --plain --no-legend "$1" 2>/dev/null)" ]
	else
		launchctl print "gui/$uid/$1" >/dev/null 2>&1
	fi
}

# holder_job_gone <instance>: the holder's job is no longer loaded or running, and on macOS its
# plist is deleted.
holder_job_gone() {
	local j
	j=$(holder_job "$1")
	if [ "$init" = systemd ]; then
		! systemctl --user is-active --quiet "$j"
	else
		! job_loaded "$j" && [ ! -e "$unitdir/$j.plist" ]
	fi
}

# paths: every path under the unit directory and the work directory, except the test's own
# files, sorted.
paths() {
	{
		[ -d "$unitdir" ] && find "$unitdir"
		find "$work" -path "$scratch" -prune -o -print
	} | LC_ALL=C sort
}

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
	[ "$(cat "$scratch/child.pid")" = "$child" ] || fail "child pid changed: $(cat "$scratch/child.pid") != $child"
	local c
	c=$(cat "$scratch/count")
	wait_until "the child's count to rise past $c" 10 count_above "$c"
	local o
	o=$(pane_field "$pane" offset)
	[ -n "$o" ] || fail "pane $pane is not listed"
	wait_until "pane $pane's output offset to grow past $o" 10 offset_above "$pane" "$o"
}

# check_env: the child got a terminal's environment, the locale, and none of the job's variables.
check_env() {
	local e=$scratch/child.env
	grep -qx 'TERM=xterm-256color' "$e" || fail "child TERM: $(grep '^TERM=' "$e" || echo none)"
	grep -qx 'COLORTERM=truecolor' "$e" || fail "child COLORTERM: $(grep '^COLORTERM=' "$e" || echo none)"
	grep -qE '^(LANG|LC_ALL|LC_CTYPE)=.' "$e" || fail "child has no locale"
	if [ -n "${LANG:-}" ]; then
		grep -qx "LANG=$LANG" "$e" || fail "child LANG $(grep '^LANG=' "$e" || echo none), want $LANG from install"
	fi
	if grep -E '^(INVOCATION_ID|JOURNAL_STREAM|XPC_SERVICE_NAME)=' "$e"; then
		fail "child has the init system's job variables"
	fi
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
		[ -n "$child2" ] && kill "$child2" 2>/dev/null
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
	if [ "$init" = launchd ]; then
		GFF_BUNDLE_PREFIX=${GFF_SVC_BUNDLE_PREFIX:-test.gffsvc} GFF_DEV_SIGN_IDENTITY='' \
			"$repo/scripts/macos-bundle.sh" -dev -bin-dir "$work/$b" -o "$work/$b" >/dev/null
	fi
done
mkdir -p "$scratch"
mkdir -m 700 "$holders" # gff-hab makes it at its first holder; made here, it is in the path list
cat >"$scratch/child.sh" <<'EOF'
echo $$ > "$1/child.pid"
env > "$1/child.env"
i=0
while :; do
	i=$((i + 1))
	echo "tick $i"
	echo $i > "$1/count"
	sleep 0.1
done
EOF
cat >"$scratch/child2.sh" <<'EOF'
echo $$ > "$1/child2.pid"
echo second
while :; do sleep 1; done
EOF
before=$(paths)

hab_install "$work/a"
wait_until "gff-hab to answer" 30 api hab.ping
epoch=$(welcome_field epoch)
if [ "$init" = launchd ]; then
	dir_a=$(ls "$state/builds")
	[ "$(registered_builds)" = "$dir_a" ] || fail "LaunchServices holds builds '$(registered_builds)', want build A's $dir_a"
	say "install registered build A's bundles with LaunchServices"
fi

created=$(api session.create "{\"argv\":[\"/bin/sh\",\"$scratch/child.sh\",\"$scratch\"]}")
pane=$(printf '%s' "$created" | sed -n 's/.*"pane":"\([^"]*\)".*/\1/p')
[ -n "$pane" ] || fail "session.create gave no pane: $created"
wait_until "the child's pid file" 10 test -s "$scratch/child.pid"
wait_until "the child's environment file" 10 test -s "$scratch/child.env"
child=$(cat "$scratch/child.pid")
build_a=$(pane_field "$pane" holder_build)
[ "$build_a" = "svc-a-$$" ] || fail "pane on holder build '$build_a', want svc-a-$$"
check_env
holder_count_is 1 || fail "$(holder_count) holders registered, want 1"
note_holders
inst_a=$(basename "$(holder_of_build "svc-a-$$")" .json)
say "child $child in pane $pane on holder $inst_a, build $build_a; TERM, COLORTERM and locale set"

for i in $(seq 1 "$loops"); do
	epoch=$(welcome_field epoch)
	restart_logic
	check_child "$pane" "$epoch"
	note_holders
	holder_count_is 1 || fail "restart $i: $(holder_count) holders registered, want 1: $(ls "$holders")"
	say "restart $i/$loops: child $child alive, output advancing, one holder"
done

epoch=$(welcome_field epoch)
hab_install "$work/b"
check_child "$pane" "$epoch"
[ "$(welcome_field build)" = "svc-b-$$" ] || fail "gff-hab build after reinstall: $(welcome_field build)"
[ "$(pane_field "$pane" holder_build)" = "svc-a-$$" ] || fail "the child's pane moved holders"
wait_until "a holder of build B" 30 holder_of_build "svc-b-$$"
note_holders
created=$(api session.create "{\"argv\":[\"/bin/sh\",\"$scratch/child2.sh\",\"$scratch\"]}")
pane2=$(printf '%s' "$created" | sed -n 's/.*"pane":"\([^"]*\)".*/\1/p')
[ -n "$pane2" ] || fail "session.create gave no pane: $created"
[ "$(pane_field "$pane2" holder_build)" = "svc-b-$$" ] || fail "a new pane after reinstall is not on build B's holder"
wait_until "the second child's pid file" 10 test -s "$scratch/child2.pid"
child2=$(cat "$scratch/child2.pid")
say "reinstall: gff-hab build B runs, child $child still on build A's holder, new panes on build B"

# Retire: with its last program gone, the drained holder A exits, and its job goes.
kill "$child"
wait_until "the child to end" 10 gone "$child"
wait_until "holder $inst_a to unregister" 30 holder_count_is 1
holder_of_build "svc-a-$$" >/dev/null 2>&1 && fail "holder $inst_a is still registered"
wait_until "holder $inst_a's job to go" 90 holder_job_gone "$inst_a"
if [ "$init" = launchd ]; then
	n=$(find "$state/builds" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')
	[ "$n" = 1 ] || fail "$n build directories after build A's holder retired, want 1: $(ls "$state/builds")"
	[ -x "$state/current/GFF Hab.app/Contents/MacOS/gff-hab" ] || fail "current does not name build B's bundles"
	dir_b=$(ls "$state/builds")
	[ "$(registered_builds)" = "$dir_b" ] ||
		fail "LaunchServices holds builds '$(registered_builds)' after build A went, want only build B's $dir_b"
fi
say "retire: holder $inst_a exited after its last program, and its job is gone"

hab_uninstall "$work/b"
wait_until "the second child to end" 30 gone "$child2"
wait_until "every process of the test to end" 30 no_test_process
note_holders
for j in "$logic" $(for i in $seen; do holder_job "$i"; echo; done); do
	job_loaded "$j" && fail "job $j is still loaded"
done
if [ "$init" = systemd ]; then
	left=$(systemctl --user list-units --all --plain --no-legend "$prefix-*" 2>/dev/null)
	[ -z "$left" ] || fail "units still loaded: $left"
fi
after=$(paths)
removed=$(comm -23 <(printf '%s\n' "$before") <(printf '%s\n' "$after"))
[ -z "$removed" ] || fail "uninstall removed what install did not write:
$removed"
added=$(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after"))
# gff-hab's own state (structure, keys, checkpoints) stays by design (spec 5.5); nothing else may.
stray=$(printf '%s\n' "$added" | grep -v -x -e "$state" -e "$state/.*" || true)
installed=$(printf '%s\n' "$added" | grep -x -e "$state/bin.*" -e "$state/logs.*" -e "$state/builds.*" \
	-e "$state/current" -e "$state/install.json" || true)
[ -z "$stray$installed" ] || fail "uninstall left:
$stray$installed"
holder_count_is 0 || fail "holder registry files left: $(ls "$holders")"
if [ "$init" = launchd ]; then
	[ -z "$(registered_builds)" ] || fail "LaunchServices still holds builds after uninstall: $(registered_builds)"
fi
passed=1
say "PASS: $loops restarts and one reinstall kept child $child; its drained holder retired;" \
	"uninstall removed every installed file and job ($(printf '%s' "$seen" | wc -w | tr -d ' ') holders seen)"
