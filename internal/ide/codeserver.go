package ide

// code-server (VS Code in the browser), run as the session's own account.
//
// Why each piece is there (checked against codercom/code-server:4.145.2, Code 1.145.2):
//   - The image's entrypoint (fixuid, the "coder" user) is bypassed: the session runs as
//     the user's real account with a one-user /etc/passwd, like every IDE here.
//   - --auth none: Atelier's proxy is the gate, as for RStudio.
//   - StripPrefix: code-server uses relative URLs and expects a reverse proxy to remove
//     the path prefix. (--abs-proxy-base-path is only for code-server's own port
//     forwarding, not for serving code-server under a prefix.)
//   - A data folder per session (--user-data-dir), for the same reason as RStudio's
//     RSTUDIO_DATA_HOME: two VS Code instances writing one state database at once is not
//     supported. Extensions stay shared (--extensions-dir), and so do the user's
//     settings and keybindings: the session's copies are symlinks to the default
//     location, ~/.local/share/code-server/User, which is where existing settings live.
//   - Health: GET /healthz answers 200 once the server is up.
var CodeServer = Definition{
	Name:       "code-server",
	Title:      "VS Code",
	Image:      "codercom/code-server:4.145.2",
	Port:       8080,
	Entrypoint: []string{"/bin/sh", "-c"},
	Cmd: []string{`set -eu
: "${ATELIER_SESSION_ID:?}" "${ATELIER_HOME:?}" "${ATELIER_PORT:?}"
base="$ATELIER_HOME/.local/share/code-server"
data="$base/atelier/$ATELIER_SESSION_ID"
mkdir -p "$base/User" "$base/extensions" "$data/User"
for f in settings.json keybindings.json; do
  [ -e "$base/User/$f" ] || printf '{}\n' > "$base/User/$f"
  ln -sfn "$base/User/$f" "$data/User/$f"
done
exec /usr/bin/code-server \
  --bind-addr "0.0.0.0:$ATELIER_PORT" \
  --auth none \
  --disable-telemetry \
  --disable-update-check \
  --user-data-dir "$data" \
  --extensions-dir "$base/extensions" \
  "$ATELIER_HOME"`},
	StripPrefix: true,
	Health:      Health{Path: "/healthz", MaxStatus: 299},
}

func init() { register(CodeServer) }
