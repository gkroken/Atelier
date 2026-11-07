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
mkdir -p /tmp/rstudio/db /tmp/rstudio/run
printf 'provider=sqlite\ndirectory=/tmp/rstudio/db\n' > /tmp/rstudio/database.conf
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
