package ide

// JupyterLab, run as the session's own account.
//
// Why each piece is there (checked against quay.io/jupyter/minimal-notebook:lab-4.6.5,
// Jupyter Server 2.21.1):
//   - The image's entrypoint (tini + start.sh, which expects the "jovyan" user) is
//     bypassed; jupyter lab runs directly as the user with a one-user /etc/passwd. The
//     image's PATH (with /opt/conda/bin) is kept because Docker merges image env.
//   - No StripPrefix: Jupyter serves everything under --ServerApp.base_url, so the proxy
//     forwards /s/<id>/... unchanged, and the health check gets the prefix too.
//   - --IdentityProvider.token= and --ServerApp.password=: Jupyter's own login is off; the
//     proxy is the gate. Jupyter's XSRF protection stays on.
//   - --ServerApp.allow_remote_access=True: Jupyter otherwise refuses requests whose Host
//     isn't local (its DNS-rebinding guard); behind the proxy the Host is Atelier's.
//     Atelier itself refuses unknown hosts (spine #2).
//   - JUPYTER_RUNTIME_DIR under /tmp: kernel connection files and server info stay in the
//     container instead of the shared home (one per session, gone when it is removed).
//   - Health: GET <prefix>/api answers 200 with the server version, without auth.
var JupyterLab = Definition{
	Name:       "jupyterlab",
	Title:      "JupyterLab",
	Image:      "quay.io/jupyter/minimal-notebook:lab-4.6.5",
	Port:       8888,
	Entrypoint: []string{"/bin/sh", "-c"},
	Cmd: []string{`set -eu
: "${ATELIER_HOME:?}" "${ATELIER_PREFIX:?}" "${ATELIER_PORT:?}"
export JUPYTER_RUNTIME_DIR=/tmp/jupyter-runtime
exec jupyter lab \
  --no-browser \
  --ServerApp.ip=0.0.0.0 \
  --ServerApp.port="$ATELIER_PORT" \
  --ServerApp.port_retries=0 \
  --ServerApp.base_url="$ATELIER_PREFIX/" \
  --ServerApp.root_dir="$ATELIER_HOME" \
  --IdentityProvider.token= \
  --ServerApp.password= \
  --ServerApp.allow_remote_access=True`},
	StripPrefix: false,
	Health:      Health{Path: "/api", MaxStatus: 299},
}

func init() { register(JupyterLab) }
