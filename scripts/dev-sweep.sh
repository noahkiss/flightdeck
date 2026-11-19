# shellcheck shell=bash
# Shared by scripts/service-check.sh and scripts/clean-dev.sh: find and remove what a test run of
# this repo left on the machine. Source it from a script with `set -euo pipefail`; it defines
# functions only. Every function touches one of this repo's own test prefixes and nothing else:
# launchd labels and systemd units that start with gffsvctest-, the service check's work folders
# gffsvc.XXXXXX, and the Go tests' temporary folders (internal/testtmp). An installed service
# (labels flightdeck.*) never matches.
#
# DRY_RUN=1 prints each action instead of running it. A failed step is reported, never fatal.

sweep_uid=$(id -u)
svc_label_prefix=gffsvctest-

# sweep_do <command...>: run the command, or print it under DRY_RUN=1. It always succeeds.
sweep_do() {
	if [ "${DRY_RUN:-0}" = 1 ]; then
		printf 'would run:'
		printf ' %q' "$@"
		printf '\n'
	else
		"$@" 2>/dev/null || true
	fi
}

sweep_say() { printf '%s\n' "$*" >&2; }

# sweep_pids_naming <dir>: the pids of this user's processes, other than this shell, whose
# command line names a path inside dir.
sweep_pids_naming() {
	local list
	# The list is taken before awk starts: awk's own command line names the directory too.
	list=$(ps -U "$sweep_uid" -ww -o pid= -o args= 2>/dev/null || true)
	printf '%s\n' "$list" |
		awk -v d="$1/" -v me="$$" '{ pid = $1; sub(/^[ \t]*[0-9]+[ \t]+/, ""); if (pid != me && index($0, d) > 0) print pid }'
}

# sweep_stop_pids <pid...>: SIGTERM, up to 3 s to end, then SIGKILL.
sweep_stop_pids() {
	[ $# -gt 0 ] || return 0
	local p left _
	sweep_do kill -TERM "$@"
	[ "${DRY_RUN:-0}" = 1 ] && return 0
	for _ in $(seq 30); do
		left=
		for p in "$@"; do
			if kill -0 "$p" 2>/dev/null; then left="$left $p"; fi
		done
		[ -n "$left" ] || return 0
		sleep 0.1
	done
	# shellcheck disable=SC2086 # left is a list of pids
	kill -KILL $left 2>/dev/null || true
}

# sweep_remove_dir <dir>: stop the processes started from dir, unregister any app bundle in it
# from LaunchServices (macOS), and delete it, read-only files included.
sweep_remove_dir() {
	local dir=${1:?} pids app
	case $dir in
	/*/*) ;;
	*)
		sweep_say "dev-sweep: refusing to remove '$dir'"
		return 0
		;;
	esac
	[ -d "$dir" ] || return 0
	pids=$(sweep_pids_naming "$dir")
	if [ -n "$pids" ]; then
		# shellcheck disable=SC2086 # one word per pid
		sweep_say "dev-sweep: stopping processes started from $dir:" $pids
		# shellcheck disable=SC2086 # one word per pid
		sweep_stop_pids $pids
	fi
	if [ "$(uname -s)" = Darwin ]; then
		local lsregister=/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
		while IFS= read -r app; do
			[ -z "$app" ] || sweep_do "$lsregister" -u "$app"
		done < <(find "$dir" -name '*.app' -prune -type d 2>/dev/null || true)
	fi
	sweep_say "dev-sweep: removing $dir"
	sweep_do chmod -R u+w "$dir"
	sweep_do rm -rf -- "${dir:?}"
	if [ "${DRY_RUN:-0}" != 1 ] && [ -e "$dir" ]; then
		sweep_say "dev-sweep: could not remove all of $dir"
	fi
	return 0
}

# svc_owner_alive <label>: a label of the older form gffsvctest-<pid>... whose pid still runs.
svc_owner_alive() {
	local rest=${1#"$svc_label_prefix"}
	rest=${rest%%[.-]*}
	case $rest in
	'' | *[!0-9]*) return 1 ;;
	esac
	kill -0 "$rest" 2>/dev/null
}

# svc_sweep_launchd <unit dir>: boot out every gffsvctest-* launch agent and delete its plist.
svc_sweep_launchd() {
	local unitdir=$1 p label labels
	labels=$(
		for p in "$unitdir/$svc_label_prefix"*.plist; do
			if [ -e "$p" ]; then basename "$p" .plist; fi
		done
		{ launchctl list 2>/dev/null || true; } | awk -v pre="$svc_label_prefix" 'index($3, pre) == 1 { print $3 }'
	)
	for label in $(printf '%s\n' "$labels" | sort -u); do
		case $label in "$svc_label_prefix"*) ;; *) continue ;; esac
		if svc_owner_alive "$label"; then
			sweep_say "dev-sweep: $label belongs to a run that still goes; left alone"
			continue
		fi
		sweep_say "dev-sweep: removing launch agent $label"
		sweep_do launchctl bootout "gui/$sweep_uid/$label"
		sweep_do rm -f "$unitdir/$label.plist"
	done
}

# svc_sweep_systemd <unit dir>: stop and remove every gffsvctest-* systemd user unit.
svc_sweep_systemd() {
	local unitdir=$1 u units f found=
	command -v systemctl >/dev/null 2>&1 || return 0
	units=$(
		{ systemctl --user list-units --all --plain --no-legend "$svc_label_prefix*" 2>/dev/null || true; } | awk '{ print $1 }'
		for f in "$unitdir/$svc_label_prefix"*; do
			if [ -e "$f" ]; then basename "$f"; fi
		done
	)
	for u in $(printf '%s\n' "$units" | sort -u); do
		case $u in "$svc_label_prefix"*) ;; *) continue ;; esac
		if svc_owner_alive "$u"; then
			sweep_say "dev-sweep: $u belongs to a run that still goes; left alone"
			continue
		fi
		found=1
		sweep_say "dev-sweep: removing systemd user unit $u"
		case $u in
		*@.service) ;; # a template; its instances are listed on their own
		*) sweep_do systemctl --user disable --now "$u" ;;
		esac
		sweep_do rm -f "$unitdir/$u" "$unitdir/default.target.wants/$u"
	done
	if [ -n "$found" ]; then
		sweep_do systemctl --user daemon-reload
		sweep_do systemctl --user reset-failed "$svc_label_prefix*"
	fi
}

# svc_sweep_work <tmp dir>: remove the service check's work folders gffsvc.XXXXXX in the tmp
# dir, with the processes started from them.
svc_sweep_work() {
	local d
	for d in "$1"/gffsvc.??????; do
		if [ -d "$d" ] && [ -O "$d" ]; then sweep_remove_dir "$d"; fi
	done
}

# One service check runs at a time per user: it holds <tmp dir>/gffsvc.lock, a folder with its
# pid. svc_lock_owner <tmp dir> prints the pid of a live holder of the lock.
svc_lock_owner() {
	local pid
	pid=$(cat "$1/gffsvc.lock/pid" 2>/dev/null) || return 1
	case $pid in '' | *[!0-9]*) return 1 ;; esac
	kill -0 "$pid" 2>/dev/null && printf '%s\n' "$pid"
}

# svc_lock <tmp dir>: take the lock for this shell, after a dead holder's; fail when a live run
# holds it.
svc_lock() {
	local lock=${1:?}/gffsvc.lock owner
	if ! mkdir -m 700 "$lock" 2>/dev/null; then
		if owner=$(svc_lock_owner "$1"); then
			sweep_say "service-check: another service check (pid $owner) runs; one at a time"
			return 1
		fi
		rm -rf -- "${lock:?}"
		mkdir -m 700 "$lock" || return 1
	fi
	printf '%s\n' "$$" >"$lock/pid"
}

# svc_unlock <tmp dir>: drop the lock when this shell holds it.
svc_unlock() {
	local lock=${1:?}/gffsvc.lock
	if [ "$(cat "$lock/pid" 2>/dev/null)" = "$$" ]; then rm -rf -- "${lock:?}"; fi
}

# svc_sweep <tmp dir>: what earlier service checks left: their jobs or units, their processes,
# their work folders. The caller holds the lock, or knows no service check runs.
svc_sweep() {
	case $(uname -s) in
	Darwin) svc_sweep_launchd "$HOME/Library/LaunchAgents" ;;
	Linux) svc_sweep_systemd "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user" ;;
	esac
	svc_sweep_work "$1"
}

# go_test_prefixes: the os.MkdirTemp prefixes of this repo's Go tests and TestMains. A folder
# matches only as <prefix> followed by digits, the form os.MkdirTemp makes.
go_test_prefixes='gffhab gffh gffhc gffhcbin gffc gffcl gffd gffk gffm gffp fdlink fdl fdbin fda fdb fdc fdg fdh fdm fdr lb'

# go_sweep_tmp <tmp dir> <minutes>: remove the Go tests' leftover folders in the tmp dir. A
# folder with a testtmp pid file goes when its owner no longer runs; one without goes when it
# has not changed for <minutes>, so a test that runs now keeps its folders.
go_sweep_tmp() {
	local tmp=$1 age=$2 d name pre pid match
	for d in "$tmp"/*; do
		[ -d "$d" ] && [ -O "$d" ] || continue
		name=${d##*/}
		match=
		for pre in $go_test_prefixes; do
			case ${name#"$pre"} in
			"$name" | '' | *[!0-9]*) ;;
			*) match=1 ;;
			esac
		done
		[ -n "$match" ] || continue
		if [ -f "$d/.testtmp-pid" ]; then
			pid=$(cat "$d/.testtmp-pid" 2>/dev/null || true)
			case $pid in '' | *[!0-9]*) continue ;; esac
			if kill -0 "$pid" 2>/dev/null; then continue; fi
		elif [ -z "$(find "$d" -maxdepth 0 -mmin +"$age" 2>/dev/null || true)" ]; then
			continue
		fi
		sweep_remove_dir "$d"
	done
}
