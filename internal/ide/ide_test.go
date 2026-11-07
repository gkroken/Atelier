package ide

import (
	"os/exec"
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
// variables. Today's definitions are literals, so this holds by construction; the test
// guards a future definition that builds its command with fmt.Sprintf or similar
// (atelier-10f7686-N3).
func TestStartCommandsAreFixed(t *testing.T) {
	p := Params{SessionID: "abcdefghijklmnopqrstuvwxyz", User: "alice", Home: "/home/x", Prefix: "/s/abcdefghijklmnopqrstuvwxyz"}
	for _, name := range Names() {
		d, _ := Lookup(name)
		t.Run(name, func(t *testing.T) {
			for _, part := range append(append([]string(nil), d.Entrypoint...), d.Cmd...) {
				for _, v := range []string{p.SessionID, p.User, p.Prefix, "/s/"} {
					if strings.Contains(part, v) {
						t.Errorf("start command contains per-session value %q", v)
					}
				}
			}
			env, err := d.SessionEnv(p)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(env, "ATELIER_USER="+p.User) || !slices.Contains(env, "ATELIER_PREFIX="+p.Prefix) {
				t.Errorf("Env = %v", env)
			}
		})
	}
}

// Per-session variables come last, so a definition's fixed Env can't override them
// (Docker keeps the last value of a repeated key).
func TestSessionEnvWins(t *testing.T) {
	d := Definition{Port: 9000, Env: []string{"USER=root", "ATELIER_PREFIX=/"}}
	env, err := d.SessionEnv(Params{SessionID: "sid12345", User: "alice", Home: "/home/alice", Prefix: "/s/abc"})
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	want := map[string]string{"ATELIER_SESSION_ID": "sid12345", "USER": "alice", "HOME": "/home/alice", "ATELIER_USER": "alice",
		"ATELIER_HOME": "/home/alice", "ATELIER_PREFIX": "/s/abc", "ATELIER_PORT": "9000"}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("%s = %q, want %q", k, last[k], v)
		}
	}
}

// Every Params field ends up where a definition may use it in a path or a passwd line;
// each of these must be refused (atelier-9a20127-F1).
func TestSessionEnvRefusesBadParams(t *testing.T) {
	good := Params{SessionID: "abcdefghijklmnopqrstuvwxyz", User: "alice", Home: "/home/alice", Prefix: "/s/abcdefghijklmnopqrstuvwxyz"}
	if _, err := RStudio.SessionEnv(good); err != nil {
		t.Fatalf("good params refused: %v", err)
	}
	for _, ok := range []func(*Params){
		func(p *Params) { p.User = "alice@corp.example.com" },
		func(p *Params) { p.User = "First.Last" },
	} {
		p := good
		ok(&p)
		if _, err := RStudio.SessionEnv(p); err != nil {
			t.Errorf("%+v refused: %v", p, err)
		}
	}
	for name, mod := range map[string]func(*Params){
		"empty session ID":    func(p *Params) { p.SessionID = "" },
		"dotdot session ID":   func(p *Params) { p.SessionID = "../x" },
		"slash in session ID": func(p *Params) { p.SessionID = "abcdefgh/ijkl" },
		"short session ID":    func(p *Params) { p.SessionID = "abc" },
		"upper session ID":    func(p *Params) { p.SessionID = "ABCDEFGHIJKL" },
		"empty user":          func(p *Params) { p.User = "" },
		"colon in user":       func(p *Params) { p.User = "a:0:0" },
		"newline in user":     func(p *Params) { p.User = "a\nroot" },
		"shell in user":       func(p *Params) { p.User = "x'; rm -rf / #" },
		"slash in user":       func(p *Params) { p.User = "a/b" },
		"backslash in user":   func(p *Params) { p.User = `CORP\alice` },
		"empty home":          func(p *Params) { p.Home = "" },
		"relative home":       func(p *Params) { p.Home = "home/alice" },
		"unclean home":        func(p *Params) { p.Home = "/home/alice/../bob" },
		"root home":           func(p *Params) { p.Home = "/" },
		"colon in home":       func(p *Params) { p.Home = "/home/a:b" },
		"newline in home":     func(p *Params) { p.Home = "/home/a\nroot" },
		"tab in home":         func(p *Params) { p.Home = "/home/a\tb" },
		"DEL in home":         func(p *Params) { p.Home = "/home/a\x7fb" },
		"empty prefix":        func(p *Params) { p.Prefix = "" },
		"trailing slash":      func(p *Params) { p.Prefix = "/s/abc/" },
		"dotdot prefix":       func(p *Params) { p.Prefix = "/s/.." },
		"query in prefix":     func(p *Params) { p.Prefix = "/s/abc?x" },
		"relative prefix":     func(p *Params) { p.Prefix = "s/abc" },
	} {
		t.Run(name, func(t *testing.T) {
			p := good
			mod(&p)
			if env, err := RStudio.SessionEnv(p); err == nil {
				t.Errorf("SessionEnv(%+v) = %v, want an error", p, env)
			}
		})
	}
}

// The RStudio script itself stops on an empty per-session variable, before it creates
// anything, in case something ever builds the environment without SessionEnv.
func TestRStudioScriptStopsOnEmptySessionID(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command("sh", "-c", RStudio.Cmd[0])
	cmd.Env = []string{"ATELIER_SESSION_ID=", "ATELIER_USER=alice", "ATELIER_HOME=/nonexistent",
		"ATELIER_PREFIX=/s/x", "ATELIER_PORT=8787", "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("script succeeded with an empty session ID: %s", out)
	}
	if !strings.Contains(string(out), "ATELIER_SESSION_ID") {
		t.Errorf("script failed, but not on the session ID guard: %s", out)
	}
}
