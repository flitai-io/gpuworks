package store

import (
	"errors"
	"testing"
)

// The named refusal is still the sentinel for every caller that does not
// ask whose it is, with the sentinel's text, and a refusal is named once.
func TestClusterAdmissionRefusalIsTheSentinel(t *testing.T) {
	named := refusedKey(ErrClusterAdmissionRejected, "k1")
	var refusal *ClusterAdmissionRefusal
	if !errors.As(named, &refusal) || refusal.Key != "k1" || !errors.Is(named, ErrClusterAdmissionRejected) || named.Error() != ErrClusterAdmissionRejected.Error() {
		t.Fatalf("named refusal: %#v", named)
	}
	if again := refusedKey(named, "k2"); again != named {
		t.Fatalf("a named refusal was renamed: %#v", again)
	}
	other := errors.New("database is locked")
	if got := refusedKey(other, "k1"); got != other {
		t.Fatalf("another error was changed: %v", got)
	}
	if refusedKey(nil, "k1") != nil {
		t.Fatal("nil became an error")
	}
	if errors.Is(named, ErrClusterIdentityConflict) {
		t.Fatal("an admission refusal reads as an identity conflict")
	}
}
