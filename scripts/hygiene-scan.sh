#!/bin/sh
# Refuse local-context leaks: private addresses, tailnet names, home paths, and private patterns
# supplied from outside the repo. Spec: docs/specs/ci-release.md 9.
#
#   scripts/hygiene-scan.sh --staged              added lines and paths of the index (pre-commit)
#   scripts/hygiene-scan.sh --range BASE..HEAD    every commit in the range: its added lines and
#                                                 paths, its message, author and committer
#   scripts/hygiene-scan.sh --tree                every tracked text file and path at HEAD
#   scripts/hygiene-scan.sh --message FILE        one commit message (commit-msg hook)
#   scripts/hygiene-scan.sh --install-hooks       install the pre-commit and commit-msg hooks
#
# --range also scans $GFF_HYGIENE_PR_TITLE and $GFF_HYGIENE_PR_BODY; --range and --tree also scan
# $GFF_HYGIENE_REF (a branch or tag name). Each is skipped when unset.
#
# Private rules: $GFF_HYGIENE_PATTERNS (the rules themselves), else the file that
# $GFF_HYGIENE_PATTERNS_FILE names. One rule per line, "<name><TAB><extended regex>"; lines that
# start with # are comments. The name is printed on a hit, so it must say nothing private, and
# holds only letters, digits, dot, dash and underscore. A rule that is malformed, does not
# compile, or matches the empty string stops the scan (exit 2); the error names the rule's number
# only, never its text.
#
# On a hit it prints "<where>:<line>: <rule>" and exits 1. It never prints the matched text, the
# line, or a pattern. A file path that hits prints as "<path withheld>". Allowed built-in hits:
# .github/hygiene-allow, "<path><TAB><rule>" per line. $GFF_HYGIENE_AWK picks the awk (CI uses
# gawk). Exit 0 clean, 1 hits, 2 usage or setup error.
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

AWK=${GFF_HYGIENE_AWK:-awk}
command -v "$AWK" >/dev/null 2>&1 || die "no awk named $AWK"

# Private rules reach awk through the environment only: never argv, never a file the script
# writes. Each one is checked on its own first, with awk's stderr silenced, because awk prints a
# regex that does not compile in its error message.
if [ -z "${GFF_HYGIENE_PATTERNS:-}" ] && [ -n "${GFF_HYGIENE_PATTERNS_FILE:-}" ]; then
	[ -r "$GFF_HYGIENE_PATTERNS_FILE" ] || die "cannot read \$GFF_HYGIENE_PATTERNS_FILE"
	GFF_HYGIENE_PATTERNS=$(cat "$GFF_HYGIENE_PATTERNS_FILE")
fi
nl='
'
tab=$(printf '\t')
cr=$(printf '\r')
rules='' bad='' n=0
saved_ifs=$IFS
IFS=$nl
set -f
for line in ${GFF_HYGIENE_PATTERNS:-}; do
	line=${line%"$cr"}
	case $line in '' | '#'*) continue ;; esac
	n=$((n + 1))
	name=${line%%"$tab"*}
	re=${line#*"$tab"}
	case $name in '' | *[!A-Za-z0-9._-]*) bad="$bad $n"; continue ;; esac
	if [ "$name" = "$line" ] || [ -z "$re" ]; then bad="$bad $n"; continue; fi
	# Exit 0 only for a regex that compiles and does not match the empty string.
	if ! GFF_HYGIENE_RE=$re "$AWK" 'BEGIN { if ("" ~ ENVIRON["GFF_HYGIENE_RE"]) exit 3; exit 0 }' \
		</dev/null >/dev/null 2>&1; then
		bad="$bad $n"
		continue
	fi
	rules="$rules$name$tab$re$nl"
done
IFS=$saved_ifs
set +f
unset GFF_HYGIENE_PATTERNS
[ -z "$bad" ] || die "private rule$bad: want <name><TAB><regex>, a name of letters, digits, dots, dashes or underscores, and a regex that compiles and never matches an empty line"
export GFF_HYGIENE_RULES="$rules"
[ -n "$rules" ] || echo "hygiene-scan: no private patterns set; built-in rules only" >&2
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

# The stream holds four kinds of line:
#   F<label>             starts a source reported under that label (a commit, a message)
#   P<path><TAB><suffix> starts a file: the path itself is scanned, and hits are reported as
#                        "<path><suffix>", or "<path withheld><suffix>" when the path hits
#   L<n>                 the next text line's number
#   T<text>              one line to scan
scan() {
	# gawk keeps one compiled regex per `~` in the program text and compiles again whenever the
	# string there changes, so one `~` in a loop over the rules compiles every rule on every line.
	# Each private rule gets its own `~` instead: the generated text holds only rule numbers.
	pmatch='' i=0
	while [ "$i" -lt "$n" ]; do
		i=$((i + 1))
		pmatch="$pmatch	if (s ~ pre[$i]) out = out \" \" pname[$i]
"
	done
	LC_ALL=C "$AWK" '
	BEGIN {
		np = split(ENVIRON["GFF_HYGIENE_RULES"], lines, "\n")
		n = 0
		for (i = 1; i <= np; i++) {
			if (lines[i] == "") continue
			t = index(lines[i], "\t")
			pname[++n] = substr(lines[i], 1, t - 1)
			pre[n] = substr(lines[i], t + 1)
		}
		np = n
		split("user u x other runner you linuxbrew Shared", ph, " ")
		for (i in ph) placeholder[ph[i]] = 1
		while ((getline l < ENVIRON["GFF_HYGIENE_ALLOW"]) > 0) {
			if (l ~ /^#/ || l == "") continue
			allowed[l] = 1
		}
		hits = 0
	}
	function allowed_hit(rule, builtin_rule) {
		return builtin_rule && ((key "\t" rule) in allowed)
	}
	function hit(rule, builtin_rule) {
		if (allowed_hit(rule, builtin_rule)) return
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
	# The built-in rules that match s, each after a space, as regex literals compiled once.
	function builtins_on(s,    out) {
		out = ""
		if (s ~ /(^|[^0-9.])(10\.[0-9]+\.[0-9]+\.[0-9]+|192\.168\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]+\.[0-9]+)([^0-9]|$)/) out = out " private-ipv4"
		if (s ~ /(^|[^0-9.])100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]+\.[0-9]+([^0-9]|$)/) out = out " cgnat-ipv4"
		if (s ~ /(^|[^0-9A-Fa-f:])[Ff][Dd]7[Aa]:115[Cc]:[Aa]1[Ee]0:/) out = out " tailnet-ipv6"
		if (s ~ /[A-Za-z0-9-]+\.ts\.net([^A-Za-z0-9-]|$)/) out = out " tailnet-name"
		if (homepath(s)) out = out " home-path"
		return out
	}
	# The private rules that match s, each after a space.
	function private_on(s,    out) {
		out = ""
'"$pmatch"'		return out
	}
	# Every rule that matches s, each after a space.
	function rules_on(s,    b, k, r, i, out) {
		out = ""
		b = builtins_on(s)
		if (b != "") {
			k = split(substr(b, 2), r, " ")
			for (i = 1; i <= k; i++) if (!allowed_hit(r[i], 1)) out = out " " r[i]
		}
		return out private_on(s)
	}
	{ kind = substr($0, 1, 1); $0 = substr($0, 2) }
	kind == "F" { where = $0; key = $0; lineno = 0; next }
	kind == "P" {
		t = index($0, "\t")
		path = substr($0, 1, t - 1); suffix = substr($0, t + 1)
		key = path; lineno = 0
		found = rules_on(path)
		if (found != "") {
			where = "<path withheld>" suffix
			k = split(substr(found, 2), r, " ")
			for (i = 1; i <= k; i++) { printf "%s: %s (file path)\n", where, r[i]; hits++ }
		} else {
			where = path suffix
		}
		next
	}
	kind == "L" { lineno = $0 - 1; next }
	{
		lineno++
		found = builtins_on($0)
		if (found != "") {
			k = split(substr(found, 2), r, " ")
			for (i = 1; i <= k; i++) hit(r[i], 1)
		}
		if (np) {
			found = private_on($0)
			if (found != "") {
				k = split(substr(found, 2), r, " ")
				for (i = 1; i <= k; i++) hit(r[i], 0)
			}
		}
	}
	END { exit hits > 0 ? 1 : 0 }
	' "$stream"
}

# Added lines and paths of a patch stream: a plain diff, or `git log -p` whose commits start with
# a \036<short sha> line. Deleted lines and binary contents are skipped. The file header ends at
# the first hunk, so an added line that starts with "++" is text.
add_patches() { # file
	"$AWK" '
	substr($0, 1, 1) == "\036" { suffix = " (commit " substr($0, 2) ")"; hdr = 0; next }
	/^diff --git / { hdr = 1; next }
	hdr && /^\+\+\+ / {
		f = substr($0, 5)
		if (f == "/dev/null") next
		sub(/^b\//, "", f)
		print "P" f "\t" suffix
		next
	}
	# A binary file still names its new path; scan the path, never the contents.
	hdr && /^Binary files / {
		f = $0; sub(/^Binary files .* and b\//, "", f); sub(/ differ$/, "", f)
		if (f !~ /^Binary files/) print "P" f "\t" suffix
		next
	}
	hdr && !/^@@ / { next }
	/^@@ / { hdr = 0; split($3, a, ","); print "L" substr(a[1], 2); next }
	/^\+/ && !hdr { print "T" substr($0, 2) }
	' "$1" >>"$stream"
}

add_staged() {
	git -c core.quotepath=off diff --cached --no-color --no-ext-diff --no-renames -U0 >"$tmp/diff" ||
		die "git diff failed"
	add_patches "$tmp/diff"
}

# Every commit of the range, one by one: a line a later commit removes was still pushed.
# Merge commits are left out; their parents' commits are in the range.
add_commits() { # base head
	git -c core.quotepath=off log -p -U0 --no-merges --no-color --no-ext-diff --no-renames \
		--format='%x1e%h' "$1..$2" >"$tmp/patches" || die "git log -p failed"
	add_patches "$tmp/patches"
	# Message, author and committer of each commit, merges included.
	git log --no-color --format='%x1e%h%n%an <%ae>%n%cn <%ce>%n%B' "$1..$2" >"$tmp/log" ||
		die "git log failed"
	"$AWK" '
	substr($0, 1, 1) == "\036" { c = substr($0, 2); state = 1; next }
	state == 1 { print "Fcommit " c " author"; print "T" $0; state = 2; next }
	state == 2 { print "Fcommit " c " committer"; print "T" $0; print "Fcommit " c; state = 3; next }
	{ print "T" $0 }
	' "$tmp/log" >>"$stream"
}

# Every tracked text file at HEAD, and every tracked path. Paths hold no colon (checked), so the
# fields split cleanly.
add_tree() {
	git -c core.quotepath=off ls-files >"$tmp/files" || die "git ls-files failed"
	if grep -q ':' "$tmp/files"; then die "a tracked path holds a colon; --tree cannot split it"; fi
	# git grep exits 1 when no line matched (an empty tree): not an error.
	rc=0
	git -c core.quotepath=off grep -I -n --no-color -e '' HEAD -- . >"$tmp/tree" || rc=$?
	[ "$rc" -le 1 ] || die "git grep failed"
	"$AWK" '
	FNR == NR { paths[++np] = $0; next }
	{
		sub(/^HEAD:/, "")
		c = index($0, ":"); f = substr($0, 1, c - 1); rest = substr($0, c + 1)
		c = index(rest, ":")
		if (f != last) { print "P" f "\t"; last = f; seen[f] = 1 }
		print "L" substr(rest, 1, c - 1)
		print "T" substr(rest, c + 1)
	}
	# Paths of binary and empty files, which git grep never prints.
	END { for (i = 1; i <= np; i++) if (!(paths[i] in seen)) print "P" paths[i] "\t" }
	' "$tmp/files" "$tmp/tree" >>"$stream"
}

add_text() { # label, text
	printf 'F%s\n' "$1" >>"$stream"
	printf '%s\n' "$2" | "$AWK" '{ print "T" $0 }' >>"$stream"
}

add_ref() {
	if [ -n "${GFF_HYGIENE_REF:-}" ]; then add_text "ref name" "$GFF_HYGIENE_REF"; fi
}

case $mode in
--staged) add_staged ;;
--tree)
	add_tree
	add_ref
	;;
--message)
	[ -r "$arg" ] || die "cannot read $arg"
	# Drop git's comment lines and everything below the scissors line of `git commit -v`, as git
	# does before it records the message.
	cc=$(git config --get core.commentChar 2>/dev/null || true)
	case $cc in '' | auto) cc='#' ;; esac
	printf 'Fcommit message\n' >>"$stream"
	GFF_HYGIENE_CC=$cc "$AWK" '
	BEGIN { cc = substr(ENVIRON["GFF_HYGIENE_CC"], 1, 1) }
	$0 == cc " ------------------------ >8 ------------------------" { exit }
	substr($0, 1, 1) == cc { next }
	{ print "L" NR; print "T" $0 }
	' "$arg" >>"$stream"
	;;
--range)
	case $arg in *..*) ;; *) die "--range wants BASE..HEAD" ;; esac
	base=${arg%%..*} head=${arg##*..}
	git rev-parse --verify --quiet "$base^{commit}" >/dev/null || die "unknown base in $arg"
	git rev-parse --verify --quiet "$head^{commit}" >/dev/null || die "unknown head in $arg"
	add_commits "$base" "$head"
	add_ref
	if [ -n "${GFF_HYGIENE_PR_TITLE:-}" ]; then add_text "pull request title" "$GFF_HYGIENE_PR_TITLE"; fi
	if [ -n "${GFF_HYGIENE_PR_BODY:-}" ]; then add_text "pull request body" "$GFF_HYGIENE_PR_BODY"; fi
	;;
esac

# The scanner's own stderr is silenced: no message of awk's may echo a pattern.
rc=0
scan 2>/dev/null || rc=$?
[ "$rc" -le 1 ] || die "the scanner failed (awk exit $rc)"
exit "$rc"
