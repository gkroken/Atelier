#!/usr/bin/env bash
# The pre-commit gate: format, vet, build, test. One line per step when it passes;
# details only for the steps that fail. Exits non-zero if any step failed.
set -uo pipefail

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel 2>/dev/null || (cd "$(dirname "$0")/.." && pwd))
cd "$root" || exit 2

if [ ! -f go.mod ]; then
  echo "check: no go.mod yet, nothing to check"
  exit 0
fi

fail=0

unformatted=$(gofmt -l $(git ls-files --cached --others --exclude-standard '*.go'))
if [ -n "$unformatted" ]; then
  echo "fmt: FAILED — run gofmt -w on:"
  echo "$unformatted" | head -20
  fail=1
else
  echo "fmt: ok"
fi

scripts/run-quiet.sh vet -- go vet ./... || fail=1
scripts/run-quiet.sh build -- go build ./... || fail=1
scripts/test.sh || fail=1

exit $fail
