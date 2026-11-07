#!/usr/bin/env bash
# Waits until the reviewer's report for a commit exists (code_review/reports/atelier-<sha>.md),
# then prints only its verdict line and its finding headings, not the whole report. Run it in
# the background and get on with reading or planning; it returns when the review is in.
#
#   scripts/wait-review.sh            HEAD
#   scripts/wait-review.sh 37b73c6    a given commit
#
# Gives up after ATELIER_REVIEW_WAIT seconds (default 3 hours), exit 1.
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
sha=${1:-$(git -C "$root" rev-parse --short=7 HEAD)}
sha=${sha:0:7}
report="$root/code_review/reports/atelier-$sha.md"
limit=${ATELIER_REVIEW_WAIT:-10800}
poll=${ATELIER_REVIEW_POLL:-30}
waited=0
until [[ -f $report ]]; do
    (( waited >= limit )) && { echo "no review of atelier-$sha after $waited s"; exit 1; }
    sleep "$poll"; waited=$((waited + poll))
done
sleep 5  # let the reviewer finish writing it
echo "review: ${report#"$root"/}"
grep -a -m1 '^\*\*Verdict' "$report" || echo "(no verdict line)"
grep -a -E '^### ' "$report" || echo "(no findings)"
