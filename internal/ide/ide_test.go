package ide

import (
	"slices"
	"strings"
	"testing"
)

func TestRegistry(t *testing.T) {
	if got := Names(); !slices.Equal(got, []string{"rstudio"}) {
		t.Errorf("Names = %v", got)
	}
	d, err := Lookup("rstudio")
	if err != nil || d.Name != "rstudio" {
		t.Fatalf("Lookup = %+v, %v", d, err)
	}
	if _, err := Lookup("positron"); err == nil {
		t.Error("Lookup of an unknown IDE succeeded")
	}
}

// Every definition must be complete enough for the launcher and proxy to use it.
func TestDefinitionsAreComplete(t *testing.T) {
	for _, name := range Names() {
		d, _ := Lookup(name)
		t.Run(name, func(t *testing.T) {
			if d.Title == "" || d.Image == "" || len(d.Cmd) == 0 {
				t.Errorf("missing Title, Image or Cmd: %+v", d)
			}
			if d.Port < 1024 || d.Port > 65535 {
				t.Errorf("port %d: want an unprivileged port (sessions have no capabilities)", d.Port)
			}
			if !strings.HasPrefix(d.Health.Path, "/") || d.Health.MaxStatus < 200 || d.Health.MaxStatus > 499 {
				t.Errorf("health check %+v", d.Health)
			}
		})
	}
}

// The start command is fixed text: per-session values come in only as environment
// variables, so nothing a user controls (their name, the session ID) is ever spliced
// into a shell script.
func TestStartCommandsAreFixed(t *testing.T) {
	p := Params{User: "x'; rm -rf / #", Home: "/home/x", Prefix: "/s/abc"}
	for _, name := range Names() {
		d, _ := Lookup(name)
		t.Run(name, func(t *testing.T) {
			for _, part := range append(append([]string(nil), d.Entrypoint...), d.Cmd...) {
				for _, v := range []string{p.User, p.Prefix, "/s/"} {
					if strings.Contains(part, v) {
						t.Errorf("start command contains per-session value %q", v)
					}
				}
			}
			env := d.SessionEnv(p)
			if !slices.Contains(env, "ATELIER_USER="+p.User) || !slices.Contains(env, "ATELIER_PREFIX=/s/abc") {
				t.Errorf("Env = %v", env)
			}
		})
	}
}

// Per-session variables come last, so a definition's fixed Env can't override them
// (Docker keeps the last value of a repeated key).
func TestSessionEnvWins(t *testing.T) {
	d := Definition{Port: 9000, Env: []string{"USER=root", "ATELIER_PREFIX=/"}}
	env := d.SessionEnv(Params{User: "alice", Home: "/home/alice", Prefix: "/s/abc"})
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	want := map[string]string{"USER": "alice", "HOME": "/home/alice", "ATELIER_USER": "alice",
		"ATELIER_HOME": "/home/alice", "ATELIER_PREFIX": "/s/abc", "ATELIER_PORT": "9000"}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("%s = %q, want %q", k, last[k], v)
		}
	}
}
