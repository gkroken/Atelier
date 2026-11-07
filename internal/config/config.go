// Package config loads atelier.yaml, the installation's source of truth (ADR-0018).
//
// Loading is strict, so a mistake fails at startup with a message naming the key:
//   - a duplicate key, an unknown key or a value of the wrong type is an error;
//   - ${NAME} in a string value is replaced from the environment, and an unset or empty
//     variable is an error, never an empty string. Only values are expanded, never keys;
//   - a secret (a field tagged config:"secret") must be a ${NAME} placeholder, so a
//     secret is never written in the file itself;
//   - required fields (config:"required") must be set, and values are checked.
//
// The JSON schema in schemas/atelier-config.schema.json describes the same structs; a
// test keeps the two from drifting.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	goyaml "go.yaml.in/yaml/v2"
	"sigs.k8s.io/yaml"
)

// Config is atelier.yaml.
type Config struct {
	Server Server `json:"server" config:"required"`
	Auth   Auth   `json:"auth" config:"required"`
	Docker Docker `json:"docker,omitempty"`
}

// Server is where Atelier listens and the two hostnames it serves (ADR-0016).
type Server struct {
	// Listen is the address the HTTP server binds, host:port.
	Listen string `json:"listen" config:"required"`
	// MainHost serves sign-in, the home page and the admin pages.
	MainHost string `json:"mainHost" config:"required"`
	// SessionsHost serves /s/<id>/; it must differ from MainHost.
	SessionsHost string `json:"sessionsHost" config:"required"`
	// TLS, when set, serves https with this certificate and key.
	TLS *TLS `json:"tls,omitempty"`
}

// TLS names the certificate and key files, PEM. The certificate covers both hosts; it
// is reloaded when the files change (internal/tlscert).
type TLS struct {
	CertFile string `json:"certFile" config:"required"`
	KeyFile  string `json:"keyFile" config:"required"`
}

// Auth is how people sign in. At least one method is on.
type Auth struct {
	PAM  *PAM  `json:"pam,omitempty"`
	OIDC *OIDC `json:"oidc,omitempty"`
}

// PAM signs in with the host's accounts through a PAM service (ADR-0017).
type PAM struct {
	// Service is the PAM service name, /etc/pam.d/<service>. Default "atelier".
	Service string `json:"service,omitempty"`
}

// OIDC signs in through an OpenID Connect provider.
type OIDC struct {
	// Issuer is the provider's https issuer URL.
	Issuer   string `json:"issuer" config:"required"`
	ClientID string `json:"clientId" config:"required"`
	// ClientSecret must be a ${NAME} placeholder.
	ClientSecret string `json:"clientSecret" config:"required,secret"`
	// UsernameClaim names the claim mapped to a host account. Default preferred_username.
	UsernameClaim string `json:"usernameClaim,omitempty"`
}

// Docker is how the launcher reaches Docker (ADR-0003, ADR-0015).
type Docker struct {
	// Socket is the Docker Engine socket. Default /var/run/docker.sock.
	Socket string `json:"socket,omitempty"`
	// Network is the session network. Default atelier-sessions.
	Network string `json:"network,omitempty"`
}

// Defaults for fields left out. The Docker ones match internal/backend/docker (a test).
const (
	DefaultPAMService    = "atelier"
	DefaultUsernameClaim = "preferred_username"
	DefaultDockerSocket  = "/var/run/docker.sock"
	DefaultDockerNetwork = "atelier-sessions"
)

// LoadFile reads and loads the file at path, with the process environment.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c, err := Load(data, os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Load parses data, expands placeholders with lookup, applies defaults and validates.
func Load(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	if err := oneDocument(data); err != nil {
		return nil, err
	}
	js, err := yaml.YAMLToJSONStrict(data) // refuses duplicate keys
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var doc any
	if err := json.Unmarshal(js, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if doc == nil {
		return nil, errors.New("the file is empty")
	}
	w := walker{lookup: lookup}
	doc = w.value(doc, reflect.TypeFor[Config](), "", false)
	if len(w.errs) > 0 {
		return nil, errors.Join(w.errs...)
	}
	js, err = json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields() // the walker already refused them; belt and braces
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// oneDocument refuses a file with a second YAML document after a "---" line: the parser
// reads only the first, so the rest would be dropped without a word, unknown keys
// included (atelier-3a70e08-F2).
func oneDocument(data []byte) error {
	dec := goyaml.NewDecoder(bytes.NewReader(data))
	var first, next any
	if err := dec.Decode(&first); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty; Load says so
		}
		return fmt.Errorf("parse: %w", err)
	}
	if err := dec.Decode(&next); !errors.Is(err, io.EOF) {
		return errors.New("the file has more than one YAML document (a '---' line after the start); put everything in one")
	}
	return nil
}

// walker checks the decoded YAML against the Go types, key by key, and expands
// placeholders in string values. It collects every error rather than stopping at one.
type walker struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (w *walker) fail(path, format string, args ...any) {
	w.errs = append(w.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (w *walker) value(v any, t reflect.Type, path string, secret bool) any {
	if t.Kind() == reflect.Pointer {
		if v == nil {
			w.fail(path, "empty; write {} to turn it on with the defaults")
			return v
		}
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			w.fail(path, "want a mapping, got %s", kind(v))
			return v
		}
		fields := fieldsOf(t)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f, ok := fields[k]
			if !ok {
				w.fail(join(path, k), "unknown key")
				continue
			}
			m[k] = w.value(m[k], f.Type, join(path, k), f.secret)
		}
		for _, name := range sortedKeys(fields) {
			if _, ok := m[name]; !ok && fields[name].required {
				w.fail(join(path, name), "required")
			}
		}
		return m
	case reflect.String:
		s, ok := v.(string)
		if !ok {
			w.fail(path, "want a string, got %s", kind(v))
			return v
		}
		if secret && !placeholderOnly.MatchString(s) {
			w.fail(path, "a secret must be a ${NAME} placeholder, read from the environment, never written in the file")
			return v
		}
		return w.expand(s, path)
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			w.fail(path, "want true or false, got %s", kind(v))
		}
		return v
	case reflect.Int:
		if f, ok := v.(float64); !ok || f != float64(int(f)) {
			w.fail(path, "want a whole number, got %s", kind(v))
		}
		return v
	case reflect.Slice:
		l, ok := v.([]any)
		if !ok {
			w.fail(path, "want a list, got %s", kind(v))
			return v
		}
		for i := range l {
			l[i] = w.value(l[i], t.Elem(), path+"["+strconv.Itoa(i)+"]", secret)
		}
		return l
	}
	panic("config: unsupported field type " + t.String())
}

var (
	placeholder     = regexp.MustCompile(`\$\{([^}]*)\}`)
	placeholderOnly = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	envName         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// expand replaces each ${NAME} in s. The error names the variable and the key, never the
// variable's value.
func (w *walker) expand(s, path string) string {
	return placeholder.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if !envName.MatchString(name) {
			w.fail(path, "%q is not a valid ${NAME} placeholder", m)
			return m
		}
		val, ok := w.lookup(name)
		if !ok || val == "" {
			w.fail(path, "environment variable %s is not set", name)
			return m
		}
		return val
	})
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "nothing"
	case string:
		return "a string"
	case bool:
		return "true/false"
	case float64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "a mapping"
	}
	return fmt.Sprintf("%T", v)
}

func sortedKeys(m map[string]field) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type field struct {
	reflect.StructField
	required, secret bool
}

// fieldsOf maps each JSON key of struct t to its field.
func fieldsOf(t reflect.Type) map[string]field {
	out := map[string]field{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			panic("config: field " + t.Name() + "." + f.Name + " needs a json name")
		}
		tags := "," + f.Tag.Get("config") + ","
		out[name] = field{f, strings.Contains(tags, ",required,"), strings.Contains(tags, ",secret,")}
	}
	return out
}

func (c *Config) defaults() {
	if c.Auth.PAM != nil && c.Auth.PAM.Service == "" {
		c.Auth.PAM.Service = DefaultPAMService
	}
	if c.Auth.OIDC != nil && c.Auth.OIDC.UsernameClaim == "" {
		c.Auth.OIDC.UsernameClaim = DefaultUsernameClaim
	}
	if c.Docker.Socket == "" {
		c.Docker.Socket = DefaultDockerSocket
	}
	if c.Docker.Network == "" {
		c.Docker.Network = DefaultDockerNetwork
	}
}

var (
	// hostRE is a lowercase DNS name, labels of 1-63 letters, digits and inner hyphens,
	// optionally with a port. No IP literals: the hosts get TLS certificates.
	hostRE    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`)
	serviceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	claimRE   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	networkRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
)

// hostname drops an optional :port.
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func (c *Config) validate() error {
	var errs []error
	bad := func(key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
	}
	s := c.Server
	if host, port, err := net.SplitHostPort(s.Listen); err != nil {
		bad("server.listen", "want host:port, got %q", s.Listen)
	} else if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		bad("server.listen", "port %q is not 1-65535", port)
	} else if host != "" && net.ParseIP(host) == nil && !hostRE.MatchString(host) {
		bad("server.listen", "%q is not an IP address or hostname", host)
	}
	for _, h := range []struct{ key, v string }{{"server.mainHost", s.MainHost}, {"server.sessionsHost", s.SessionsHost}} {
		if !hostRE.MatchString(h.v) || len(h.v) > 253 {
			bad(h.key, "want a lowercase hostname (optionally :port), got %q", h.v)
		} else if net.ParseIP(hostname(h.v)) != nil {
			bad(h.key, "%q is an IP address; use a DNS name, so the two hosts get their own certificate and cookies (ADR-0016)", h.v)
		}
	}
	// Ports don't separate cookies, and :443 is the same origin as no port behind TLS,
	// so the hostnames themselves must differ (atelier-3a70e08-F1).
	if s.MainHost != "" && hostname(s.MainHost) == hostname(s.SessionsHost) {
		bad("server.sessionsHost", "must be a different hostname from server.mainHost, not just another port, so session content never shares an origin or cookies with Atelier's pages (ADR-0016)")
	}

	if t := s.TLS; t != nil {
		for _, f := range []struct{ key, v string }{{"server.tls.certFile", t.CertFile}, {"server.tls.keyFile", t.KeyFile}} {
			if !filepath.IsAbs(f.v) || filepath.Clean(f.v) != f.v {
				bad(f.key, "want an absolute, clean path, got %q", f.v)
			}
		}
	}

	if c.Auth.PAM == nil && c.Auth.OIDC == nil {
		bad("auth", "turn on at least one of pam and oidc")
	}
	if p := c.Auth.PAM; p != nil && !serviceRE.MatchString(p.Service) {
		bad("auth.pam.service", "want a PAM service name like %q, got %q", DefaultPAMService, p.Service)
	}
	if o := c.Auth.OIDC; o != nil {
		u, err := url.Parse(o.Issuer)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			bad("auth.oidc.issuer", "want an https URL with no query or fragment, got %q", o.Issuer)
		}
		if strings.TrimSpace(o.ClientID) == "" {
			bad("auth.oidc.clientId", "empty")
		}
		if !claimRE.MatchString(o.UsernameClaim) {
			bad("auth.oidc.usernameClaim", "want a claim name like %q, got %q", DefaultUsernameClaim, o.UsernameClaim)
		}
	}

	if d := c.Docker.Socket; !filepath.IsAbs(d) || filepath.Clean(d) != d {
		bad("docker.socket", "want an absolute, clean path, got %q", d)
	}
	if !networkRE.MatchString(c.Docker.Network) {
		bad("docker.network", "want a Docker network name, got %q", c.Docker.Network)
	}
	return errors.Join(errs...)
}
