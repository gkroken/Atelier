# Keeping Claude Code's token use down

Researched 2026-11-07 from Anthropic's docs and what users report on forums. What this
repo does about each point is in the right-hand column.

| Practice | Why it helps | In this repo |
|---|---|---|
| **Filter tool output before Claude sees it** | Passing tests and verbose logs are the biggest waste; one user cut `cargo test` from 155 lines to 3 and reports 89% fewer tokens over two weeks | `scripts/test.sh`, `scripts/run-quiet.sh`, `scripts/check.sh`: one line on success, failures only |
| **Keep the full log on disk, print a pointer** | Avoids rerunning a suite because `tail` cut off the error | Every wrapper writes `.logs/<label>.log` and ends with `full log: …` |
| **PreToolUse hook that rewrites test commands** | Anthropic's cost guide gives this exact pattern: rewrite `go test` etc. to show only failures | `.claude/hooks/quiet-go.sh` (install below) |
| **Short CLAUDE.md (< 200 lines)** | It loads into every request | `CLAUDE.md` ~90 lines; maintainer notes in stripped HTML comments |
| **Move workflow detail into skills** | Only a skill's name and description load until it's used | `run-checks`, `handoff`, `adr` skills |
| **Path-scoped rules** | `.claude/rules/*.md` with `paths:` frontmatter load only when matching files are touched | `.claude/rules/go.md` |
| **Delegate searches and verbose work to subagents on a small model** | The noisy output stays in the subagent; only its summary comes back | `scout` agent (Haiku, read-only, ≤15 bullets) |
| **Handoff file, then `/clear`** | A fresh session reading 40 lines beats a long session re-reading history on every request | `handoff` skill → `handover/session.md` |
| **Custom compact instructions** | `/compact` keeps what you name instead of a generic summary | `# Compact instructions` section in `CLAUDE.md` |
| **Specific prompts, plan mode for big changes** | Vague asks trigger broad scanning; plan mode avoids rework | Working conventions in `CLAUDE.md` |
| **Prefer CLIs over MCP servers; disable unused servers** | CLIs add no per-tool listing | — (habit) |
| **Right model and effort for the job** | `/model`, `/effort`; Haiku for subagents doing simple work | `scout` runs on Haiku |
| **Watch it** | `/usage` and `/context` show where tokens go; `/usage` on paid plans suggests fixes | — (habit) |

## Installing the hook

The hook is switched on by `.claude/settings.json`. That file changes how Claude Code
behaves in this repo, so it is shipped as an example for you to review and copy rather
than committed directly:

```bash
cp docs/claude/settings.example.json .claude/settings.json
```

Check it with `/hooks` (it should be listed under PreToolUse). The hook needs `jq`. It
only rewrites simple commands (`go test ./...`, `go vet ./...`, `go build ./...`) with no
pipes, redirects or `&&`; anything else passes through untouched. `ATELIER_RAW=1 go test
-v ./...` bypasses it on purpose.

## Considered, not adopted

- **rtk ("Rust Token Killer")**, a CLI proxy that compresses output for many tools.
  Reported to work well, but it is a third-party binary sitting in front of every
  command; review it before installing. Users also report it conflicting with other
  PreToolUse hooks. Our wrappers cover the Go toolchain without it.
- **`--quiet` flags everywhere.** Commenters warn they hide too much; the wrappers keep
  everything in the log and show the failures.

## Sources

- [Anthropic: Manage costs effectively](https://code.claude.com/docs/en/costs) — hooks that
  filter test output, CLAUDE.md under 200 lines, skills, subagents, compaction
- [Anthropic: Hooks reference](https://code.claude.com/docs/en/hooks) — `updatedInput`,
  `additionalContext`, `${CLAUDE_PROJECT_DIR}`
- [Anthropic: How Claude remembers your project](https://code.claude.com/docs/en/memory) —
  path-scoped rules, stripped HTML comments
- [r/ClaudeAI: "I saved 10M tokens (89%) … with a CLI proxy"](https://reddit.sentinel-team.org/posts/1r2tt7q/snapshots/2026-02-13T17%3A14%3A42.327723Z)
  — log-to-file with a pointer, quiet Makefile targets, hook conflicts
- [9 ways to cut token consumption in Claude Code](https://dev.to/developer_harsh/9-ways-to-cut-token-consumption-in-claude-code-3jpd)
  — grep-filtered test output, handoff files, bounded subagents, skills
- [Stop blowing your context window](https://agenticai.nuilab.org/guides/context-optimization/)
  — logs to files, `.claude/rules/`, counts before content
