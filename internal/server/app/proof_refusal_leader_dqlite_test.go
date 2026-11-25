//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The member that leads the database is revoked. Its own proof is refused
// and closes it at once, though it still leads and still reaches itself.
// When it stops, the two that remain elect a leader, remove it from the
// database, prove again and commit. While the revoked member is still in the
// database their proofs cannot complete, so they may close meanwhile if the
// election and the removal outlast the confirmation they hold; what is
// required is that they serve again afterwards. Three real voters.
func TestNativeRevokedLeaderClosesAndTheOthersKeepServing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	expected := f.expected()
	leader, err := f.databases[0].Leader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lead := -1
	for i, database := range f.databases {
		if database.ID == leader.ID {
			lead = i
		}
	}
	if lead < 0 {
		t.Fatalf("leader %d is not a fixture member", leader.ID)
	}
	rest := []int{(lead + 1) % 3, (lead + 2) % 3}
	members := make([]*refusalMember, 3)
	for i := range members {
		members[i] = f.refusalMember(t, i, time.Now, nil)
		members[i].mustProve(t, ctx, "initial proof")
	}

	if _, err := f.stores[lead].RevokeClusterAdmission(ctx, expected.ClusterID, f.keys[lead]); err != nil {
		t.Fatal(err)
	}
	revokedAt := time.Now()
	ready, err := members[lead].eligibility.Reprove(ctx)
	if ready || !store.RefusesMember(err, f.keys[lead]) {
		t.Fatalf("proof of the revoked leader: ready=%t err=%v", ready, err)
	}
	if members[lead].eligibility.Eligible() {
		t.Fatal("the revoked leader still admits requests")
	}
	if code, reason := members[lead].read(); code != http.StatusServiceUnavailable || reason != refusalMemberRevoked {
		t.Fatalf("a read on the revoked leader: %d, refusal %q", code, reason)
	}
	if still, err := f.databases[lead].Leader(ctx); err != nil || still.ID != f.databases[lead].ID {
		t.Fatalf("the revoked member was expected to lead the database still: %+v %v", still, err)
	}
	t.Logf("the revoked leader closed %v after its revocation", time.Since(revokedAt).Round(time.Millisecond))
	if journal := members[lead].journal.String(); count(journal, journalClosed) != 1 || !strings.Contains(journal, "caller=proof_failed") {
		t.Fatalf("the leader's close logged:\n%s", journal)
	}
	// Its writes are refused as any revoked member's are.
	setting := store.SettingKey("revoked-leader-setting")
	if err := f.stores[lead].SetBusinessSetting(ctx, setting, "by the revoked leader"); !errors.Is(err, store.ErrClusterAdmissionRejected) {
		t.Fatalf("a write by the revoked leader: %v", err)
	}

	// The others were not refused by the leader's revocation.
	for _, i := range rest {
		if _, _, err := f.proveMember(i)(ctx); store.Causes(err, store.CauseMemberRefused) != 0 {
			t.Fatalf("member %d read the leader's revocation as its own: %v", i, err)
		}
	}

	// The revoked leader stops. Nobody hands leadership over: the two that
	// remain elect one of themselves, and that leader removes the member.
	if err := f.databases[lead].Close(); err != nil {
		t.Fatal(err)
	}
	var elected uint64
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline) && elected == 0; time.Sleep(100 * time.Millisecond) {
		if now, err := f.databases[rest[0]].Leader(ctx); err == nil && (now.ID == f.databases[rest[0]].ID || now.ID == f.databases[rest[1]].ID) {
			elected = now.ID
		}
	}
	if elected == 0 {
		t.Fatal("the remaining members elected no leader")
	}
	removed := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !removed; time.Sleep(100 * time.Millisecond) {
		removed = f.databases[rest[0]].Remove(ctx, f.databases[lead].ID) == nil
	}
	if !removed {
		t.Fatal("the revoked leader was not removed from the database")
	}
	for _, i := range rest {
		proved := false
		var last error
		for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline) && !proved; time.Sleep(200 * time.Millisecond) {
			proved, last = members[i].eligibility.Reprove(ctx)
		}
		if !proved || !members[i].eligibility.Eligible() {
			t.Fatalf("member %d did not prove again after the removal: %v", i, last)
		}
		if code, _ := members[i].read(); code != http.StatusNoContent {
			t.Fatalf("member %d refuses reads after the removal: %d", i, code)
		}
	}
	if err := f.stores[rest[0]].SetBusinessSetting(ctx, setting, "by the remaining members"); err != nil {
		t.Fatalf("a write by a remaining member: %v", err)
	}
	if got := f.stores[rest[1]].GetSetting(setting); got != "by the remaining members" {
		t.Fatalf("the other remaining member reads %q", got)
	}
}
