#!/usr/bin/env bash
# Validates the sync fixer's artifact before anything from it reaches the
# remote. Every artifact byte is untrusted: the AI job that made it read
# upstream code and logs, which can carry a prompt injection.
#
# Run from a full clone of this repository with origin/main fetched.
#
# usage: validate-sync-fix.sh <artifact dir> <run id> <upstream url> <upstream branch>
#
# The artifact dir may hold fix.bundle and base.txt (a repair) and comment.md.
# On success it prints "tip=<sha>" when there is a branch to push, and nothing
# when there is only a comment. Any failed check exits non-zero.

set -euo pipefail

dir=$1
run_id=$2
upstream_url=$3
upstream_branch=$4

fail() { echo "validate-sync-fix: $*" >&2; exit 1; }

[[ "$run_id" =~ ^[0-9]+$ ]] || fail "run id is not numeric"
ref="refs/heads/claude/sync-${run_id}"

if [ -e "$dir/comment.md" ]; then
  [ -f "$dir/comment.md" ] && [ ! -L "$dir/comment.md" ] || fail "comment.md is not a regular file"
  # GitHub rejects a comment body over 65536 characters.
  [ "$(wc -c < "$dir/comment.md")" -le 60000 ] || fail "comment.md is over 60000 bytes"
fi

if [ ! -e "$dir/fix.bundle" ]; then
  [ ! -e "$dir/base.txt" ] || fail "base.txt without fix.bundle"
  exit 0
fi
for f in fix.bundle base.txt; do
  [ -f "$dir/$f" ] && [ ! -L "$dir/$f" ] || fail "$f is missing or not a regular file"
done

# The expected upstream tip comes from upstream itself, never from the
# artifact. If upstream moved since the AI job ran, this fails closed.
git fetch --quiet --no-tags "$upstream_url" "+refs/heads/${upstream_branch}:refs/sync-fix/upstream"
upstream_tip=$(git rev-parse refs/sync-fix/upstream)

base=$(head -c 41 "$dir/base.txt" | tr -d '\n')
[[ "$base" =~ ^[0-9a-f]{40}$ ]] || fail "base.txt is not a 40-character SHA"
[ "$base" = "$upstream_tip" ] || fail "base ${base} is not the upstream tip ${upstream_tip}"

git bundle verify --quiet "$dir/fix.bundle" >/dev/null 2>&1 || fail "the bundle does not verify"

mapfile -t heads < <(git bundle list-heads "$dir/fix.bundle")
[ "${#heads[@]}" -eq 1 ] || fail "the bundle must hold exactly one ref; it holds ${#heads[@]}"
read -r tip head_ref <<< "${heads[0]}"
[ "$head_ref" = "$ref" ] || fail "the bundle ref is not ${ref}"
[[ "$tip" =~ ^[0-9a-f]{40}$ ]] || fail "the bundle tip is not a SHA"

git fetch --quiet --no-tags "$dir/fix.bundle" "+${ref}:refs/sync-fix/candidate"
[ "$(git rev-parse refs/sync-fix/candidate)" = "$tip" ] || fail "the fetched tip does not match the bundle"

git merge-base --is-ancestor "$upstream_tip" "$tip" || fail "the tip does not descend from the upstream tip"

# The fixer may not change .github/. Every commit it carries on top of upstream
# must hold a .github tree that already exists in the reviewed series on main
# (or no .github at all, as upstream has none).
old_base=$(git merge-base origin/main "$upstream_tip")
declare -A allowed=([none]=1)
while read -r c; do
  t=$(git rev-parse --verify --quiet "${c}:.github" || echo none)
  allowed[$t]=1
done < <(git rev-list "${old_base}..origin/main"; echo "$old_base")

count=0
while read -r c; do
  count=$((count + 1))
  [ "$count" -le 200 ] || fail "more than 200 commits on top of upstream"
  t=$(git rev-parse --verify --quiet "${c}:.github" || echo none)
  [ -n "${allowed[$t]:-}" ] || fail "commit ${c} changes .github/"
done < <(git rev-list "${upstream_tip}..${tip}")
[ "$count" -ge 1 ] || fail "the branch carries no commits on top of upstream"

echo "tip=${tip}"
