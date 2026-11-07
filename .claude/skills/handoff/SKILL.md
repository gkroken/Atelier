---
name: handoff
description: Write a short session handoff before /clear or at the end of a work block, so the next session starts from a 40-line note instead of re-reading the repo. Use when the user says "wrap up", "hand off", "I'm clearing", or a task switches.
---

# Session handoff

Context is expensive; a fresh session that reads a good note is cheap. Write
`code_review/HANDOFF.md` (local, gitignored, read by the coder only, never the reviewer),
**at most 40 lines**, then tell the user they can `/clear` and start the next session
with: `Read code_review/HANDOFF.md and continue.` Replace the old note; keep only lessons
that still apply.

```markdown
# Handoff — <date> — <one-line goal>

## Where it stopped (read this first)
- Last commit: <sha> <subject> — review: <verdict, or "waiting">
- Open findings to fix: <ids from code_review/LEDGER.md, or none>
- Stashed: <git stash list entries, or none>
- Checks: <output line of scripts/check.sh>

## Done this session
- <change> (<file:line>)

## In progress
- <what>, stopped at <exact point>

## Decided (and why, one line each)
- ...

## Next step
1. <the very next concrete action, with the file to open>

## Lessons
- <thing that cost time and would again>
```

Rules:

- Facts the next session needs, not a narrative of this one.
- File paths with line numbers instead of descriptions of code.
- Anything that should outlive the handoff goes elsewhere: a decision → an ADR
  (`adr` skill), a plan change → `WORKPLAN.md`, a recurring gotcha → `CLAUDE.md` or a
  `.claude/rules/` file.
- Commit the work before writing the handoff, so "last commit" is real.
