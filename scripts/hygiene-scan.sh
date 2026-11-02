#!/bin/sh
# Refuse local-context leaks: private addresses, home paths, and private patterns supplied from
# outside the repo. Spec: docs/specs/ci-release.md 9.
#
#   scripts/hygiene-scan.sh --staged              added lines of the index (pre-commit hook)
#   scripts/hygiene-scan.sh --range BASE..HEAD    added lines and commit messages of a range
#   scripts/hygiene-scan.sh --tree                every tracked text file at HEAD
#   scripts/hygiene-scan.sh --message FILE        one commit message (commit-msg hook)
#   scripts/hygiene-scan.sh --install-hooks       install the pre-commit and commit-msg hooks
#
# --range also scans $GFF_HYGIENE_PR_TITLE and $GFF_HYGIENE_PR_BODY when set.
#
# Private rules: $GFF_HYGIENE_PATTERNS (the rules themselves), else the file that
# $GFF_HYGIENE_PATTERNS_FILE names. One rule per line, "<name><TAB><extended regex>"; lines that
# start with # are comments. The name is printed on a hit, so it must say nothing private.
#
# On a hit it prints "<where>:<line>: <rule>" and exits 1. It never prints the matched text, the
# line, or a pattern. Allowed built-in hits: .github/hygiene-allow, "<path><TAB><rule>" per line.
# Exit 0 clean, 1 hits, 2 usage or setup error.
set -eu

die() { echo "hygiene-scan: $*" >&2; exit 2; }

mode=${1:-}
arg=${2:-}
case $mode in
--staged | --tree | --install-hooks) [ $# -eq 1 ] || die "$mode takes no argument" ;;
--range | --message) [ $# -eq 2 ] && [ -n "$arg" ] || die "$mode needs one argument" ;;
*) die "usage: hygiene-scan.sh --staged | --range BASE..HEAD | --tree | --message FILE | --install-hooks" ;;
esac

top=$(git rev-parse --show-toplevel 2>/dev/null) || die "not inside a git work tree"
cd "$top"

if [ "$mode" = --install-hooks ]; then
	marker='# installed by scripts/hygiene-scan.sh'
	if hooks_path=$(git config --get core.hooksPath); then
		cat >&2 <<MSG
hygiene-scan: core.hooksPath is set ($hooks_path), so git ignores this repo's own hooks.
Add these two calls to the hooks that path holds, when run inside this repo:
  pre-commit:  scripts/hygiene-scan.sh --staged
  commit-msg:  scripts/hygiene-scan.sh --message "\$1"
MSG
		exit 2
	fi
	dir=$(git rev-parse --git-path hooks)
	mkdir -p "$dir"
	for hook in pre-commit commit-msg; do
		f="$dir/$hook"
		if [ -e "$f" ] && ! grep -qF "$marker" "$f"; then
			die "$f exists and is not ours; add the scan to it by hand"
		fi
	done
	printf '#!/bin/sh\n%s\nexec scripts/hygiene-scan.sh --staged\n' "$marker" >"$dir/pre-commit"
	printf '#!/bin/sh\n%s\nexec scripts/hygiene-scan.sh --message "$1"\n' "$marker" >"$dir/commit-msg"
	chmod +x "$dir/pre-commit" "$dir/commit-msg"
	echo "hygiene-scan: installed pre-commit and commit-msg hooks in $dir"
	exit 0
fi

# Private rules reach awk through the environment only: never argv, never a temporary file.
if [ -z "${GFF_HYGIENE_PATTERNS:-}" ] && [ -n "${GFF_HYGIENE_PATTERNS_FILE:-}" ]; then
	[ -r "$GFF_HYGIENE_PATTERNS_FILE" ] || die "cannot read \$GFF_HYGIENE_PATTERNS_FILE"
	GFF_HYGIENE_PATTERNS=$(cat "$GFF_HYGIENE_PATTERNS_FILE")
fi
export GFF_HYGIENE_PATTERNS="${GFF_HYGIENE_PATTERNS:-}"
if [ -z "$GFF_HYGIENE_PATTERNS" ]; then
	echo "hygiene-scan: no private patterns set; built-in rules only" >&2
fi
allow=.github/hygiene-allow
[ -f "$allow" ] || allow=/dev/null
export GFF_HYGIENE_ALLOW="$allow"


# Git output goes to files in a private temporary directory, so a failing git command is seen
# (no pipe hides its status). The private rules never go there.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/hygiene-scan.XXXXXX") || die "mktemp failed"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 2' INT TERM HUP
stream="$tmp/stream"
: >"$stream"

# The stream holds three kinds of line: "F<where>" starts a source (hits are reported under its
# name), "L<n>" gives the next text line's number, "T<text>" is one line to scan.
scan() {
	LC_ALL=C awk '
	BEGIN {
		nb = 0
		builtin[++nb] = "private-ipv4"
		re[nb] = "(^|[^0-9.])(10\\.[0-9]+\\.[0-9]+\\.[0-9]+|192\\.168\\.[0-9]+\\.[0-9]+|172\\.(1[6-9]|2[0-9]|3[01])\\.[0-9]+\\.[0-9]+)([^0-9]|$)"
		builtin[++nb] = "cgnat-ipv4"
		re[nb] = "(^|[^0-9.])100\\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\\.[0-9]+\\.[0-9]+([^0-9]|$)"
		np = 0
		n = split(ENVIRON["GFF_HYGIENE_PATTERNS"], lines, "\n")
		for (i = 1; i <= n; i++) {
			l = lines[i]
			sub(/\r$/, "", l)
			if (l == "" || substr(l, 1, 1) == "#") continue
			t = index(l, "\t")
			if (t < 2 || t == length(l)) { bad++; continue }
			pname[++np] = substr(l, 1, t - 1)
			pre[np] = substr(l, t + 1)
		}
		if (bad) print "hygiene-scan: " bad " private pattern line(s) not <name><TAB><regex>, skipped" > "/dev/stderr"
		split("user u x other runner you linuxbrew Shared", ph, " ")
		for (i in ph) placeholder[ph[i]] = 1
		while ((getline l < ENVIRON["GFF_HYGIENE_ALLOW"]) > 0) {
			if (l ~ /^#/ || l == "") continue
			allowed[l] = 1
		}
		hits = 0
	}
	function hit(rule, builtin_rule) {
		if (builtin_rule && ((where "\t" rule) in allowed)) return
		printf "%s:%d: %s\n", where, lineno, rule
		hits++
	}
	function homepath(s,    rest, name) {
		rest = s
		while (match(rest, /(^|[^A-Za-z0-9._-])\/(Users|home)\/[A-Za-z0-9._-]+/)) {
			name = substr(rest, RSTART, RLENGTH)
			sub(/^.*\/(Users|home)\//, "", name)
			if (!(name in placeholder)) return 1
			rest = substr(rest, RSTART + RLENGTH)
		}
		return 0
	}
	{ kind = substr($0, 1, 1); $0 = substr($0, 2) }
	kind == "F" { where = $0; lineno = 0; next }
	kind == "L" { lineno = $0 - 1; next }
	{
		lineno++
		for (i = 1; i <= nb; i++) if ($0 ~ re[i]) hit(builtin[i], 1)
		if (homepath($0)) hit("home-path", 1)
		for (i = 1; i <= np; i++) if ($0 ~ pre[i]) hit(pname[i], 0)
	}
	END { exit hits > 0 ? 1 : 0 }
	' "$stream"
}

# Added lines of a diff, with their new line numbers. Deleted lines and binary files are skipped.
# The file header ends at the first hunk, so an added line that starts with "++" is text.
add_diff() {
	git -c core.quotepath=off diff --no-color --no-ext-diff --no-renames -U0 "$@" >"$tmp/diff" ||
		die "git diff failed"
	awk '
	/^diff --git / { hdr = 1; next }
	hdr && /^\+\+\+ / { f = substr($0, 5); sub(/^b\//, "", f); print "F" f; next }
	hdr && !/^@@ / { next }
	/^@@ / { hdr = 0; split($3, a, ","); print "L" substr(a[1], 2); next }
	/^\+/ { print "T" substr($0, 2) }
	' "$tmp/diff" >>"$stream"
}

# Commit messages of a range, one source per commit.
add_log() {
	git log --no-color --format='%x1e%h%n%B' "$1" >"$tmp/log" || die "git log failed"
	awk '
	substr($0, 1, 1) == "\036" { print "Fcommit " substr($0, 2); next }
	{ print "T" $0 }
	' "$tmp/log" >>"$stream"
}

# Every tracked text file at HEAD. Paths hold no colon (checked), so the fields split cleanly.
add_tree() {
	git ls-files >"$tmp/files" || die "git ls-files failed"
	if grep -q ':' "$tmp/files"; then die "a tracked path holds a colon; --tree cannot split it"; fi
	# git grep exits 1 when no line matched (an empty tree): not an error.
	rc=0
	git -c core.quotepath=off grep -I -n --no-color -e '' HEAD -- . >"$tmp/tree" || rc=$?
	[ "$rc" -le 1 ] || die "git grep failed"
	awk '
	{
		sub(/^HEAD:/, "")
		c = index($0, ":"); f = substr($0, 1, c - 1); rest = substr($0, c + 1)
		c = index(rest, ":")
		if (f != last) { print "F" f; last = f }
		print "L" substr(rest, 1, c - 1)
		print "T" substr(rest, c + 1)
	}
	' "$tmp/tree" >>"$stream"
}

add_text() { # label, text
	printf 'F%s\n' "$1" >>"$stream"
	printf '%s\n' "$2" | awk '{ print "T" $0 }' >>"$stream"
}

case $mode in
--staged) add_diff --cached ;;
--tree) add_tree ;;
--message)
	[ -r "$arg" ] || die "cannot read $arg"
	# Drop git's comment lines, as git does before it records the message.
	printf 'Fcommit message\n' >>"$stream"
	awk '!/^#/ { print "T" $0 }' "$arg" >>"$stream"
	;;
--range)
	case $arg in *..*) ;; *) die "--range wants BASE..HEAD" ;; esac
	base=${arg%%..*} head=${arg##*..}
	git rev-parse --verify --quiet "$base^{commit}" >/dev/null || die "unknown base in $arg"
	git rev-parse --verify --quiet "$head^{commit}" >/dev/null || die "unknown head in $arg"
	# The diff from the merge base: only what the range adds, not what the base gained meanwhile.
	add_diff "$base...$head"
	add_log "$base..$head"
	if [ -n "${GFF_HYGIENE_PR_TITLE:-}" ]; then add_text "pull request title" "$GFF_HYGIENE_PR_TITLE"; fi
	if [ -n "${GFF_HYGIENE_PR_BODY:-}" ]; then add_text "pull request body" "$GFF_HYGIENE_PR_BODY"; fi
	;;
esac

rc=0
scan || rc=$?
[ "$rc" -le 1 ] || die "the scanner failed"
exit "$rc"
