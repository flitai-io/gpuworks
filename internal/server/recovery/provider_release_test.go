package recovery

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Once the platform learns an instance was released (a confirmed delete or a
// sync elsewhere), the row tying the node to it is gone. Recovery must still
// know to stop instead of probing a released machine forever.
func TestReleasedProviderInstanceRowStopsRecoveryWithoutProbing(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	providers := &fakeProviders{}
	r.providers = providers
	node := newOfflineNode(t, st, true)
	p, err := st.CreateProvider(&store.Provider{Name: "cloud", Kind: "test", CredsCipher: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: p.ID, ExternalID: "released"}); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkProviderInstanceNode(p.ID, "released", node.ID); err != nil {
		t.Fatal(err)
	}
	probeAt := r.now().Add(-11 * time.Minute)
	if err := st.SetNodeRecoveryPhase(node.UUID, store.RecoveryPhaseUnreachable, 6, &probeAt, store.RecoveryError{Message: "offline"}); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteProviderInstance(p.ID, "released"); err != nil {
		t.Fatal(err)
	}

	pending, err := st.NodesNeedingRecovery()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range pending {
		if n.UUID == node.UUID {
			t.Fatalf("node of a released instance still queued for recovery: %+v", n)
		}
	}
	node, err = st.NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := r.recoverNode(context.Background(), node, true)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != "skipped" || outcome.SkippedCode != messages.RecoverySkippedStopped || outcome.SkippedReason != store.RecoveryStoppedProviderInstanceGone {
		t.Fatalf("outcome=%+v", outcome)
	}
	if providers.calls != 0 || fake.probes.Load() != 0 {
		t.Fatalf("provider_calls=%d probes=%d, want none", providers.calls, fake.probes.Load())
	}
}
