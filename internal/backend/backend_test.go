package backend

import (
	"errors"
	"testing"
)

func validSpec() Spec {
	return Spec{
		Image: "rocker/rstudio:4.5",
		UID:   4321,
		GID:   4321,
		Home:  Mount{Source: "/home/alice", Target: "/home/alice"},
		Files: []File{{Path: "/etc/passwd", Mode: 0o644, Content: []byte("alice:x:4321:4321::/home/alice:/bin/sh\n")}},
	}
}

func TestValidateAccepts(t *testing.T) {
	for name, mod := range map[string]func(*Spec){
		"plain":            func(*Spec) {},
		"digest image":     func(s *Spec) { s.Image = "ghcr.io/x/rstudio@sha256:abc123" },
		"named":            func(s *Spec) { s.Name = "atelier-k3j9" },
		"no files":         func(s *Spec) { s.Files = nil },
		"other target":     func(s *Spec) { s.Home.Target = "/home/rstudio" },
		"nfs home path":    func(s *Spec) { s.Home.Source = "/mnt/nfs/home/alice" },
		"file beside home": func(s *Spec) { s.Files[0].Path = "/home/alice2/x" },
	} {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mod(&s)
			if err := Validate(s); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// Every case here must be refused: each one would break an invariant (root, a mount
// that is not one user's home, a path that escapes, a URL-changing image reference).
func TestValidateDenies(t *testing.T) {
	for name, mod := range map[string]func(*Spec){
		"root uid":                func(s *Spec) { s.UID = 0 },
		"root gid":                func(s *Spec) { s.GID = 0 },
		"unset uid":               func(s *Spec) { s.UID = -1 },
		"no image":                func(s *Spec) { s.Image = "" },
		"image with query":        func(s *Spec) { s.Image = "x?fromImage=evil" },
		"image with dotdot":       func(s *Spec) { s.Image = "../../containers/create" },
		"image with inner dotdot": func(s *Spec) { s.Image = "a/../../containers/create" },
		"image with space":        func(s *Spec) { s.Image = "a b" },
		"host root as home":       func(s *Spec) { s.Home.Source = "/" },
		"relative home":           func(s *Spec) { s.Home.Source = "home/alice" },
		"unclean home":            func(s *Spec) { s.Home.Source = "/home/alice/../bob" },
		"empty home":              func(s *Spec) { s.Home.Source = "" },
		"container root":          func(s *Spec) { s.Home.Target = "/" },
		"relative target":         func(s *Spec) { s.Home.Target = "home" },
		"unclean target":          func(s *Spec) { s.Home.Target = "/home/./alice" },
		"relative file":           func(s *Spec) { s.Files[0].Path = "etc/passwd" },
		"unclean file":            func(s *Spec) { s.Files[0].Path = "/etc/../root/x" },
		"file in home":            func(s *Spec) { s.Files[0].Path = "/home/alice/.Rprofile" },
		"file is home":            func(s *Spec) { s.Files[0].Path = "/home/alice" },
		"setuid file":             func(s *Spec) { s.Files[0].Mode = 0o4755 },
		"bad container name":      func(s *Spec) { s.Name = "-x" },
		"slash container name":    func(s *Spec) { s.Name = "a/b" },
	} {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mod(&s)
			err := Validate(s)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("Validate = %v, want ErrInvalidSpec", err)
			}
		})
	}
}
