#!/usr/bin/env bash
# Run any command quietly: one line on success; on failure, the error lines (or the
# tail) plus where the full log is. The full output is always kept in .logs/<label>.log.
#
#   scripts/run-quiet.sh build -- go build ./...
#   scripts/run-quiet.sh smoke -- bash dev/smoke.sh
set -uo pipefail

if [ $# -lt 2 ]; then
  echo "usage: run-quiet.sh <label> -- <command> [args...]" >&2
  exit 2
fi
label=$1
shift
[ "${1:-}" = "--" ] && shift

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel 2>/dev/null || (cd "$(dirname "$0")/.." && pwd))
mkdir -p "$root/.logs"
log="$root/.logs/$label.log"

start=$SECONDS
"$@" >"$log" 2>&1
code=$?
secs=$((SECONDS - start))

if [ $code -eq 0 ]; then
  echo "$label: ok (${secs}s)"
  exit 0
fi

lines=$(wc -l <"$log")
echo "$label: FAILED (exit $code, ${secs}s)"
if [ "$lines" -le 40 ]; then
  cat "$log"
else
  matches=$(grep -n -i -E 'error|fail|panic|undefined|cannot|not found|denied|refused|timeout' "$log" | head -40)
  if [ -n "$matches" ]; then
    echo "$matches"
  else
    tail -30 "$log"
  fi
  echo "… $lines lines in all"
fi
echo "full log: ${log#"$root"/} (grep it; don't cat it)"
exit $code
