---
paths:
  - "**/*.go"
  - "go.mod"
---

# Go conventions (loaded only when working on Go files)

- **Standard library first.** Flag every new dependency, with its licence, before adding
  it. GPL/AGPL needs sign-off.
- `gofmt` clean; `go vet` clean. `scripts/check.sh` is the gate.
- Wrap errors with context: `fmt.Errorf("start session %s: %w", id, err)`.
- Context first in signatures that do I/O; respect cancellation in the launcher and proxy.
- Table-driven tests. Every authorization check gets a test that asserts it **denies**.
- The Docker backend sits behind the launcher's backend interface (ADR-0003). Nothing
  outside `internal/backend/docker` imports the Docker client.
- Nothing outside the IDE definitions knows about a specific IDE (ADR-0008).
- Never log passwords, tokens or session cookies.
