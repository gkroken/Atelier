# Atelier — Workplan

A self-hosted replacement for Posit Workbench: browser IDE sessions (RStudio, VS Code,
JupyterLab) for many users on a shared server. Decisions referenced as ADR-NNNN are in
[`docs/DECISIONS.md`](docs/DECISIONS.md) and are locked; a track that finds one wrong
stops and escalates.

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done

**Current status (2026-11-07):** planning done, no code yet. **NEXT = spine #0** (startup
spike).

---

## 1. Definition of Done for v1

v1 is reached when **all** of the following hold:

- A user signs in with their Windows username and password (PAM), or through OIDC when
  that is switched on, and lands on a home page listing their sessions.
- They start an RStudio, VS Code or JupyterLab session from the New Session dialog,
  choosing an image and a resource profile, and it opens in **under 6 s** from an image
  already on the host. Median measured over 20 starts.
- Every session runs as the user's real account ID, sees only that user's home folder, and
  a file written in one session appears in another of theirs without a reload.
- A user runs at least three sessions at once, closes the browser, and rejoins them all
  from another device.
- An idle session is suspended after the admin-set timeout, frees its memory, and resumes
  with its files intact.
- An admin sees every session (user, image, CPU/RAM, age), can suspend or kill any of
  them, manages images and profiles, and a user cannot exceed their profile or their
  session cap.
- An end-to-end smoke script passes for two users and asserts the **deny** cases: user A
  cannot reach user B's session URL, home folder or session list.

## 2. Guiding principles

1. **Build the irreversible things first.** Session identity and URL shape, how account
   IDs are resolved, the home-folder mount layout and the database schema are expensive
   to change once people have sessions. The UI is cheap to change.
2. **Measure startup, don't assume it.** The timings in
   `docs/research/workbench.md` are estimates. Spine #0 measures them; every later change
   that touches the start path re-measures.
3. **One backend interface.** The launcher talks to Docker through an interface narrow
   enough that a Kubernetes backend is an added implementation, not a rewrite (ADR-0003).
4. **No runtime dependency on forge or Skald** in v1 (ADR-0011).
5. **Deny cases get tests.** Every authorization decision has a test that asserts it
   refuses.

## 3. v1 scope

**In:**

- Home page: running and suspended sessions; start, rejoin, suspend, quit; several at once.
- New Session dialog: IDE, image, resource profile, optional name and working folder.
- Sessions survive the browser closing.
- Suspend idle sessions after an admin-set timeout; one click resumes.
- Admin dashboard: all sessions, kill/suspend, images, profiles.
- Limits: resource profiles granted per AD group, and a session cap per user.
- Image management: register an image, pull it when published, retire it without
  breaking running sessions.
- Sign-in: PAM and OIDC (ADR-0005).

**Later (not v1):**

- Publish to Skald from a session
- Background jobs (like Workbench Jobs)
- SSH into sessions, for Positron Desktop or VS Code Desktop (ADR-0012)
- Kubernetes backend and multiple hosts
- A pool of pre-started containers, only if measured startup times turn out to bother people
- Shared project folders, Git credential helper
- Our own VS Code extension with Positron-style panes (ADR-0012)

## 4. Session lifecycle

```
Start (pre-pulled image)                                             about 3–5 s total
  Click ─► Create container ─► Start + mount home ─► IDE boots ─► Browser loads
   0 s        +0.3 s              +0.5 s              +1–2.5 s       +1–1.5 s

States
  Running ──idle timeout──► Suspended ──quit──► Removed
     ▲                         │                   ▲
     └────────resume───────────┘                   │
     └──────────────quit, or admin kills it────────┘
```

- **Suspended** = container stopped and kept; memory freed. RStudio saves the R
  workspace to disk first. code-server and JupyterLab come back with files and settings,
  but anything held only in memory is gone.
- **Removed** = container deleted. Files in the home folder stay. `apt` installs made
  inside a session are lost; R and Python packages in the home folder stay, kept in one
  library folder per image so a package built for one image cannot break another.

## 5. Spine

Dependency-ordered. Each item lists what "done" means.

- [ ] **#0 Startup spike.** Go launcher starts an RStudio container as a given account ID
  with that user's home bind-mounted; reverse proxy at `/s/<id>/`, websockets included.
  *Done:* click-to-IDE time measured (20 starts, median and p95) and recorded here; a file
  written in a second container appears in the first; sessions join the ICC-off session
  network (ADR-0015) and a live test shows one session can't connect to another.
- [ ] **#1 Same spike for code-server and JupyterLab.** *Done:* all three IDEs behind the
  proxy with no IDE-specific hacks outside one per-IDE definition file (ADR-0008).
- [ ] **#2 Identity.** PAM sign-in, account ID and groups from the OS (`getpwnam`,
  `getgrouplist`); OIDC sign-in mapping `preferred_username` to the same lookup.
  *Done:* tests for unknown user, disabled user, wrong password, user without an account
  ID; user A denied user B's session. Host firewall rules for the session network
  (ADR-0015 table): only the Atelier process can open connections to sessions, and
  sessions can't open connections to the host or to other containers' published ports.
  Live deny tests: from another host account to a session, from a session to a port
  published on the host, and from a session to a service on the host's 0.0.0.0.
  Main-host endpoints refuse cross-origin writes (ADR-0016 amendment): a POST with a
  valid cookie and `Origin: https://<sessions host>` or `Sec-Fetch-Site: same-site` is
  refused, with a test.
- [ ] **#3 Schema.** PostgreSQL schema for users, sessions, images, profiles, grants;
  migrations. *Done:* migrations run forward from empty; schema reviewed before anything
  depends on it.
- [ ] **#4 Home page and New Session dialog.**
- [ ] **#5 Suspend and resume**, idle detection per IDE. *Done:* a suspend hook in each
  definition saves the IDE's state before the container stops, and Stop doesn't wait out
  its timeout (today rserver as PID 1 ignores SIGTERM: Stop(10 s) ends in SIGKILL with no
  workspace saved, atelier-10f7686-N1); a live test suspends and resumes RStudio with the
  workspace intact.
- [ ] **#6 Limits.** Profiles per group, session cap per user, enforced in the launcher.
- [ ] **#7 Admin dashboard and image management**, including pull-on-publish.
- [ ] **#8 Smoke script** for the Definition of Done, two users, deny cases.

## 6. Non-goals for v1

Kubernetes, multi-host, HA; browser Positron (ADR-0012); Workbench API compatibility;
background jobs; any forge or Skald integration.

## 7. Open questions

- [ ] Confirm the Workbench server is joined to AD through SSSD (`realm list`,
  `id <user>`, `/etc/pam.d/rstudio`), and that a new host would map users to the same IDs.
- [ ] How many users and concurrent sessions, and how big the host is (CPU/RAM/disk).
- [ ] Do homes stay on the Workbench server's disk, or move to an NFS share?
- [ ] TLS: does Atelier handle it itself, or sit behind an existing reverse proxy? (If
  behind one, the proxy needs a trusted-front-proxy setting for X-Forwarded-Proto,
  atelier-fa69e6c-N4.)
- [ ] Confirm ADR-0016 (proposed): sessions on their own hostname, separate from
  Atelier's own pages, and choose a subdomain (`sessions.atelier.example.org`, relies on
  the CSRF check alone) or a separate domain (`atelier-sessions.example.net`, also
  cross-site for SameSite).
- [ ] Who builds session images, and where: plain Dockerfiles in this repo, or Skald's
  build sandbox?
- [ ] Migration: move users over one at a time, or switch everyone on a set day?
- [ ] Check that the name Atelier is free on GitHub and as a container image name.
- [ ] License for the repository.
