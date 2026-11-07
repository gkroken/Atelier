package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const example = "../../deploy/config/atelier.example.yaml"

// The worked example passes -config-check, given the secret its placeholder names.
func TestExamplePassesConfigCheck(t *testing.T) {
	t.Setenv("ATELIER_OIDC_CLIENT_SECRET", "s3cret")
	var out strings.Builder
	if err := checkConfig(example, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "config ok: "+example+"\n" {
		t.Errorf("output %q", out.String())
	}
}

func TestConfigCheckRefuses(t *testing.T) {
	t.Setenv("ATELIER_OIDC_CLIENT_SECRET", "") // restored after the test
	os.Unsetenv("ATELIER_OIDC_CLIENT_SECRET")
	bad := filepath.Join(t.TempDir(), "atelier.yaml")
	os.WriteFile(bad, []byte("server: {listen: 127.0.0.1:1}\n"), 0o600)
	for _, tc := range []struct{ path, want string }{
		{"", "-config-check needs -config"},
		{example, "auth.oidc.clientSecret: environment variable ATELIER_OIDC_CLIENT_SECRET is not set"},
		{bad, "server.mainHost: required"},
		{filepath.Join(t.TempDir(), "missing.yaml"), "no such file"},
	} {
		var out strings.Builder
		err := checkConfig(tc.path, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Errorf("%q: err %v, out %q; want an error containing %q", tc.path, err, out.String(), tc.want)
		}
	}
}
