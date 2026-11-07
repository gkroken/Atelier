# CLAUDE.md

Guidance for Claude Code in this repository. Kept short on purpose: it loads into every
session. Task-specific detail lives in skills (`.claude/skills/`) and path-scoped rules
(`.claude/rules/`), which load only when needed.

<!-- Maintainers: block HTML comments like this one are stripped before Claude sees the
file, so notes here cost no tokens. Keep this file under ~100 lines. -->

## What this is

Atelier: self-hosted browser IDE sessions (RStudio, VS Code, JupyterLab) for many users
on one server, replacing Posit Workbench. Go single binary + PostgreSQL + one Docker
container per session. Sibling projects: forge (packages), Skald (publishing).

The coder and reviewer workflow is in `code_review/` (local, not committed): read
`code_review/CODER.md` or `REVIEWER.md` at the start of a session, as your role says.
Committing goes through the `commit-for-review` skill.

**Read first, only what the task needs:**

- `WORKPLAN.md` — Definition of Done, spine, open questions. Status line at the top.
- `docs/DECISIONS.md` — locked ADRs. Use the `adr` skill; don't read it whole.
- `docs/research/` — background (Workbench, Positron). Rarely needed.

## Commands

All checks print one line on success and only failures otherwise; full logs go to
`.logs/`. See the `run-checks` skill.

```bash
scripts/check.sh                    # fmt + vet + build + test: the pre-commit gate
scripts/test.sh [-run X] [./pkg/]   # tests, failures only
scripts/run-quiet.sh <label> -- <cmd>   # any other noisy command
scripts/wait-review.sh [sha]        # in the background: verdict + finding headings only
```

## Keeping context small (follow these every session)

- Run checks through the wrappers above, never raw `go test -v`. When something fails,
  **grep the log in `.logs/`**; never cat it, and never rerun a whole suite just to see an
  error. Rerun only the failing test.
- Read files with offset/limit, or grep first. Don't read a whole large file to find one
  function.
- Broad searches across many files: use the `scout` agent (read-only, Haiku, ≤15
  bullets back) instead of reading results into this session.
- Never reprint a whole file. Show only changed functions or a diff.
- Before `/clear` or a task switch, use the `handoff` skill.
- Ask for counts or filenames (`grep -c`, `grep -l`) before matching lines.

## Working conventions

- For changes over ~30 lines or spanning multiple files, state the plan and wait for
  confirmation before writing code.
- Keep `scripts/check.sh` green at all times.
- **Commit after each self-contained unit of work**, through the `commit-for-review`
  skill. Message format: `<scope>: <what and why>` (e.g. `launcher: stop container on
  idle timeout`). Never amend or force-push; fix forward, citing finding IDs.
- **Ask before** changing: authentication, the database schema once it ships, the
  session URL shape, or anything security-sensitive.
- If `WORKPLAN.md` or an ADR conflicts with what the code does, stop and report it.

## Architecture invariants

- Sessions run as the user's real account ID, resolved from the OS after PAM or OIDC
  sign-in (ADR-0005). Never as root, never as a shared user.
- A session container mounts **only** its owner's home folder (ADR-0006).
- All of one user's sessions share one home, one account ID and one host (ADR-0007).
- The launcher reaches Docker only through the backend interface (ADR-0003).
- IDEs are definitions; no IDE-specific code outside them (ADR-0008).
- Images are pulled when published, never when a session starts (ADR-0004).

## Security invariants

- Every authorization decision goes through one place, and deny cases get tests: user A
  can never reach user B's session URL, home folder or session list.
- Strip client-supplied identity headers before the proxy injects its own.
- Never log passwords, tokens or session cookies.
- The Docker socket is root-equivalent: only the launcher touches it.

# Compact instructions

When compacting, keep: the current spine item and its done-criteria, files changed and
why, the failing check (its summary line and test name), decisions made this session,
and the next step. Drop passing check output and file contents that are on disk.
