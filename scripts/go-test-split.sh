#!/usr/bin/env bash
# Run one shard of go test over a package list, with one slow package split across shards
# (docs/specs/ci-release.md 3.2). CI runs each shard as its own job on its own runner.
#
#   scripts/go-test-split.sh <shard> <slow package> [go test flags...] -- <packages...>
#
# <shard> is `all`, `0/n` or `i/n` (1 <= i <= n):
#   all  `go test <flags> <packages>`, nothing split.
#   0/n  every package but the slow one.
#   i/n  the slow package only, with the i-th share of its top-level tests.
# Shares: the test names, sorted, cut into batches of 16 (GO_TEST_SPLIT_BATCH), and batch b goes
# to shard (b mod n) + 1. Batches, not single names, keep a family of tests together; dealing
# them round the shards spreads the long families (measured: four shards within 1.6x of each
# other). Shards 0 to n together run every test once per -count, in exactly one shard.
#
# The test names come from the `func TestX(` lines of the slow package's _test.go files. A file
# that a build tag leaves out only adds names that match nothing, so no test can be missed. A -run
# in the flags is refused.
set -euo pipefail

GO=${GO:-go}
BATCH=${GO_TEST_SPLIT_BATCH:-16}

usage() {
	echo "usage: $0 <all | 0/n | i/n> <slow package> [go test flags...] -- <packages...>" >&2
	exit 64
}

[ $# -ge 3 ] || usage
shard=$1
slow=$2
shift 2

flags=()
while [ $# -gt 0 ] && [ "$1" != -- ]; do
	case "$1" in -run | -run=*) echo "$0: -run cannot be combined with a shard" >&2; exit 64 ;; esac
	flags+=("$1")
	shift
done
[ "${1:-}" = -- ] || usage
shift
[ $# -gt 0 ] || usage
pkgs=("$@")

if [ "$shard" = all ]; then
	exec "$GO" test ${flags[@]+"${flags[@]}"} "${pkgs[@]}"
fi
[[ "$shard" =~ ^([0-9]+)/([1-9][0-9]*)$ ]] || usage
i=${BASH_REMATCH[1]}
n=${BASH_REMATCH[2]}
[ "$i" -le "$n" ] || usage

slow_path=$("$GO" list "$slow")
if [ "$i" = 0 ]; then
	rest=()
	found=0
	while IFS= read -r p; do
		if [ "$p" = "$slow_path" ]; then found=1; else rest+=("$p"); fi
	done < <("$GO" list "${pkgs[@]}")
	[ "$found" = 1 ] || { echo "$0: $slow is not among the packages" >&2; exit 64; }
	[ "${#rest[@]}" -gt 0 ] || exit 0
	exec "$GO" test ${flags[@]+"${flags[@]}"} "${rest[@]}"
fi

dir=$("$GO" list -f '{{.Dir}}' "$slow")
names=()
while IFS= read -r t; do names+=("$t"); done < <(
	sed -n 's/^func \(Test[A-Za-z0-9_]*\)(.*/\1/p' "$dir"/*_test.go | grep -vx 'TestMain' | sort -u
)
[ "${#names[@]}" -gt 0 ] || { echo "$0: no tests found in $dir" >&2; exit 1; }

mine=()
b=0
for ((k = 0; k < ${#names[@]}; k += BATCH)); do
	if [ $((b % n + 1)) = "$i" ]; then mine+=("${names[@]:k:BATCH}"); fi
	b=$((b + 1))
done
[ "${#mine[@]}" -gt 0 ] || { echo "shard $shard of $slow: no tests"; exit 0; }
echo "shard $shard of $slow: ${#mine[@]} of ${#names[@]} tests"
re=$(IFS='|'; echo "${mine[*]}")
exec "$GO" test ${flags[@]+"${flags[@]}"} -run "^($re)\$" "$slow"
