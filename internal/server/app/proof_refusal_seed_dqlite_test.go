//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A seed member has no join record on disk, so before each proof it reads
// its own admission from the database directly. When that admission is
// revoked the read's answer is the same refusal the proof's grant would
// give for this member's key, and the member's proof returns it. Three real
// voters and the member's real candidate proof.
func TestNativeSeedMemberReadsItsOwnRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f, _, _ := newAliasBootstrapMaterialFixture(t, ctx, []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"})
	for i, bundle := range f.bundles {
		if err := replicateRequiredMaterials(ctx, bundle.dataDir, f.stores[i], bundle, "", true, func() {}, enrollmentNativeLog()); err != nil {
			t.Fatal(err)
		}
	}
	bundle := f.bundles[0]
	if _, err := os.Lstat(filepath.Join(bundle.dataDir, "gpuworks.db.join")); !os.IsNotExist(err) {
		t.Fatalf("the first member is expected to be a seed without a join record: %v", err)
	}
	seed := &App{cfg: Config{DataDir: bundle.dataDir}, store: f.stores[0], memberState: memberState{joinedClosed: true}, log: enrollmentNativeLog()}
	if ready, err := seed.CandidateMemberReady(ctx); !ready || err != nil {
		t.Fatalf("the seed's proof before the revocation: ready=%t err=%v", ready, err)
	}
	if current, id, key, cause := seed.candidateEnrollmentWithCause(ctx); current == nil || id == 0 || key != f.keys[0] || cause != nil {
		t.Fatalf("the seed's enrollment before the revocation: id=%d key=%q cause=%v", id, key, cause)
	}

	// Another member is revoked first: the seed's own read is untouched.
	if _, err := f.stores[0].RevokeClusterAdmission(ctx, bundle.info.ClusterID, f.keys[2]); err != nil {
		t.Fatal(err)
	}
	if current, _, _, cause := seed.candidateEnrollmentWithCause(ctx); current == nil || cause != nil {
		t.Fatalf("another member's revocation changed the seed's own read: %v", cause)
	}

	if _, err := f.stores[0].RevokeClusterAdmission(ctx, bundle.info.ClusterID, f.keys[0]); err != nil {
		t.Fatal(err)
	}
	current, id, key, cause := seed.candidateEnrollmentWithCause(ctx)
	var refusal *store.ClusterAdmissionRefusal
	if current != nil || id != 0 || key != f.keys[0] || !errors.As(cause, &refusal) || refusal.Key != f.keys[0] {
		t.Fatalf("the seed's read of its revoked admission: bundle=%t id=%d key=%q cause=%v", current != nil, id, key, cause)
	}
	ready, err := seed.CandidateMemberReady(ctx)
	if ready || !store.RefusesMember(err, f.keys[0]) || !errors.Is(err, store.ErrClusterAdmissionRejected) {
		t.Fatalf("the revoked seed's proof: ready=%t err=%v", ready, err)
	}
	// The refusal is the answer also when the proof's context has ended.
	ended, end := context.WithCancel(ctx)
	end()
	if ready, err := materialProofFailure(ended, key, cause); ready || !store.RefusesMember(err, f.keys[0]) {
		t.Fatalf("the refusal under an ended context: ready=%t err=%v", ready, err)
	}
}
