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

**Amended 2026-11-07 — two more rows** (atelier-54063f0-N1):

| Path | Allowed? | Held by | Status |
|---|---|---|---|
| session -> DNS (Docker's embedded resolver, forwarding to the host's) | yes | the spine #2 firewall rule must keep it working; its live test resolves a public name from a session | spine #2 |
| session -> link-local metadata (169.254.169.254) | no | host firewall rule | n/a on the on-prem host; required before Atelier runs on any cloud VM |

**Amended 2026-11-07 — confirmed by the user.** The decision above was first taken by the
coder under the user's instruction to follow its recommendation while the user was away
(atelier-4e18c55-Q1); the user has since confirmed it.

---

## ADR-0016 — Session URLs are /s/<id>/ on a hostname of their own

**Date:** 2026-11-07 · **Status:** accepted 2026-11-07, with a subdomain (see the last amendment)

The session URL is what people bookmark and keep in open tabs, so it is expensive to
change. The ID part was agreed with the user: 128 random bits as 26 lowercase base32
characters (`internal/session`), so `/s/k3j9…/`. An ID is unguessable but not a
credential; every request is still checked against the session's owner.

The host part matters for security. A session serves whatever HTML and JavaScript its
user's code produces (htmlwidgets, reports, a package's viewer output). If that shares an
origin with Atelier's own pages, it can call Atelier's API as that user, and as an admin
when the user is one (publish an image every user then runs). JupyterHub's security
guidance says the same: don't serve user content on the hub's origin. Users can't reach
each other's sessions either way, because the proxy's Resolver refuses them.

**Decision:** sessions are served at `https://<sessions host>/s/<id>/`, and the sessions
host is a different hostname from the one serving sign-in, the home page and the admin
dashboard (for example `atelier.example.org` and `sessions.atelier.example.org`). Atelier's
cookies are host-only `__Host-` cookies with SameSite=Lax or Strict, so neither host's
cookie is sent to the other. Signing in to the sessions host is a redirect from the main
host with a single-use token (spine #2). A single hostname for both is allowed only in
development, with a warning in the log.

**Consequences:** one extra DNS name and TLS certificate (not a wildcard). Session-to-
session separation between a user's own sessions isn't needed (same user). If sharing a
session with another user ever arrives, per-session subdomains (`<id>.sessions...`, which
needs a wildcard) come back up, as JupyterHub requires for sharing.

**Rejected:** one origin for everything, as Workbench does (session content gets the
user's Atelier rights, including admin rights); per-session subdomains now (a wildcard
DNS name and certificate for a protection v1 doesn't need).

**Amended 2026-11-07 — separate hosts are not enough on their own** (atelier-572d510-F1,
N1). The text above overstates it. A sessions host that is a subdomain of the same
registrable domain (`sessions.atelier.example.org` next to `atelier.example.org`) is the
**same site** to browsers. A script or form on the sessions host can then send a POST to
the main host, and the browser attaches the main host's cookie even with SameSite=Strict.
CORS only stops the script reading the answer; the write still happens. So the split
stops session content *reading* Atelier's pages and API, but stopping *writes* needs this
as well:

- **Every state-changing endpoint on the main host refuses requests not from its own
  origin.** That means `Sec-Fetch-Site: same-origin`, or an `Origin` equal to the main
  host's when the header is absent, *and* a CSRF token that a session can't plant: one
  bound to the server-side session, not a double-submit cookie. JavaScript on the sessions
  host can set cookies for the parent domain (cookie tossing), and `__Host-` protects
  only cookies with that prefix. Any cookie the main host trusts must be `__Host-`.
- **Deny test, in the spine item that adds the first main-host endpoint (spine #2):** a
  POST carrying a valid main-host cookie and `Origin: https://<sessions host>` (or
  `Sec-Fetch-Site: same-site`) is refused.
- **Choice of domain, for the user:** a sessions host on a *different registrable domain*
  (for example `atelier-sessions.example.net`) is cross-site, so SameSite cookies also
  stop these writes, and cookie tossing between the two is impossible. It costs a second
  domain name. A subdomain is simpler to get from IT but relies on the CSRF check alone.
  Either way the check above is required. JupyterHub's guidance prefers a separate
  domain for the same reason.

**Amended 2026-11-07 — accepted by the user, with a subdomain.** Sessions are served from a
subdomain of Atelier's own domain (for example `sessions.atelier.example.org` next to
`atelier.example.org`): one more DNS name under a domain IT already runs, and a second
name on the same certificate. Because a subdomain is same-site, the cross-origin write
rule above is what protects Atelier's pages, and it is required (spine #2).

---

## ADR-0017 — PAM sign-in through pam_sss, in process, as Atelier's own non-root account

**Date:** 2026-11-07 · **Status:** accepted

ADR-0005 says sign-in goes through PAM. Checking *another* user's password with
`pam_unix` needs root (`unix_chkpwd` lets a non-root caller check only its own password),
which would mean running Atelier as root or adding a setuid helper. On an AD-joined host
the password check is `pam_sss`, which passes it to SSSD, and SSSD lets non-root
services authenticate domain users (its `pam_trusted_users` default is all). The users
are AD accounts.

**Decision:** Atelier runs as a dedicated system account (`atelier`), a member of the
`docker` group, and calls libpam in process through a small cgo binding of its own (no
Go dependency; building needs the `libpam0g-dev` headers). Its PAM service is
`/etc/pam.d/atelier`, using `pam_sss` for auth and account. Local, non-AD accounts can't
sign in this way; that is accepted. Tests use a private PAM config folder
(`pam_start_confdir`) with modules that need no root, such as `pam_exec`, so they run
without installing anything into `/etc/pam.d`.

**Note:** membership of `docker` is root-equivalent (CLAUDE.md), so this keeps password
handling out of a root process but doesn't make Atelier unprivileged overall. Only
Atelier's own account may be in that group.

**Rejected:** running Atelier as root (a root process parsing passwords and HTTP); a
setuid helper (extra install step and attack surface, not needed with `pam_sss`); the
`msteinert/pam` package (fine, but a dependency for about a hundred lines of cgo).

---

## ADR-0018 — Configuration as code: one strict YAML file is the source of truth

**Date:** 2026-11-07 · **Status:** accepted

The user wants configuration as code, as in forge and Skald: a file in git describes the
installation, and a change is a reviewed commit, not a click in an admin page.

**Decision:** `atelier.yaml` is the source of truth for everything it declares: the main
and sessions hostnames (ADR-0016), sign-in (PAM service, OIDC issuer and client), the
Docker socket and session network, and later images (ADR-0014), resource profiles and
group grants (ADR-0013), session caps and the idle timeout. As in forge:

- An unknown key is an error at startup, so a typo fails loudly.
- Secrets are never written in the file: `${NAME}` placeholders are expanded from the
  environment, and an unset variable is an error, never an empty string.
- `atelier -config-check -config <file>` validates a file without starting anything.
- A JSON schema ships with it, for editor completion and CI checks, as in Skald.
- Objects the file declares are read-only in the admin UI and API, shown as "config".

Parsing uses `sigs.k8s.io/yaml` (MIT and BSD-3-Clause), which converts YAML to JSON and
uses `encoding/json`, so the config structs use plain `json` tags, as forge does
(user-approved dependency, 2026-11-07).

**Rejected:** settings in the database edited through the UI (no review, no history);
environment variables only (no structure for lists like images and grants); TOML or JSON
(the sibling projects use YAML).
