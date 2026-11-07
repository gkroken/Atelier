package ide

// RStudio Server (open source), one rserver per container (ADR-0008), run as the
// session's own account rather than as root.
//
// Why each piece is there (checked against rocker/rstudio:4.5, RStudio 2026.04.0):
//   - The stock image keeps rserver's database config and state in root-owned
//     /etc/rstudio and /var/lib/rstudio-server; run as a normal user, rserver exits on
//     "Permission denied". So the script writes a sqlite database config under /tmp and
//     points --database-config-file and --server-data-dir there.
//   - --auth-none=1: Atelier's proxy has already checked who the user is and that the
//     session is theirs; RStudio's own sign-in page would ask a second time. With
//     auth-none, rserver signs in as $USER, which Definition.SessionEnv sets.
//   - --server-user must be the account rserver runs as.
//   - --www-root-path: the proxy strips /s/<id> (StripPrefix), and rserver adds it back to
//     its redirects and cookie paths. This is Posit's documented way to run behind a
//     path prefix.
//   - RSTUDIO_DATA_HOME per session: open-source RStudio is one session per user, so
//     every rserver on the same home would pick the same
//     ~/.local/share/rstudio/sessions/active folder (open editors, environment,
//     suspended workspace) and two rsessions would write it at once
//     (atelier-10f7686-F1). Each session gets its own data folder, keyed by the session
//     ID. User settings in ~/.config/rstudio stay shared, as on Workbench. Removing a
//     session's folder when it is quit belongs to the session lifecycle (spine #4/#5).
//     SessionEnv refuses an empty or path-unsafe ID, and the script also stops on an
//     empty one rather than fall back to a shared folder (atelier-9a20127-F1).
//   - --www-address=0.0.0.0: the proxy reaches the container by its bridge IP.
//   - Health: rserver answers 302 (to its sign-in or main page) as soon as it is up.
//     The R session itself (rsession) starts on the first page load, about 2 s later on
//     the dev host; that is part of what the startup measurement times.
var RStudio = Definition{
	Name:       "rstudio",
	Title:      "RStudio",
	Image:      "rocker/rstudio:4.5",
	Port:       8787,
	Entrypoint: []string{"/bin/sh", "-c"},
	Cmd: []string{`set -eu
: "${ATELIER_SESSION_ID:?}" "${ATELIER_USER:?}" "${ATELIER_HOME:?}" "${ATELIER_PREFIX:?}" "${ATELIER_PORT:?}"
mkdir -p /tmp/rstudio/db /tmp/rstudio/run
printf 'provider=sqlite\ndirectory=/tmp/rstudio/db\n' > /tmp/rstudio/database.conf
export RSTUDIO_DATA_HOME="$ATELIER_HOME/.local/share/rstudio/atelier/$ATELIER_SESSION_ID"
exec /usr/lib/rstudio-server/bin/rserver \
  --server-daemonize=0 \
  --auth-none=1 \
  --server-user="$ATELIER_USER" \
  --server-data-dir=/tmp/rstudio/run \
  --database-config-file=/tmp/rstudio/database.conf \
  --www-address=0.0.0.0 \
  --www-port="$ATELIER_PORT" \
  --www-root-path="$ATELIER_PREFIX/"`},
	StripPrefix: true,
	Health:      Health{Path: "/", MaxStatus: 399},
}

func init() { register(RStudio) }
