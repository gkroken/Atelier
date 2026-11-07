// Package session holds what identifies a session. Its ID is part of the session URL,
// /s/<id>/, which is expensive to change once people have bookmarks and open tabs.
package session

import (
	"crypto/rand"
	"encoding/base32"
	"regexp"
	"strings"
)

// IDLen is the length of a session ID: 128 random bits in base32 without padding.
const IDLen = 26

var (
	idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
	idRE       = regexp.MustCompile(`^[a-z2-7]{26}$`)
)

// NewID returns a new random session ID: 26 characters from a-z and 2-7. 128 bits makes
// IDs unguessable, but an ID is not a credential: every request is still checked
// against the session's owner.
func NewID() string {
	b := make([]byte, 16)
	// crypto/rand.Read never returns an error on Linux (Go 1.24+); it panics instead.
	_, _ = rand.Read(b)
	return strings.ToLower(idEncoding.EncodeToString(b))
}

// ValidID reports whether id has the shape NewID produces.
func ValidID(id string) bool { return idRE.MatchString(id) }
