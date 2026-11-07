---
name: adr
description: Add or amend an architecture decision in docs/DECISIONS.md without reading the whole file. Use when a choice is made that would be expensive to reverse, when the user says "record this decision", or when work contradicts an existing ADR.
---

# Recording a decision

`docs/DECISIONS.md` grows long. Don't read it whole.

1. Find the last number and the titles:
   `grep -n '^## ADR-' docs/DECISIONS.md`
2. To check an existing decision, read only its section:
   `sed -n '/^## ADR-0005/,/^---$/p' docs/DECISIONS.md`
3. Append a new one at the end, in this shape:

```markdown
---

## ADR-NNNN — <decision as a short sentence>

**Date:** YYYY-MM-DD · **Status:** accepted

<Why: the problem and the deciding facts, 2–5 sentences.>

**Decision:** <what we do>.

**Rejected:** <alternative> (<why, one line>).
```

4. Amending: never rewrite history. Add an `**Amended YYYY-MM-DD — <what>.**` paragraph
   under the original, or mark it `**Status:** superseded by ADR-NNNN` and add the new
   one.
5. If the change affects scope or the spine, update `WORKPLAN.md` in the same commit.

An ADR is **locked**: work that finds one wrong stops and asks the user rather than
working around it.
