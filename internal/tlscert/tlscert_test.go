package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// writeCert writes a self-signed certificate for localhost with common name cn,
// valid until notAfter, and its key (mode keyMode) into dir.
func writeCert(t *testing.T, dir, cn string, notAfter time.Time, keyMode os.FileMode) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notAfter.Add(-48 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	write := func(path string, mode os.FileMode, typ string, b []byte) {
		// Write and rename, as certbot does, so a reader never sees half a file.
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tmp, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	write(certFile, 0o644, "CERTIFICATE", der)
	write(keyFile, keyMode, "EC PRIVATE KEY", kder)
	return certFile, keyFile
}

func cn(t *testing.T, s *Source) string {
	t.Helper()
	c, err := s.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Leaf.Subject.CommonName
}

func TestNewRefuses(t *testing.T) {
	future := time.Now().Add(90 * 24 * time.Hour)
	for _, tc := range []struct {
		name string
		make func(dir string) (string, string)
		want string
	}{
		{"world-readable key", func(d string) (string, string) { return writeCert(t, d, "a", future, 0o644) }, "accessible to other accounts"},
		{"world-writable key", func(d string) (string, string) { return writeCert(t, d, "a", future, 0o602) }, "accessible to other accounts"},
		{"expired", func(d string) (string, string) { return writeCert(t, d, "a", time.Now().Add(-time.Hour), 0o600) }, "expired"},
		{"missing key", func(d string) (string, string) { c, _ := writeCert(t, d, "a", future, 0o600); return c, d + "/nope" }, "no such file"},
		{"folder as cert", func(d string) (string, string) { _, k := writeCert(t, d, "a", future, 0o600); return d, k }, "not a regular file"},
		{"mismatched pair", func(d string) (string, string) {
			c, _ := writeCert(t, d, "a", future, 0o600)
			d2 := t.TempDir()
			_, k := writeCert(t, d2, "b", future, 0o600)
			return c, k
		}, "private key does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, k := tc.make(t.TempDir())
			_, err := New(c, k, quiet)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	// Group access to the key is fine (an ssl-cert group).
	c, k := writeCert(t, t.TempDir(), "a", future, 0o640)
	if _, err := New(c, k, quiet); err != nil {
		t.Fatal(err)
	}
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	future := time.Now().Add(90 * 24 * time.Hour)
	certFile, keyFile := writeCert(t, dir, "first", future, 0o600)
	s, err := New(certFile, keyFile, quiet)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	s.now = func() time.Time { return clock }
	s.checked = clock

	writeCert(t, dir, "second", future, 0o600)
	if got := cn(t, s); got != "first" {
		t.Errorf("before the recheck interval: %s, want first", got)
	}
	clock = clock.Add(recheck)
	if got := cn(t, s); got != "second" {
		t.Errorf("after renewal: %s, want second", got)
	}

	// A broken renewal (a cert without its key, a world-readable key, an expired
	// cert) keeps the working one.
	for _, broken := range []func(){
		func() { os.WriteFile(certFile, []byte("garbage"), 0o644) },
		func() { writeCert(t, dir, "third", future, 0o644) },
		func() { writeCert(t, dir, "third", clock.Add(-time.Hour), 0o600) },
	} {
		broken()
		clock = clock.Add(recheck)
		if got := cn(t, s); got != "second" {
			t.Errorf("after a broken renewal: %s, want second", got)
		}
	}
	writeCert(t, dir, "fourth", future, 0o600)
	clock = clock.Add(recheck)
	if got := cn(t, s); got != "fourth" {
		t.Errorf("after a good renewal following broken ones: %s, want fourth", got)
	}
}

// Through a real TLS listener: the client sees the certificate, and TLS 1.1 is refused.
func TestServes(t *testing.T) {
	certFile, keyFile := writeCert(t, t.TempDir(), "atelier-test", time.Now().Add(time.Hour), 0o600)
	s, err := New(certFile, keyFile, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Proto) }), TLSConfig: s.Config()}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()

	pool := x509.NewCertPool()
	pem, _ := os.ReadFile(certFile)
	pool.AppendCertsFromPEM(pem)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true}}
	resp, err := c.Get("https://localhost:" + strings.Split(ln.Addr().String(), ":")[1] + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.TLS.PeerCertificates[0].Subject.CommonName != "atelier-test" || string(body) != "HTTP/2.0" {
		t.Errorf("got %s over %s", resp.TLS.PeerCertificates[0].Subject.CommonName, body)
	}

	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}}}
	if _, err := old.Get("https://" + ln.Addr().String() + "/"); err == nil {
		t.Error("a TLS 1.1 client was served")
	}
}
