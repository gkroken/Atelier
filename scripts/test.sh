#!/usr/bin/env bash
# Run `go test` and print only what matters: one summary line when everything passes,
# or the failing tests (with their own output) and build errors when something fails.
# The full machine-readable log is kept in .logs/test.jsonl for grepping.
#
#   scripts/test.sh                    # all packages
#   scripts/test.sh ./internal/proxy/  # one package
#   scripts/test.sh -run TestResume ./internal/launcher/
set -uo pipefail

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel 2>/dev/null || (cd "$(dirname "$0")/.." && pwd))
cd "$root" || exit 2

if [ ! -f go.mod ]; then
  echo "test: no go.mod yet, nothing to test"
  exit 0
fi

mkdir -p .logs
log=.logs/test.jsonl
err=.logs/test.stderr

args=("$@")
# Default to every package unless a package pattern was given.
has_pkg=0
for a in "${args[@]}"; do
  case $a in ./*|.) has_pkg=1 ;; esac
done
[ $has_pkg -eq 0 ] && args+=(./...)

start=$SECONDS
go test -json "${args[@]}" >"$log" 2>"$err"
code=$?

python3 -I "$root/scripts/lib/gotest_summary.py" "$log" "$err" "$code" "$((SECONDS - start))"
exit $code
