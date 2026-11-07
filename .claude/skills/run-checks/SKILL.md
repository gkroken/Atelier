---
name: run-checks
description: How to run tests, vet, build and smoke scripts in Atelier without flooding context — the quiet wrappers, where full logs live, and how to dig into one failure. Use before running any test, build or long script, and whenever a check fails.
---

# Running checks quietly

Passing output is noise. Every check here prints **one line when it passes** and only
the failures when it doesn't. The full output is always on disk in `.logs/`.

| Want | Run | Prints on success |
|---|---|---|
| Everything before a commit | `scripts/check.sh` (or `make check`) | 4 lines: fmt, vet, build, test |
| All tests | `scripts/test.sh` | `test: ok — N passed, …` |
| One package | `scripts/test.sh ./internal/launcher/` | one line |
| One test | `scripts/test.sh -run 'TestResume$' ./internal/launcher/` | one line |
| Any other command | `scripts/run-quiet.sh <label> -- <cmd>` | `<label>: ok (Ns)` |

Package paths are relative to the repo root (the scripts `cd` there first). A bare
`go test`, `go vet` or `go build` is rewritten to these wrappers by
`.claude/hooks/quiet-go.sh` when the hook is installed. Prefix `ATELIER_RAW=1` only when
you need raw output, and say why.

## When something fails

1. Read the summary. It already holds each failing test's own output (30 lines max per
   test) and any build errors.
2. Need more? **Grep the log, don't cat it:**
   - `grep -n 'TestResume' .logs/test.jsonl | head -20`
   - `grep -n -i 'panic\|race' .logs/<label>.log | head`
   - `sed -n '120,160p' .logs/<label>.log` for a known region
3. Rerun **only the failing test** with `-run '^TestName$'`, not the suite.
4. Never pipe a check through `tail` and then rerun it because the tail hid the error.
   The log already has it.

## Long or noisy work

- A script whose output you only need a verdict from (a smoke run, a load test, a
  multi-log diagnosis): run it through `run-quiet.sh`, or hand it to a subagent and ask
  for at most 10 lines back.
- Background a long server with `run_in_background` and read its log with `grep`, not
  by streaming it.
- Asking for counts or filenames first (`grep -c`, `grep -l`) is cheaper than asking for
  matching lines.
