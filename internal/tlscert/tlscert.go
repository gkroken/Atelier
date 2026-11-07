// Package tlscert serves Atelier's TLS certificate from files named in atelier.yaml,
// and picks up a renewed certificate (Let's Encrypt, or IT's yearly one) without a
// restart.
//
// The key file must not be readable or writable by other accounts on the host: it is
// refused at startup and on every reload. Group access is allowed, for the usual
// "ssl-cert" group arrangement.
package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// recheck is how often the files are looked at again for a change, at most.
const recheck = 5 * time.Second

// Source holds the current certificate.
type Source struct {
	certFile, keyFile string
	log               *slog.Logger
	now               func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   string // the files' sizes and modification times when cert was loaded
	checked time.Time
}

// New loads the certificate and key, or fails.
func New(certFile, keyFile string, log *slog.Logger) (*Source, error) {
	s := &Source{certFile: certFile, keyFile: keyFile, log: log, now: time.Now}
	stamp, err := s.stampFiles()
	if err != nil {
		return nil, err
	}
	cert, err := s.load()
	if err != nil {
		return nil, err
	}
	s.cert, s.stamp, s.checked = cert, stamp, s.now()
	return s, nil
}

// Config is a TLS server configuration using s: TLS 1.2 or later, Go's default cipher
// suites (all AEAD with forward secrecy for TLS 1.2), HTTP/2 and HTTP/1.1.
func (s *Source) Config() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// GetCertificate returns the current certificate, reloading it first if the files
// changed. A failed reload keeps serving the old certificate and logs why.
func (s *Source) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.checked) < recheck {
		return s.cert, nil
	}
	s.checked = now
	stamp, err := s.stampFiles()
	if err == nil && stamp == s.stamp {
		return s.cert, nil
	}
	if err == nil {
		var cert *tls.Certificate
		if cert, err = s.load(); err == nil {
			s.cert, s.stamp = cert, stamp
			s.log.Info("TLS certificate reloaded", "cert", s.certFile, "expires", cert.Leaf.NotAfter.Format(time.DateOnly))
			return s.cert, nil
		}
	}
	// Half-written renewals land here too; the next check tries again.
	s.log.Warn("TLS certificate not reloaded; still serving the previous one", "cert", s.certFile, "err", err)
	return s.cert, nil
}

func (s *Source) stampFiles() (string, error) {
	var stamp string
	for _, f := range []string{s.certFile, s.keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			return "", fmt.Errorf("tls: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("tls: %s is not a regular file", f)
		}
		if f == s.keyFile && fi.Mode().Perm()&0o007 != 0 {
			return "", fmt.Errorf("tls: key file %s is accessible to other accounts (mode %04o); chmod o-rwx it", f, fi.Mode().Perm())
		}
		stamp += fmt.Sprintf("%s %d %d;", f, fi.Size(), fi.ModTime().UnixNano())
	}
	return stamp, nil
}

func (s *Source) load() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: load %s and %s: %w", s.certFile, s.keyFile, err)
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, fmt.Errorf("tls: %s: %w", s.certFile, err)
		}
	}
	if s.now().After(cert.Leaf.NotAfter) {
		return nil, fmt.Errorf("tls: %s expired on %s", s.certFile, cert.Leaf.NotAfter.Format(time.DateOnly))
	}
	return &cert, nil
}
