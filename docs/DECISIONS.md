# Decisions

Lightweight ADRs. One per decision that would be expensive or embarrassing to reverse.
Newest last. A decision recorded here is **locked** — a track that finds it wrong stops
and escalates rather than working around it. All decisions below were taken in the
planning Q&A on 2026-11-07.

---

## ADR-0001 — Build the platform before any custom IDE

**Date:** 2026-11-07 · **Status:** accepted

The goal is to switch Workbench off. RStudio Server (open source) covers R users and
code-server covers VS Code users from day one, so the session platform is what unblocks
that. A Positron-like VS Code extension (ADR-0012) makes the platform nicer, but doesn't
replace anything.

**Rejected:** building the extension first. It is useful on its own but moves nobody off
Workbench.

---

## ADR-0002 — Go single binary with its own launcher and reverse proxy

**Date:** 2026-11-07 · **Status:** accepted

The Workbench-like parts — the home page, the New Session dialog, suspend and resume,
several sessions per user — are the real work, and they need full control over the session
model. A Go binary (same toolchain as forge) gives that.

**Rejected:** building on ContainerProxy like Skald. It would reuse Skald's backends and
auth, but ShinyProxy's one-app-per-user model would have to be bent around a
multi-session home page.

---

## ADR-0003 — One container per session on a single Docker host; Kubernetes later

**Date:** 2026-11-07 · **Status:** accepted

Workbench's Local mode is fast (about 2–3 s) because a session is a process forked as the
user, with R already on the host. A container started from an image already on the host
costs about 1–2 s more (about 3–5 s) and buys isolation, per-session CPU/RAM caps, one
image per R/Python version, and a clean host. See `docs/research/workbench.md` for the
timings and the alternatives.

The launcher talks to Docker through a narrow backend interface, so Kubernetes is an added
implementation later.

**Rejected:** host processes as Linux users (needs root, PAM and SSSD, and every R
version installed on the host; weak isolation); Kubernetes from day one (heavy, and pod
scheduling plus NFS attach add seconds); a warm pool of pre-started containers (saves
about 1 s but holds idle RAM — revisit only if measured startup times turn out to bother
people).

---

## ADR-0004 — Images are pulled when published, never on click

**Date:** 2026-11-07 · **Status:** accepted

A cold pull of a 2–5 GB IDE image takes 30–180 s, and it is the only step that makes a
container start slow. Publishing an image pulls it onto the host; a session can only be
started from an image that is already present. Retiring an image hides it from the New
Session dialog without touching sessions still running on it.

---

## ADR-0005 — Sign-in through PAM and OIDC; account IDs and groups from the OS

**Date:** 2026-11-07 · **Status:** accepted

Today's Workbench login is PAM: Windows username and password, checked by the host
(almost certainly joined to AD through SSSD). Atelier does the same, and also offers OIDC;
the admin switches on either or both. Either way, once a username is known, the account
ID and groups come from the OS (`getpwnam`, `getgrouplist`), so there are no LDAP
settings in Atelier and file ownership matches the existing home folders.

OIDC maps the `preferred_username` claim to the OS lookup. forge's OIDC code is the
reference.

**Constraint:** a newly joined host must map users to the same account IDs as the current
Workbench server (copy its SSSD ID-mapping config), or existing home folders will look
like someone else's.

---

## ADR-0006 — Home folders on the host or NFS, bind-mounted; only your own

**Date:** 2026-11-07 · **Status:** accepted

`/home/<user>` (local disk or an NFS mount on the host) is bind-mounted into each of that
user's sessions, and nothing else from other users is mounted. This is stricter than
Workbench, where other homes are on disk but blocked by file permissions.

**Rejected:** per-user Docker volumes (hard to back up or reach outside sessions).

---

## ADR-0007 — All of a user's sessions share one home, one account ID and one host

**Date:** 2026-11-07 · **Status:** accepted

This is what lets someone manage files and git in VS Code while running R in RStudio
on the same files, live. RStudio's file pane watches the folder through inotify, which
sees changes from other containers on the same kernel. On different hosts that would
stop working, so when multi-host arrives, a user's sessions are kept on one host.

---

## ADR-0008 — IDEs are definitions, not code paths

**Date:** 2026-11-07 · **Status:** accepted

v1 ships RStudio Server (open source), code-server and JupyterLab. Each IDE is a
definition: image, start command, port, health check, idle probe and URL-prefix handling.
The launcher and proxy know nothing IDE-specific beyond that file. This also leaves a slot
where a customer with their own licence could add browser Positron (ADR-0012).

One rserver per container gets around open-source RStudio's limit of one session per
user per server.

---

## ADR-0009 — PostgreSQL from day one

**Date:** 2026-11-07 · **Status:** accepted

Same as Skald. One more service to run, but no migration later when Atelier goes
multi-host or HA.

**Rejected:** SQLite first, Postgres optional (forge's eval-mode approach).

---

## ADR-0010 — Suspend means stop the container and keep it

**Date:** 2026-11-07 · **Status:** accepted

After an admin-set idle time the container is stopped (memory freed) and kept; resume
starts it again, so a resume costs about the same as a fresh start. RStudio saves its
workspace on suspend.

**Rejected:** `docker pause` (instant resume but frees no RAM); pause then suspend
(better feel, more logic — possible later).

---

## ADR-0011 — No forge or Skald integration in v1

**Date:** 2026-11-07 · **Status:** accepted

Integrations add dependencies. Users choose package sources in their own `.Rprofile` or
`pip.conf`; an image may ship a default in `Rprofile.site` / `pip.conf`, which is a file
in the image, not an integration. Publish to Skald comes later.

---

## ADR-0012 — No browser Positron; two later paths instead

**Date:** 2026-11-07 · **Status:** accepted

Positron Desktop is free for commercial use. Browser Positron is not available to us:
the server build is sold only as part of Workbench (or licensed free for coursework). It
checks a licence file, and the Elastic License 2.0 forbids getting around licence-key
checks. Details and sources are in `docs/research/positron.md`.

What the user values in Positron is covered in v1 by ADR-0007: VS Code for files and git,
plus one or more RStudio sessions on the same home folder.

Later paths, both outside v1:

1. **SSH into sessions**, so Positron Desktop's Remote SSH can connect. Confirm with Posit
   before advertising it.
2. **Our own VS Code extension** — console, variables, plots and viewer panes on top of
   `ark` (Positron's R kernel, MIT licence) and ipykernel. Roughly 2–3 months to an MVP.
   Do not copy `kallichore` or Positron's Python kernel code: both are under the Elastic
   licence.

---

## ADR-0013 — Resource profiles per AD group, plus a session cap per user

**Date:** 2026-11-07 · **Status:** accepted

Admins define profiles (for example Small 2 CPU / 8 GB, Large 8 CPU / 64 GB) and grant
them to AD groups; each user also has a cap on concurrent sessions.

**Rejected:** free choice under a per-user total (harder to reason about); one size for
everyone.

---

## ADR-0014 — Admin-curated images

**Date:** 2026-11-07 · **Status:** accepted

Admins publish images such as "R 4.5 + Python 3.12"; users pick one in the New Session
dialog. Built from rocker / python-slim bases like Skald's.

**Rejected:** picking R and Python separately (many more images); users bring their own
image (hard to support).

---

## ADR-0015 — Sessions share one bridge network with container-to-container traffic off

**Date:** 2026-11-07 · **Status:** accepted

IDEs run with their own login off (`--auth-none` and the like); the proxy's ownership
check is the only gate. On Docker's default bridge, any session can open any other
session's port directly, so user A's terminal could reach user B's IDE without going
through the proxy (atelier-4ceb450-F3). The Atelier binary runs on the host (ADR-0002) and
reaches containers by their bridge IP, so the host must keep that access.

**Decision:** every session container joins one user-defined bridge network,
`atelier-sessions`, created with `com.docker.network.bridge.enable_icc=false`. The Docker
backend creates it if missing and refuses to start sessions on an existing network with
that name that is not an ICC-off bridge. No session ports are published on the host. A
live deny test checks that one session cannot connect to another while the host can.
Raw-socket tricks that get around ICC need CAP_NET_RAW, which sessions don't have (all
capabilities are dropped).

**Not covered yet:** other accounts with a shell on the host can still reach container
IPs directly. Closing that (a host firewall rule letting only the Atelier process reach
the session network) is part of spine #2.

**Rejected:** one network per session (isolates too, but every network takes an address
pool, and Docker's default pools hold about 30 bridge networks; more moving parts on every start
and stop); host firewall rules alone (Atelier would be managing iptables for the
container-to-container case Docker already handles); the default bridge with daemon-wide
`icc=false` (changes every other container on the host, and needs a daemon config change).

**Amended 2026-11-07 — every network path a session has, and which are allowed**
(atelier-4e18c55-F1). The ICC-off bridge closes only the first row. Any new path into or
out of a session must be added to this table, with the test that holds it:

| Path | Allowed? | Held by | Status |
|---|---|---|---|
| session -> another session | no | ICC-off bridge | done, live deny test |
| session -> the host's own services (gateway IP, anything listening on 0.0.0.0: PostgreSQL, Atelier's own ports, a TCP Docker API) | no | host firewall: drop new connections from the session bridge to the host | spine #2 |
| session -> other containers' published ports (through the gateway) | no | same host firewall rule | spine #2 |
| session -> internet and the LAN | yes (package installs, git, data sources, as on Workbench today) | | admins may narrow later |
| Atelier process -> session | yes (the proxy dials container IPs) | replies to it pass the firewall as established traffic | done |
| other host accounts -> session | no | host firewall: only Atelier's account may open connections to the session bridge | spine #2 |
