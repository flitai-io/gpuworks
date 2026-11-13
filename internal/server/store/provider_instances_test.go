package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func providerInstanceIDs(t *testing.T, s *Store, providerID uint) map[string]uint {
	t.Helper()
	rows, err := s.ProviderInstances(providerID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]uint, len(rows))
	for _, row := range rows {
		out[row.ExternalID] = row.NodeID
	}
	return out
}

func newProviderWithInstances(t *testing.T, s *Store, externalIDs ...string) *Provider {
	t.Helper()
	p, err := s.CreateProvider(&Provider{Name: "cloud", Kind: "test", CredsCipher: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range externalIDs {
		if err := s.UpsertProviderInstance(&ProviderInstance{ProviderID: p.ID, ExternalID: id, Power: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// collectEvents drains what the bus delivered within a short window.
func collectEvents(sub *bus.Subscription) []bus.Event {
	var out []bus.Event
	timeout := time.After(200 * time.Millisecond)
	for {
		select {
		case ev := <-sub.C():
			out = append(out, ev)
		case <-timeout:
			return out
		}
	}
}

func TestSyncProviderInstancesRemovesUnlistedRowsAndKeepsLinks(t *testing.T) {
	s := newTestStore(t)
	kept, err := s.RegisterNode("node-kept", HashToken("kept"), "kept")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.RegisterNode("node-gone", HashToken("gone"), "gone")
	if err != nil {
		t.Fatal(err)
	}
	probedAt := time.Now().UTC().Truncate(time.Second)
	if err := s.SetNodeRecoveryPhase("node-gone", RecoveryPhaseUnreachable, 3, &probedAt, "dial tcp: i/o timeout"); err != nil {
		t.Fatal(err)
	}
	p := newProviderWithInstances(t, s, "inst-kept", "inst-gone", "inst-unlinked")
	other := newProviderWithInstances(t, s, "inst-gone")
	if err := s.LinkProviderInstanceNode(p.ID, "inst-kept", kept.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkProviderInstanceNode(p.ID, "inst-gone", gone.ID); err != nil {
		t.Fatal(err)
	}
	sub := s.Bus.Subscribe(16, "node")
	defer sub.Close()

	// The listing omits inst-gone and inst-unlinked and carries a new one. The
	// node id in the listing is ignored: links belong to the stored row.
	err = s.SyncProviderInstances(p.ID, []ProviderInstance{
		{ExternalID: "inst-kept", Power: "stopped", NodeID: 999},
		{ExternalID: "inst-new", Power: "running"},
	})
	if err != nil {
		t.Fatalf("SyncProviderInstances: %v", err)
	}
	if got, want := providerInstanceIDs(t, s, p.ID), map[string]uint{"inst-kept": kept.ID, "inst-new": 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	rows, _ := s.ProviderInstances(p.ID)
	if rows[0].ExternalID != "inst-kept" || rows[0].Power != "stopped" {
		t.Fatalf("kept row not refreshed: %+v", rows[0])
	}
	if got := providerInstanceIDs(t, s, other.ID); !reflect.DeepEqual(got, map[string]uint{"inst-gone": 0}) {
		t.Fatalf("another provider's rows changed: %v", got)
	}

	goneNode, err := s.NodeByUUID("node-gone")
	if err != nil {
		t.Fatalf("linked node of a removed row must survive: %v", err)
	}
	if goneNode.RecoveryPhase != RecoveryPhaseStopped || goneNode.LastRecoveryError != RecoveryStoppedProviderInstanceGone {
		t.Fatalf("removed row's node recovery = %q/%q", goneNode.RecoveryPhase, goneNode.LastRecoveryError)
	}
	if goneNode.RecoveryAttempts != 3 || goneNode.LastProbeAt == nil || !goneNode.LastProbeAt.Equal(probedAt) {
		t.Fatalf("recovery bookkeeping rewritten: attempts=%d last_probe=%v", goneNode.RecoveryAttempts, goneNode.LastProbeAt)
	}
	keptNode, err := s.NodeByUUID("node-kept")
	if err != nil || keptNode.RecoveryPhase != "" {
		t.Fatalf("kept row's node = %+v, err=%v", keptNode, err)
	}
	var stopped []string
	for _, ev := range collectEvents(sub) {
		if ev.Reason == "recovery:"+RecoveryPhaseStopped {
			stopped = append(stopped, ev.Key)
		}
	}
	if !reflect.DeepEqual(stopped, []string{"node-gone"}) {
		t.Fatalf("recovery stopped events = %v", stopped)
	}

	// A successful empty listing means everything was released.
	if err := s.SyncProviderInstances(p.ID, nil); err != nil {
		t.Fatalf("SyncProviderInstances(empty): %v", err)
	}
	if got := providerInstanceIDs(t, s, p.ID); len(got) != 0 {
		t.Fatalf("rows after empty listing = %v", got)
	}
	if keptNode, err = s.NodeByUUID("node-kept"); err != nil || keptNode.RecoveryPhase != RecoveryPhaseStopped {
		t.Fatalf("node of the last released row = %+v, err=%v", keptNode, err)
	}
	if got := providerInstanceIDs(t, s, other.ID); len(got) != 1 {
		t.Fatalf("another provider's rows changed: %v", got)
	}
}

func TestSyncProviderInstancesRollsBackOnFailure(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RegisterNode("node-1", HashToken("n1"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	p := newProviderWithInstances(t, s, "inst-a", "inst-b")
	if err := s.LinkProviderInstanceNode(p.ID, "inst-b", n.ID); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected delete failure")
	if err := s.DB().Callback().Delete().Before("gorm:delete").Register("test:fail_provider_instance_delete", func(db *gorm.DB) {
		if db.Statement.Table == "provider_instances" {
			_ = db.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}

	err = s.SyncProviderInstances(p.ID, []ProviderInstance{{ExternalID: "inst-a", Power: "stopped"}, {ExternalID: "inst-c"}})
	if !errors.Is(err, injected) {
		t.Fatalf("SyncProviderInstances err = %v, want injected failure", err)
	}
	rows, err := s.ProviderInstances(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ExternalID != "inst-a" || rows[0].Power != "running" || rows[1].ExternalID != "inst-b" || rows[1].NodeID != n.ID {
		t.Fatalf("rows after rolled back sync = %+v", rows)
	}
	if node, err := s.NodeByUUID("node-1"); err != nil || node.RecoveryPhase != "" {
		t.Fatalf("node after rolled back sync = %+v, err=%v", node, err)
	}

	err = s.DeleteProviderInstance(p.ID, "inst-b")
	if !errors.Is(err, injected) {
		t.Fatalf("DeleteProviderInstance err = %v, want injected failure", err)
	}
	if got := providerInstanceIDs(t, s, p.ID); !reflect.DeepEqual(got, map[string]uint{"inst-a": 0, "inst-b": n.ID}) {
		t.Fatalf("rows after failed delete = %v", got)
	}
}

func TestDeleteProviderInstanceRemovesOnlyThatRow(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RegisterNode("node-1", HashToken("n1"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	p := newProviderWithInstances(t, s, "inst-a", "inst-b")
	other := newProviderWithInstances(t, s, "inst-a")
	if err := s.LinkProviderInstanceNode(p.ID, "inst-a", n.ID); err != nil {
		t.Fatal(err)
	}
	sub := s.Bus.Subscribe(16, "node", "provider")
	defer sub.Close()

	if err := s.DeleteProviderInstance(p.ID, "inst-a"); err != nil {
		t.Fatalf("DeleteProviderInstance: %v", err)
	}
	if got := providerInstanceIDs(t, s, p.ID); !reflect.DeepEqual(got, map[string]uint{"inst-b": 0}) {
		t.Fatalf("rows = %v", got)
	}
	if got := providerInstanceIDs(t, s, other.ID); !reflect.DeepEqual(got, map[string]uint{"inst-a": 0}) {
		t.Fatalf("another provider's rows changed: %v", got)
	}
	node, err := s.NodeByUUID("node-1")
	if err != nil {
		t.Fatalf("linked node must survive: %v", err)
	}
	if node.RecoveryPhase != RecoveryPhaseStopped || node.LastRecoveryError != RecoveryStoppedProviderInstanceGone {
		t.Fatalf("node recovery = %q/%q", node.RecoveryPhase, node.LastRecoveryError)
	}
	if _, err := s.ProviderInstanceForNode(n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ProviderInstanceForNode err = %v, want ErrNotFound", err)
	}
	got := map[string]string{}
	for _, ev := range collectEvents(sub) {
		got[ev.Topic] = ev.Key + " " + ev.Reason
	}
	want := map[string]string{"node": "node-1 recovery:stopped", "provider": uintKey(p.ID) + " instance_deleted"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	// Releasing an instance whose row is already gone still succeeds.
	if err := s.DeleteProviderInstance(p.ID, "inst-a"); err != nil {
		t.Fatalf("DeleteProviderInstance(absent): %v", err)
	}
	if got := providerInstanceIDs(t, s, p.ID); !reflect.DeepEqual(got, map[string]uint{"inst-b": 0}) {
		t.Fatalf("rows after repeated delete = %v", got)
	}
}
