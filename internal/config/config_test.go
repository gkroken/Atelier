package config

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gkroken/Atelier/internal/backend/docker"
)

const minimal = `
server:
  listen: 127.0.0.1:8080
  mainHost: atelier.example.org
  sessionsHost: sessions.atelier.example.org
auth:
  pam: {}
`

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestMinimalGetsDefaults(t *testing.T) {
	c, err := Load([]byte(minimal), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Server: Server{Listen: "127.0.0.1:8080", MainHost: "atelier.example.org", SessionsHost: "sessions.atelier.example.org"},
		Auth:   Auth{PAM: &PAM{Service: "atelier"}},
		Docker: Docker{Socket: "/var/run/docker.sock", Network: "atelier-sessions"},
	}
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("got %+v\nwant %+v", *c, want)
	}
}

func TestDevOnLoopback(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8080", "localhost:8080", "\"[::1]:8080\"", "127.0.0.2:1"} {
		doc := strings.Replace(strings.Replace(minimal, "  pam: {}", "  dev: {}", 1), "127.0.0.1:8080", listen, 1)
		c, err := Load([]byte(doc), env(nil))
		if err != nil || c.Auth.Dev == nil {
			t.Errorf("%s: %v", listen, err)
		}
	}
}

func TestDockerDefaultsMatchTheBackend(t *testing.T) {
	if DefaultDockerSocket != docker.DefaultSocket || DefaultDockerNetwork != docker.DefaultNetwork {
		t.Errorf("config defaults %q %q, backend %q %q", DefaultDockerSocket, DefaultDockerNetwork, docker.DefaultSocket, docker.DefaultNetwork)
	}
}

func TestExampleLoads(t *testing.T) {
	data, err := os.ReadFile("../../deploy/config/atelier.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Load(data, env(map[string]string{"ATELIER_OIDC_CLIENT_SECRET": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.OIDC == nil || c.Auth.OIDC.ClientSecret != "s3cret" || c.Auth.PAM == nil {
		t.Errorf("example: %+v %+v", c.Auth.PAM, c.Auth.OIDC)
	}
}

func TestPlaceholdersExpandInValues(t *testing.T) {
	doc := minimal + `  oidc:
    issuer: https://${IDP}/realms/main
    clientId: atelier
    clientSecret: ${SECRET}
`
	c, err := Load([]byte(doc), env(map[string]string{"IDP": "login.example.org", "SECRET": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	if o := c.Auth.OIDC; o.Issuer != "https://login.example.org/realms/main" || o.ClientSecret != "s3cret" || o.UsernameClaim != "preferred_username" {
		t.Errorf("oidc: %+v", o)
	}
	// A $ that isn't ${...} stays as it is, and an expanded value isn't expanded again.
	c, err = Load([]byte(strings.Replace(doc, "clientId: atelier", "clientId: a$b", 1)),
		env(map[string]string{"IDP": "login.example.org", "SECRET": "${IDP}"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.OIDC.ClientID != "a$b" || c.Auth.OIDC.ClientSecret != "${IDP}" {
		t.Errorf("got %q %q", c.Auth.OIDC.ClientID, c.Auth.OIDC.ClientSecret)
	}
}

// Each refusal names the key, and none prints a secret's value.
func TestRefusals(t *testing.T) {
	oidc := minimal + `  oidc:
    issuer: https://login.example.org
    clientId: atelier
    clientSecret: ${SECRET}
`
	vars := map[string]string{"SECRET": "s3cret-value"}
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"unknown key", strings.Replace(oidc, "clientId:", "clientID:", 1), "auth.oidc.clientID: unknown key"},
		{"unknown top-level key", minimal + "images: []\n", "images: unknown key"},
		{"unset variable", strings.Replace(oidc, "${SECRET}", "${NOT_SET}", 1), "auth.oidc.clientSecret: environment variable NOT_SET is not set"},
		{"unset variable in a plain value", strings.Replace(minimal, "atelier.example.org\n", "${MAIN_HOST}\n", 1), "server.mainHost: environment variable MAIN_HOST is not set"},
		{"bad placeholder", strings.Replace(minimal, "atelier.example.org\n", "${1X}\n", 1), `server.mainHost: "${1X}" is not a valid`},
		{"missing required", strings.Replace(minimal, "  sessionsHost: sessions.atelier.example.org\n", "", 1), "server.sessionsHost: required"},
		{"missing required nested", strings.Replace(oidc, "    clientId: atelier\n", "", 1), "auth.oidc.clientId: required"},
		{"missing section", "auth:\n  pam: {}\n", "server: required"},
		{"secret written in the file", strings.Replace(oidc, "${SECRET}", "hunter2", 1), "auth.oidc.clientSecret: a secret must be a ${NAME} placeholder"},
		{"secret partly written", strings.Replace(oidc, "${SECRET}", "x${SECRET}", 1), "auth.oidc.clientSecret: a secret must be"},
		{"wrong type", strings.Replace(minimal, "127.0.0.1:8080", "[a, b]", 1), "server.listen: want a string, got a list"},
		{"wrong type for a section", strings.Replace(minimal, "  pam: {}", "  pam: yes", 1), "auth.pam: want a mapping, got true/false"},
		{"empty section", strings.Replace(minimal, "  pam: {}", "  pam:", 1), "auth.pam: empty; write {}"},
		{"duplicate key", minimal + "auth:\n  pam: {}\n", `"auth" already set`},
		{"empty file", "", "the file is empty"},
		{"bad listen", strings.Replace(minimal, "127.0.0.1:8080", "localhost", 1), "server.listen: want host:port"},
		{"bad listen port", strings.Replace(minimal, "127.0.0.1:8080", "127.0.0.1:99999", 1), "server.listen: port"},
		{"uppercase host", strings.Replace(minimal, "mainHost: atelier", "mainHost: Atelier", 1), "server.mainHost: want a lowercase hostname"},
		{"host with a path", strings.Replace(minimal, "mainHost: atelier.example.org", "mainHost: atelier.example.org/x", 1), "server.mainHost: want a lowercase hostname"},
		{"same hosts", strings.Replace(minimal, "sessions.atelier", "atelier", 1), "server.sessionsHost: must be a different hostname"},
		{"same host, other port", strings.Replace(minimal, "sessions.atelier.example.org", "atelier.example.org:8443", 1), "server.sessionsHost: must be a different hostname"},
		{"same host, :443", strings.Replace(minimal, "sessions.atelier.example.org", "atelier.example.org:443", 1), "server.sessionsHost: must be a different hostname"},
		{"same host, both with ports", strings.Replace(strings.Replace(minimal, "sessions.atelier.example.org", "atelier.example.org:2", 1), "mainHost: atelier.example.org", "mainHost: atelier.example.org:1", 1), "server.sessionsHost: must be a different hostname"},
		{"IP as main host", strings.Replace(minimal, "mainHost: atelier.example.org", "mainHost: 10.0.0.1", 1), "server.mainHost: \"10.0.0.1\" is an IP address"},
		{"IP with port as sessions host", strings.Replace(minimal, "sessions.atelier.example.org", "10.0.0.1:8443", 1), "server.sessionsHost: \"10.0.0.1:8443\" is an IP address"},
		{"second document", minimal + "---\ndocker:\n  netwrk: typo\n", "more than one YAML document"},
		{"empty second document", minimal + "---\n", "more than one YAML document"},
		{"no sign-in", strings.Replace(minimal, "  pam: {}", "  {}", 1), "auth: turn on at least one"},
		{"http issuer", strings.Replace(oidc, "https://", "http://", 1), "auth.oidc.issuer: want an https URL"},
		{"bad pam service", strings.Replace(minimal, "pam: {}", "pam: {service: ../x}", 1), "auth.pam.service: want a PAM service name"},
		{"tls without key", strings.Replace(minimal, "auth:", "  tls: {certFile: /etc/c.pem}\nauth:", 1), "server.tls.keyFile: required"},
		{"relative tls cert", strings.Replace(minimal, "auth:", "  tls: {certFile: c.pem, keyFile: /etc/k.pem}\nauth:", 1), "server.tls.certFile: want an absolute"},
		{"empty tls", strings.Replace(minimal, "auth:", "  tls:\nauth:", 1), "server.tls: empty"},
		{"dev with pam", strings.Replace(minimal, "  pam: {}", "  pam: {}\n  dev: {}", 1), "auth.dev: not together with auth.pam"},
		{"dev on all addresses", strings.Replace(strings.Replace(minimal, "  pam: {}", "  dev: {}", 1), "127.0.0.1:8080", "0.0.0.0:8080", 1), "auth.dev: only when server.listen is a loopback"},
		{"dev on an empty host", strings.Replace(strings.Replace(minimal, "  pam: {}", "  dev: {}", 1), "127.0.0.1:8080", "\":8080\"", 1), "auth.dev: only when server.listen is a loopback"},
		{"dev with settings", strings.Replace(minimal, "  pam: {}", "  dev: {password: x}", 1), "auth.dev.password: unknown key"},
		{"relative socket", minimal + "docker:\n  socket: docker.sock\n", "docker.socket: want an absolute"},
		{"bad network", minimal + "docker:\n  network: -x\n", "docker.network: want a Docker network name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.doc), env(vars))
			if err == nil {
				t.Fatalf("loaded; want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q\nwant it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cret-value") {
				t.Errorf("error prints the secret: %q", err)
			}
		})
	}
}

// Different hostnames with ports are fine, and a leading "---" is still one document.
func TestAcceptsPortsOnDifferentHostsAndALeadingSeparator(t *testing.T) {
	doc := "---\n" + strings.Replace(strings.Replace(minimal, "sessions.atelier.example.org", "sessions.localhost:8443", 1),
		"mainHost: atelier.example.org", "mainHost: localhost:8443", 1)
	if _, err := Load([]byte(doc), env(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestReportsEveryProblemAtOnce(t *testing.T) {
	doc := strings.Replace(minimal, "listen:", "listn:", 1) + "extra: 1\n"
	_, err := Load([]byte(doc), env(nil))
	for _, want := range []string{"server.listn: unknown key", "server.listen: required", "extra: unknown key"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v: want %q", err, want)
		}
	}
}

// TestSchemaMatchesStructs keeps schemas/atelier-config.schema.json and the Go structs
// in step: the same keys at every level, the same required keys, the same types, no
// extra keys allowed, and secrets constrained to a placeholder.
func TestSchemaMatchesStructs(t *testing.T) {
	data, err := os.ReadFile("../../schemas/atelier-config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	compareSchema(t, "(root)", schema, reflect.TypeFor[Config](), false)
}

func compareSchema(t *testing.T, path string, s map[string]any, typ reflect.Type, secret bool) {
	t.Helper()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	jsonType := map[reflect.Kind]string{reflect.Struct: "object", reflect.String: "string", reflect.Bool: "boolean", reflect.Int: "integer", reflect.Slice: "array"}[typ.Kind()]
	if s["type"] != jsonType {
		t.Errorf("%s: schema type %v, Go %s", path, s["type"], typ)
	}
	if secret && s["pattern"] != placeholderOnly.String() {
		t.Errorf("%s: secret, so the schema pattern should be %q, got %v", path, placeholderOnly, s["pattern"])
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	if s["additionalProperties"] != false {
		t.Errorf("%s: schema should set additionalProperties: false", path)
	}
	props, _ := s["properties"].(map[string]any)
	fields := fieldsOf(typ)
	var goKeys, schemaKeys, goReq, schemaReq []string
	for k, f := range fields {
		goKeys = append(goKeys, k)
		if f.required {
			goReq = append(goReq, k)
		}
	}
	for k := range props {
		schemaKeys = append(schemaKeys, k)
	}
	if r, ok := s["required"].([]any); ok {
		for _, k := range r {
			schemaReq = append(schemaReq, k.(string))
		}
	}
	slices.Sort(goKeys)
	slices.Sort(schemaKeys)
	slices.Sort(goReq)
	slices.Sort(schemaReq)
	if !slices.Equal(goKeys, schemaKeys) {
		t.Errorf("%s: Go keys %v, schema keys %v", path, goKeys, schemaKeys)
	}
	if !slices.Equal(goReq, schemaReq) {
		t.Errorf("%s: Go required %v, schema required %v", path, goReq, schemaReq)
	}
	for k, f := range fields {
		if p, ok := props[k].(map[string]any); ok {
			compareSchema(t, path+"."+k, p, f.Type, f.secret)
		}
	}
}
