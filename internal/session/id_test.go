package session

import "testing"

func TestNewIDIsValidAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10000; i++ {
		id := NewID()
		if !ValidID(id) {
			t.Fatalf("NewID() = %q, not valid", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}

func TestValidIDRefuses(t *testing.T) {
	ok := NewID()
	for _, id := range []string{
		"", ok[:25], ok + "a", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxy1",
		"abcdefghijklmnopqrstuvwxy8", "abcdefghijklmnopqrstuvwxy.", "../abcdefghijklmnopqrstuvw",
		"abcdefghijklmnopqrstuvwxy\n",
	} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}
