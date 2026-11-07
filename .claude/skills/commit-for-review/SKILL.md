---
name: commit-for-review
description: Commit one finished unit of work in Atelier, then wait for the reviewer's verdict before starting anything else. Use whenever a change is ready to commit.
---

# Commit, then wait for the review

1. **Run the gate:** `scripts/check.sh`. Every step must say `ok`.
   - Never weaken or skip a test to make it pass.
2. **Commit one logical change.** The code, its test and its docs go in the same commit.
   - Commit as `git -c user.name=gard1 -c user.email=89836666+gkroken@users.noreply.github.com commit -F <msgfile>`. Write the message file in the scratchpad.
   - Message: `<scope>: <what and why>`. Scopes: launcher, proxy, auth, backend, ide, config, db, ui, admin, scripts, docs, plan, test.
   - Every claim in the message must be true, and must say how it was tested ("tested: scripts/check.sh + live start of an RStudio session", "not yet tried against a real Docker host").
   - A fix cites its finding IDs, for example `(atelier-d34a1a6-N1)`.
   - A change to something an ADR locks also updates `docs/DECISIONS.md` (the `adr` skill) in the same commit.
   - End the message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
   - Never amend, rebase or force-push. Never push without the user's OK.
3. **Leave the working tree clean.** The reviewer builds this same checkout.
   - `git status --short` must be empty. Stash any unfinished work.
4. **Wait for the review.** Run `scripts/wait-review.sh` in the background.
   - It prints only the verdict and the finding headings when the report arrives.
   - While waiting, only read and plan. Change no file.
5. **On the verdict:**
   - **PASS with no findings:** go on.
   - **Findings:** read just that report's findings section (grep for the `### ` heading, then read with an offset). Fix forward in a new commit that cites the IDs.
   - **CHANGES_REQUIRED:** fix it before any new work.

Security-sensitive commits — authentication, authorization, the proxy's identity headers,
the Docker backend, anything that mounts a path — always go through review, even when the
user has said to skip it for routine work.
