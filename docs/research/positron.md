# Positron: licensing and our options

Researched 2026-11-07. Not legal advice; if Atelier becomes a product sold to others, get
Posit's position in writing.

## What the licence and availability allow

- **Positron Desktop** is "free for personal, academic, and commercial use with minimal
  restrictions" (Elastic License 2.0). Internal use at work is fine.
- **Hosting:** "You may not provide the software to third parties as a hosted or managed
  service" without Posit's agreement.
- **Browser Positron is the real blocker.** The server build (`positron-server`) checks a
  `license.lic` file and is available only as part of Posit Workbench, or free to
  students for coursework through `jupyter-positron-server`. Posit has said: "The native
  browser or server-based version of Positron is only available via Posit Workbench", and
  has no standalone server planned. The Elastic licence forbids getting around licence-key
  checks, so building it from source with the check removed is not allowed either.

## What we do instead

1. **v1 covers what Positron is used for:** VS Code (file viewer, git graph) plus one or
   more RStudio sessions on the same home folder, live (ADR-0007).
2. **Later — SSH into sessions.** Positron Desktop's Remote SSH runs the UI locally and a
   `positron-reh` server on the remote Linux host (unpacked into `~/.positron-server`,
   2 GB+ RAM, client and server versions must match). Sessions shut down when the desktop
   app closes unless `kernelSupervisor.shutdownTimeout` is changed. Confirm with Posit that
   this use is allowed before advertising it.
3. **Later — our own VS Code extension** with Positron-style panes.

## Building blocks for our own extension

| Piece | Licence | Use |
|---|---|---|
| [ark](https://github.com/posit-dev/ark) | MIT | Positron's R kernel: Jupyter kernel + LSP + DAP, with comms for variables, plots, data explorer, help, connections, UI |
| ipykernel | BSD | Python kernel; needs our own small helper for variables and plots |
| [vscode-R](https://github.com/REditorSupport/vscode-R) | MIT | Reference for workspace, plot and htmlwidget viewers |
| [httpgd](https://github.com/nx10/httpgd) | GPL-2 | Plot device; avoid embedding (GPL) |
| [kallichore](https://github.com/posit-dev/kallichore) | Elastic 2.0 | Positron's kernel supervisor — **do not copy** |
| Positron's Python kernel additions | Elastic 2.0 | **Do not copy** |

Rough effort to an MVP (one developer): kernel client + console 2–3 weeks; interpreter
discovery 1 week; plots 1–2 weeks; variables 2–3 weeks; viewer pane 1–2 weeks. About
2–3 months in all. Out of scope for the MVP: data explorer, notebooks, debugger,
connections pane, AI assistant.

## Sources

- [Positron licensing](https://positron.posit.co/licensing)
- [Positron Remote SSH](https://positron.posit.co/remote-ssh.html)
- [jupyter-positron-server](https://posit-dev.github.io/jupyter-positron-server/)
- [Posit forum: Positron via Coder](https://forum.posit.co/t/positron-via-coder/205773)
