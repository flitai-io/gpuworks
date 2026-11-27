//go:build linux && dqlite

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// revokeThird revokes the third member's admission and leaves its process
// running and in the database's member list.
func (f *candidateGrantFixture) revokeThird(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := f.stores[0].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[2]); err != nil {
		t.Fatal(err)
	}
	live, err := f.databases[0].Members(ctx)
	if err != nil || len(live) != 3 {
		t.Fatalf("the revoked member is expected to stay in the database: %+v %v", live, err)
	}
}

// A member is revoked and stays in the database, running. It is not a member
// any more, and that says nothing about the other two: they go on proving
// past the proof's window and past the unconfirmed limit, and commit. The
// revoked member is in nobody's member list, which is what a connection
// between members is decided on; its own proof
// is refused, and removing it from the database afterwards changes nothing
// for the others. Three real voters, the real proof and the real grant; the
// workers' clock is the test's.
func TestNativeRevokedMemberStillInTheDatabaseDoesNotCloseTheOthers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	expected := f.expected()
	// Every member publishes its material, as in a running cluster; the
	// fixture leaves the first member out. A member proves against the
	// copies of the other admitted sources, so without this the second
	// member would have no source left once the third is revoked.
	f.publish(t, ctx, 0)
	clock := &materialTestClock{now: time.Now()}
	others := []*refusalMember{f.refusalMember(t, 0, clock.Now, nil), f.refusalMember(t, 1, clock.Now, nil)}
	for _, member := range others {
		member.mustProve(t, ctx, "initial proof")
	}
	f.revokeThird(t, ctx)

	_, members, err := f.stores[0].GrantClusterMembership(ctx, expected, f.keys[0])
	if err != nil || len(members) != 2 {
		t.Fatalf("grant beside a revoked member that is still in the database: %+v %v", members, err)
	}
	for _, member := range members {
		if member.ID == f.databases[2].ID || member.KeyFingerprint == f.keys[2] {
			t.Fatalf("the revoked member is in the member list: %+v", members)
		}
	}

	// Eight proofs ten seconds apart: 80 seconds, past the 15-second window
	// and the 60-second limit, and each one succeeds.
	for round := 1; round <= 8; round++ {
		clock.Advance(10 * time.Second)
		for i, member := range others {
			if ready, err := member.eligibility.Reprove(ctx); !ready || err != nil {
				t.Fatalf("round %d: proof of member %d beside a revoked member: ready=%t err=%v", round, i, ready, err)
			}
			if code, reason := member.read(); code != http.StatusNoContent {
				t.Fatalf("round %d: member %d refuses reads: %d %q", round, i, code, reason)
			}
		}
	}
	for i, member := range others {
		if journal := member.journal.String(); count(journal, journalClosed) != 0 {
			t.Fatalf("member %d closed:\n%s", i, journal)
		}
	}

	setting := store.SettingKey("beside-a-revoked-member")
	if err := f.stores[0].SetBusinessSetting(ctx, setting, "committed"); err != nil {
		t.Fatalf("a write by a member that remains: %v", err)
	}
	if got := f.stores[1].GetSetting(setting); got != "committed" {
		t.Fatalf("the other member that remains reads %q", got)
	}

	// The revoked member itself is refused, by its key.
	if ready, _, err := f.proveMember(2)(ctx); ready || !store.RefusesMember(err, f.keys[2]) {
		t.Fatalf("the revoked member's own proof: ready=%t err=%v", ready, err)
	}

	// It leaves the database: nothing changes for the others.
	removed := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !removed; time.Sleep(100 * time.Millisecond) {
		removed = f.databases[0].Remove(ctx, f.databases[2].ID) == nil
	}
	if !removed {
		t.Fatal("the revoked member was not removed from the database")
	}
	clock.Advance(10 * time.Second)
	for i, member := range others {
		proved := false
		var last error
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !proved; time.Sleep(200 * time.Millisecond) {
			proved, last = member.eligibility.Reprove(ctx)
		}
		if !proved || !member.eligibility.Eligible() {
			t.Fatalf("member %d after the removal: %v", i, last)
		}
	}
	if _, after, err := f.stores[0].GrantClusterMembership(ctx, expected, f.keys[0]); err != nil || len(after) != 2 {
		t.Fatalf("the member list after the removal: %+v %v", after, err)
	}
}

// A member that publishes material is revoked while another member's proof
// is between its two grants. The closing grant names the revoked source's
// key and is refused for it: that proof does not complete and does not close
// the member, and the next one, which no longer names the key, succeeds.
func TestNativeRevokedSourceKeyMidProofIsFollowedByASuccessfulProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	member := f.refusalMember(t, 0, time.Now, nil)
	member.mustProve(t, ctx, "initial proof")
	var once sync.Once
	revoked := make(chan error, 1)
	f.mu.Lock()
	f.before = func() {
		once.Do(func() {
			_, err := f.stores[0].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[2])
			revoked <- err
		})
	}
	f.mu.Unlock()
	ready, err := member.eligibility.Reprove(ctx)
	select {
	case revokeErr := <-revoked:
		if revokeErr != nil {
			t.Fatal(revokeErr)
		}
	default:
		t.Fatal("the proof did not reach its peer requests, so nothing was revoked under it")
	}
	if ready || store.Causes(err, store.CauseMemberRefused) != 0 || !member.eligibility.Eligible() {
		t.Fatalf("the proof under which a member was revoked: ready=%t err=%v eligible=%t", ready, err, member.eligibility.Eligible())
	}
	if ready, err := member.eligibility.Reprove(ctx); !ready || err != nil {
		t.Fatalf("the proof after the revocation: ready=%t err=%v", ready, err)
	}
	if journal := member.journal.String(); count(journal, journalClosed) != 0 {
		t.Fatalf("a close was logged:\n%s", journal)
	}
}

// A member of the database that has no admission at all is unknown, not
// revoked: every grant fails, no member list is given out, and the other
// members close when the confirmation they hold runs out. The state is made
// on the real database by deleting the member's admission row.
func TestNativeUnknownMemberInTheDatabaseStillFailsEveryGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	expected := f.expected()
	clock := &materialTestClock{now: time.Now()}
	member := f.refusalMember(t, 0, clock.Now, nil)
	member.mustProve(t, ctx, "initial proof")
	deleted := f.stores[0].DB().WithContext(ctx).Where("key_fingerprint = ?", f.keys[2]).Delete(&store.ClusterAdmission{})
	if deleted.Error != nil || deleted.RowsAffected != 1 {
		t.Fatalf("delete the third member's admission row: %d %v", deleted.RowsAffected, deleted.Error)
	}
	_, members, err := f.stores[0].GrantClusterMembership(ctx, expected, f.keys[0])
	if err == nil || members != nil {
		t.Fatalf("grant beside an unknown member of the database: %+v %v", members, err)
	}
	if store.Causes(err, store.CauseMemberRefused) != 0 || store.GrantUndecided.Unanswered(err) {
		t.Fatalf("the failure reads as a refusal of a member or as no answer: %v", err)
	}
	if ready, err := member.eligibility.Reprove(ctx); ready || err != nil {
		t.Fatalf("proof beside an unknown member: ready=%t err=%v", ready, err)
	}
	if !member.eligibility.Eligible() {
		t.Fatal("the member closed before its confirmation ran out")
	}
	clock.Advance(materialEligibilityWindow + time.Second)
	if member.eligibility.Eligible() {
		t.Fatal("the member is still open past its confirmation beside an unknown member of the database")
	}
	if code, reason := member.read(); code != http.StatusServiceUnavailable || reason == refusalMemberRevoked {
		t.Fatalf("a read past the window: %d %q", code, reason)
	}
}

// The material majority is counted over admitted members. With one of three
// revoked, two are admitted and the proof needs the one other admitted
// member's copy: the revoked member's copy is not asked for and does not
// count, although that member runs and holds the same bytes. So when the
// other admitted member cannot be reached the proof gets no answer, while
// the database itself still has two of its three voters.
func TestNativeRevokedMembersMaterialCopyDoesNotCountTowardTheMajority(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	if ready, _, err := f.proveMember(0)(ctx); !ready || err != nil {
		t.Fatalf("proof with three admitted members: ready=%t err=%v", ready, err)
	}
	f.revokeThird(t, ctx)
	before := f.asked[2].Load()
	if ready, _, err := f.proveMember(0)(ctx); !ready || err != nil {
		t.Fatalf("proof with two admitted members: ready=%t err=%v", ready, err)
	}
	if asked := f.asked[2].Load() - before; asked != 0 {
		t.Fatalf("the revoked member's endpoint was asked %d times", asked)
	}

	f.down[1].Store(true)
	before, other := f.asked[2].Load(), f.asked[1].Load()
	ready, _, err := f.proveMember(0)(ctx)
	if ready || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("proof with the only other admitted member unreachable: ready=%t err=%v, want no answer", ready, err)
	}
	if f.asked[1].Load() == other {
		t.Fatal("the other admitted member was not asked")
	}
	if asked := f.asked[2].Load() - before; asked != 0 {
		t.Fatalf("the revoked member's copy was asked for (%d requests) when the admitted one could not be reached", asked)
	}
	if leader, err := f.databases[0].Leader(ctx); err != nil || leader.ID == 0 {
		t.Fatalf("the database is expected to keep a leader with two of three voters: %+v %v", leader, err)
	}

	f.down[1].Store(false)
	if ready, _, err := f.proveMember(0)(ctx); !ready || err != nil {
		t.Fatalf("proof once the other admitted member answers again: ready=%t err=%v", ready, err)
	}
}

// A replication pass on a member that remains, with a revoked member still
// in the database: the pass completes over the admitted members and is not
// a failure that closes the member. Three real voters and the real pass.
func TestNativeReplicationPassBesideARevokedMemberDoesNotCloseTheMember(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f, _, _ := newAliasBootstrapMaterialFixture(t, ctx, []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"})
	pass := func(i int) error {
		return replicateRequiredMaterials(ctx, f.bundles[i].dataDir, f.stores[i], f.bundles[i], "", true, func() {}, enrollmentNativeLog())
	}
	for i := range f.bundles {
		if err := pass(i); err != nil {
			t.Fatalf("first pass of member %d: %v", i, err)
		}
	}
	if _, err := f.stores[0].RevokeClusterAdmission(ctx, f.bundles[0].info.ClusterID, f.keys[2]); err != nil {
		t.Fatal(err)
	}
	if live, err := f.databases[0].Members(ctx); err != nil || len(live) != 3 {
		t.Fatalf("the revoked member is expected to stay in the database: %+v %v", live, err)
	}
	for _, i := range []int{0, 1} {
		err := pass(i)
		if materialReplicationFailureInvalidates(err) {
			t.Fatalf("pass of member %d beside a revoked member closes the member: %v", i, err)
		}
		if err != nil {
			t.Fatalf("pass of member %d beside a revoked member: %v", i, err)
		}
	}
	// The revoked member's own pass is refused, and that does close it.
	if err := pass(2); !errors.Is(err, store.ErrClusterAdmissionRejected) || !materialReplicationFailureInvalidates(err) {
		t.Fatalf("pass of the revoked member: %v", err)
	}
}

// quorumFacts projects the facts route's answer from the real database: the
// leader's own quorum proof and the admission rows, as the route's reader
// hands them over. The store's read under the route is tested in the store.
func (f *candidateGrantFixture) quorumFacts(t *testing.T, ctx context.Context, leader, requester int) (int, error) {
	t.Helper()
	proof, err := f.databases[leader].WithLocalQuorumReadSnapshot(ctx, func(context.Context, *sql.Tx, cluster.QuorumProof) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var admissions []store.ClusterAdmission
	if err := f.stores[0].DB().WithContext(ctx).Order("key_fingerprint").Find(&admissions).Error; err != nil {
		t.Fatal(err)
	}
	phase, err := f.stores[0].ReadMaintenancePhase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts, _, _, err := projectClosedQuorumFacts(f.expected(), strings.Repeat("a", 64), false, phase, admissions, proof, f.keys[requester], f.keys[leader])
	if err != nil {
		return 0, err
	}
	// As a client reads it: the answer must decode, which among other
	// things requires the leader to be in the member list.
	wire, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := enrollment.DecodeClusterQuorumFacts(wire)
	if err != nil {
		t.Fatalf("a client rejects the projected facts: %v\n%s", err, wire)
	}
	return len(decoded.Members), nil
}

func (f *candidateGrantFixture) leaderIndex(t *testing.T, ctx context.Context) int {
	t.Helper()
	leader, err := f.databases[0].Leader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, database := range f.databases {
		if database.ID == leader.ID {
			return i
		}
	}
	t.Fatalf("leader %d is not a fixture member", leader.ID)
	return -1
}

// The facts route, which cluster status and a member's discovery read,
// answers beside a revoked member that is still in the database: the answer
// lists the admitted members. Three real voters, the leader's real proof.
func TestNativeQuorumFactsBesideARevokedMemberListTheAdmittedMembers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	lead := f.leaderIndex(t, ctx)
	other, gone := (lead+1)%3, (lead+2)%3
	if n, err := f.quorumFacts(t, ctx, lead, other); err != nil || n != 3 {
		t.Fatalf("facts with three admitted members: %d members %v", n, err)
	}
	if _, err := f.stores[lead].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[gone]); err != nil {
		t.Fatal(err)
	}
	if n, err := f.quorumFacts(t, ctx, lead, other); err != nil || n != 2 {
		t.Fatalf("facts beside a revoked member that is still in the database: %d members %v", n, err)
	}
	if _, err := f.quorumFacts(t, ctx, lead, gone); !errors.Is(err, store.ErrClusterAdmissionRejected) {
		t.Fatalf("facts for the revoked member: %v", err)
	}
}

// The member that leads the database is revoked and keeps leading. Every
// grant of the other two commits through it, and they go on proving and
// committing. It is not a provider of facts: the facts route has no answer
// until an admitted member leads. Three real voters.
func TestNativeRevokedDatabaseLeaderDoesNotCloseTheOthers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	f.publish(t, ctx, 0)
	lead := f.leaderIndex(t, ctx)
	rest := []int{(lead + 1) % 3, (lead + 2) % 3}
	members := map[int]*refusalMember{}
	for _, i := range rest {
		members[i] = f.refusalMember(t, i, time.Now, nil)
		members[i].mustProve(t, ctx, "initial proof")
	}
	if _, err := f.stores[lead].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[lead]); err != nil {
		t.Fatal(err)
	}
	if still := f.leaderIndex(t, ctx); still != lead {
		t.Fatalf("the revoked member was expected to lead the database still; member %d leads", still)
	}
	for _, i := range rest {
		if ready, err := members[i].eligibility.Reprove(ctx); !ready || err != nil {
			t.Fatalf("proof of member %d with a revoked leader: ready=%t err=%v", i, ready, err)
		}
		_, list, err := f.stores[i].GrantClusterMembership(ctx, f.expected(), f.keys[i])
		if err != nil || len(list) != 2 {
			t.Fatalf("grant of member %d through a revoked leader: %+v %v", i, list, err)
		}
		if journal := members[i].journal.String(); count(journal, journalClosed) != 0 {
			t.Fatalf("member %d closed:\n%s", i, journal)
		}
	}
	setting := store.SettingKey("through-a-revoked-leader")
	if err := f.stores[rest[0]].SetBusinessSetting(ctx, setting, "committed"); err != nil {
		t.Fatalf("a write by a member that remains: %v", err)
	}
	if got := f.stores[rest[1]].GetSetting(setting); got != "committed" {
		t.Fatalf("the other member that remains reads %q", got)
	}
	if n, err := f.quorumFacts(t, ctx, lead, rest[0]); !errors.Is(err, store.ErrClusterIdentityConflict) {
		t.Fatalf("facts from the revoked leader: %d members, %v, want %v", n, err, store.ErrClusterIdentityConflict)
	}
}

// A member that publishes no material is revoked while another member's
// proof is between its two grants. The proof's grants do not name its key,
// so the closing grant succeeds with a shorter member list: the proof ends
// without an answer and does not close the member, and the next one
// succeeds.
func TestNativeMemberListShrinksMidProofThenTheNextProofSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	f := newCandidateGrantFixture(t, ctx)
	unpublished := f.stores[0].DB().WithContext(ctx).Where("source_member_id = ?", int64(f.databases[2].ID)).Delete(&store.MaterialManifestRecord{})
	if unpublished.Error != nil || unpublished.RowsAffected != 1 {
		t.Fatalf("withdraw the third member's manifest pointer: %d %v", unpublished.RowsAffected, unpublished.Error)
	}
	member := f.refusalMember(t, 0, time.Now, nil)
	member.mustProve(t, ctx, "initial proof")
	var once sync.Once
	revoked := make(chan error, 1)
	f.mu.Lock()
	f.before = func() {
		once.Do(func() {
			_, err := f.stores[0].RevokeClusterAdmission(ctx, f.expected().ClusterID, f.keys[2])
			revoked <- err
		})
	}
	f.mu.Unlock()
	ready, err := member.eligibility.Reprove(ctx)
	select {
	case revokeErr := <-revoked:
		if revokeErr != nil {
			t.Fatal(revokeErr)
		}
	default:
		t.Fatal("the proof did not reach its peer requests, so nothing was revoked under it")
	}
	if ready || !errors.Is(err, context.DeadlineExceeded) || !member.eligibility.Eligible() {
		t.Fatalf("the proof under which the member list shrank: ready=%t err=%v eligible=%t, want no answer", ready, err, member.eligibility.Eligible())
	}
	if ready, err := member.eligibility.Reprove(ctx); !ready || err != nil {
		t.Fatalf("the proof after the list shrank: ready=%t err=%v", ready, err)
	}
	if journal := member.journal.String(); count(journal, journalClosed) != 0 {
		t.Fatalf("a close was logged:\n%s", journal)
	}
}
