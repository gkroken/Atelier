# Atelier

Self-hosted browser IDE sessions for R and Python: RStudio, VS Code and JupyterLab,
several at once per user, on a shared server. A replacement for Posit Workbench, built
alongside [forge](https://github.com/gkroken/forge) (replaces Package Manager) and
[Skald](https://github.com/gkroken/Skald) (replaces Connect).

**Status: planning. No code yet.** The plan is in [`WORKPLAN.md`](WORKPLAN.md) and the
locked decisions, with their reasons, are in [`docs/DECISIONS.md`](docs/DECISIONS.md).

## How it works

- **One Go binary.** It handles sign-in (PAM or OIDC), launches sessions, proxies each
  session at `/s/<id>/`, and serves the home page and admin dashboard. Its state lives in
  PostgreSQL.
- **Every session is its own container** on a single Docker host. Images are pulled
  onto the host when an admin publishes them, so starting a session takes about 3–5
  seconds and never waits on a download.
- **Sessions run as you.** The container runs under your real Linux account ID (from
  the host's AD/SSSD), and only your home folder is mounted into it. All your sessions
  share that home, so a file saved in VS Code shows up at once in your RStudio session.
- **Idle sessions are suspended.** After an admin-set idle time the container is
  stopped to free memory, and it restarts when you resume. Your files are untouched.

```
Browser ──► Atelier (Go) ──► Docker host
             │  sign-in: PAM / OIDC, IDs from the OS      ┌──────────┬──────────┬──────────┐
             │  launcher · proxy /s/<id>/ · UI            │ RStudio  │ VS Code  │ Jupyter  │
             ▼                                            └────┬─────┴────┬─────┴────┬─────┘
          PostgreSQL                                           └── /home/<user> (host or NFS)
```

## Try it on a development machine

Needs Go, gcc, `libpam0g-dev` and Docker (your account in the `docker` group).

```bash
go build -o atelier ./cmd/atelier
./atelier -config deploy/config/atelier.dev.yaml
```

Open <http://localhost:8080/> and sign in as your own account with the password
`atelier-dev` (development sign-in: no PAM, no real password; refused beyond
loopback). Sessions open on `sessions.localhost:8080`, which browsers send to this
machine without a DNS entry. A session mounts your real home folder. For a server,
start from [`deploy/config/atelier.example.yaml`](deploy/config/atelier.example.yaml).

## Documentation

| | |
|---|---|
| [`WORKPLAN.md`](WORKPLAN.md) | Scope, v1 features, spine of work items, open questions |
| [`docs/DECISIONS.md`](docs/DECISIONS.md) | Architecture decisions and why |
| [`docs/research/workbench.md`](docs/research/workbench.md) | How Workbench works, and session startup timings |
| [`docs/research/positron.md`](docs/research/positron.md) | Positron licensing and the paths we have |
| [`CLAUDE.md`](CLAUDE.md) | Guidance for Claude Code in this repo |

## License

Not chosen yet.
