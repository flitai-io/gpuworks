package modelrepo

import "testing"

func TestIsCommitRevision(t *testing.T) {
	for revision, want := range map[string]bool{
		"48d788eca847d4d7548f375ad03d3c9312f6139e": true,
		"48D788E":   true,
		" a1b2c3d ": true,
		"a1b2c3":    false,
		"main":      false,
		"v1.0.0":    false,
		"":          false,
		"master":    false,
	} {
		if got := IsCommitRevision(revision); got != want {
			t.Errorf("IsCommitRevision(%q) = %v, want %v", revision, got, want)
		}
	}
}
