#!/usr/bin/env bash
# Keep one GitHub issue per scheduled workflow (docs/specs/ci-release.md 3.8). The last job of a
# scheduled run calls it:
#
#   - a failed run opens the issue, or comments on the open one;
#   - a passed run comments on the open issue and closes it.
#
# The repository is public, so the issue holds fixed text, the workflow name, the run URL, the
# commit and the names of the failed jobs: never a log line, a path or an environment value.
#
# Settings come from the environment (the workflow sets them):
#
#   GH_TOKEN   the run's GITHUB_TOKEN, with issues: write and actions: read
#   REPO       owner/name
#   RUN_ID     the run to report
#   WORKFLOW   the workflow's name, in the issue title
#   SHA        the commit the run tested
#   SELF_JOB   the reporting job's own name, left out of the failed jobs
#   LABEL      the label that marks these issues (default scheduled-ci)
set -euo pipefail

: "${REPO:?}" "${RUN_ID:?}" "${WORKFLOW:?}" "${SHA:?}"
label=${LABEL:-scheduled-ci}
self=${SELF_JOB:-}
server=${GITHUB_SERVER_URL:-https://github.com}
run_url="$server/$REPO/actions/runs/$RUN_ID"
title="Scheduled $WORKFLOW run failed on main"

# The jobs of this run that failed, timed out or were cancelled, one name a line. A matrix job
# lists each leg, and a reusable workflow's job its caller's name too.
failed=$(gh run view "$RUN_ID" -R "$REPO" --json jobs \
	--jq '.jobs[] | select(.conclusion == "failure" or .conclusion == "timed_out" or .conclusion == "cancelled") | .name' |
	grep -vxF -e "$self" || true)

# The open issue with the label and this exact title, if there is one.
issue=$(gh issue list -R "$REPO" --label "$label" --state open --limit 100 --json number,title \
	--jq ".[] | select(.title == \"$title\") | .number" | head -n 1)

if [ -n "$failed" ]; then
	jobs=$(printf '%s\n' "$failed" | sed 's/^/- /')
	if [ -n "$issue" ]; then
		gh issue comment "$issue" -R "$REPO" --body "Failed again.

Run: $run_url
Commit: $SHA
Failed jobs:
$jobs"
		echo "scheduled-ci-report: commented on issue $issue"
	else
		gh label create "$label" -R "$REPO" --color B60205 \
			--description "A scheduled CI run failed on main" --force >/dev/null
		gh issue create -R "$REPO" --label "$label" --title "$title" --body "A scheduled run of the $WORKFLOW workflow failed on main. A later failure comments here; the next passing scheduled run closes this issue.

Run: $run_url
Commit: $SHA
Failed jobs:
$jobs"
		echo "scheduled-ci-report: opened an issue"
	fi
elif [ -n "$issue" ]; then
	gh issue comment "$issue" -R "$REPO" --body "Passed.

Run: $run_url
Commit: $SHA"
	gh issue close "$issue" -R "$REPO"
	echo "scheduled-ci-report: closed issue $issue"
else
	echo "scheduled-ci-report: the run passed and no issue is open"
fi
