# How Posit Workbench works, and what it means for Atelier

Researched 2026-11-07. Workbench is the bar Atelier has to meet for speed and ease of use.

## Architecture

- **`rserver`** serves sign-in, the home page and a proxy to each session.
- **The Job Launcher** starts sessions through a plugin: **Local** (same host),
  **Kubernetes** or **Slurm**. With the Local plugin, `rserver` talks to the launcher on
  `127.0.0.1:5559` and sessions report back to `rserver` on its callback address.
- **Single server** runs everything on one host, with several sessions per user.
  **Load-balanced** and **Launcher** setups need shared NFS home folders (`no_root_squash`)
  and an external PostgreSQL.

## Why Local mode feels fast

A session is a process forked as the user's real Linux account (through PAM), using the R
and Python installed on the host and the home folder already there. Nothing has to be
set up per session. On Kubernetes, each session waits for a pod to be scheduled and, on a
cold node, for an image pull.

## What users see

- Home page listing running and suspended sessions; several at once.
- New Session dialog: IDE (RStudio Pro, Positron Pro, VS Code, JupyterLab), R/Python
  version, resources or image.
- Suspended sessions resume where they left off; closing the browser doesn't end a
  session.
- Project sharing, Workbench Jobs, admin dashboard.

## Recent releases

- **2026.01:** admin-enforced settings per user or group; custom OAuth credentials
  (Enhanced/Advanced tiers); Workbench Jobs find project venvs and R interpreters.
- **2026.04:** session metadata can live in a central database (`session-use-file-storage=0`),
  so Workbench no longer needs root or mounted homes in locked-down containers; per-session
  resource display; out-of-band Positron upgrades (`rstudio-server install-positron`);
  monthly releases from now on.

## Session startup paths

Typical estimates on decent hardware, not measurements. Spine #0 measures ours.

| Path | Steps (time each adds) | Total |
|---|---|---|
| Host process (Workbench Local) | click · launcher +0.05 s · fork as user +0.2 s · R starts +0.5–1 s · IDE loads +1–1.5 s | about 2–3 s |
| **Pre-pulled container (chosen)** | click · create +0.3 s · start + mount +0.5 s · IDE boots +1–2.5 s · IDE loads +1–1.5 s | about 3–5 s |
| Warm pool | click · claim idle +0.1 s · set user +0.3 s · IDE boots +0.5–1 s · IDE loads +1–1.5 s | about 2–3 s |
| Cold container | click · **pull image +30–180 s** · create, start +0.8 s · IDE boots · IDE loads | about 35 s–3 min |
| Kubernetes pod | click · schedule +0.5–2 s · **attach NFS +1–5 s** · IDE boots · IDE loads | about 6–15 s; minutes on a cold node |

Every warm path ends up within about 1–2 s of the others, because the IDE loading in the
browser takes the same time either way. The slow steps are pulling an image and
attaching network storage at click time.

### Trade-offs of pre-pulled containers

**Gains:** startup close to Workbench; isolation and per-session CPU/RAM caps; one image
per R/Python version, rollback by tag; the same environment Skald deploys; a clean host;
the same model moves to Kubernetes.

**Costs:**

- Images must be pulled onto each host when published (2–5 GB each).
- Account IDs must be mapped correctly so home-folder ownership is right.
- User R/Python package libraries must be kept separate per image.
- Suspending stops the container, so memory state is lost.
- Docker runs as root, so whoever controls the launcher has root on the host: lock it
  down, or use rootless Docker or Podman.
- `apt` installs inside a session are lost.

## Sources

- [Workbench reference architectures](https://docs.posit.co/ide/server-pro/admin/reference-architectures/generic-architectures.html)
- [Launcher with the Local plugin](https://support.posit.co/hc/en-us/articles/360018219913-Setting-up-Posit-Workbench-s-Launcher-feature-with-the-Local-plugin)
- [Workbench 2026.01 release](https://posit.co/blog/whats-new-in-posit-workbench-2026-01-0-enhanced-admin-settings-flexible-access-across-providers-and-smarter-jobs)
- [Workbench 2026.04 release](https://posit.co/blog/workbench-release-2026-04)
