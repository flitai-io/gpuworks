//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

func TestGrantClusterMembershipRetriesATransientViewChange(t *testing.T) {
	st, id, admissions := membershipFixture(t)
	requester := admissions[0].KeyFingerprint
	stable := []cluster.Member{{ID: 1, Address: admissions[0].Address, Role: "voter"}}
	changed := []cluster.Member{{ID: 1, Address: admissions[0].Address, Role: "voter"}, {ID: 2, Address: admissions[1].Address, Role: "voter"}}
	var calls atomic.Int32
	st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
		if calls.Add(1) == 1 {
			return slices.Clone(changed), nil
		}
		return slices.Clone(stable), nil
	})
	revision, members, err := st.GrantClusterMembership(t.Context(), id, requester)
	if err != nil || revision == 0 || len(members) != 1 || members[0].ID != 1 || calls.Load() < 3 {
		t.Fatalf("transient view change was not retried: rev=%d members=%#v err=%v calls=%d", revision, members, err, calls.Load())
	}
}

func TestGrantClusterMembershipObservationFailsBeforeTheGrantBudget(t *testing.T) {
	st, id, admissions := membershipFixture(t)
	previous, previousWindow := clusterMembershipObserveTimeout, clusterMembershipRetryWindow
	// The window bounds how long an interrupted observation is repeated.
	clusterMembershipObserveTimeout, clusterMembershipRetryWindow = 40*time.Millisecond, func() time.Duration { return 100 * time.Millisecond }
	t.Cleanup(func() { clusterMembershipObserveTimeout, clusterMembershipRetryWindow = previous, previousWindow })
	st.bindClusterMembership(id, func(ctx context.Context) ([]cluster.Member, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	started := time.Now()
	_, _, err := st.GrantClusterMembership(t.Context(), id, admissions[0].KeyFingerprint)
	if err == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("blocked observation waited for the full grant budget: %v after %s", err, time.Since(started))
	}
}

func TestGrantClusterMembershipReturnsOnlyLiveAdmittedMembers(t *testing.T) {
	st, id, admissions := membershipFixture(t)
	requester := admissions[2].KeyFingerprint
	if revision, members, err := st.GrantClusterMembership(t.Context(), id, requester); err == nil || revision != 0 || members != nil {
		t.Fatalf("SQL fixture invented live membership %d %#v %v", revision, members, err)
	}
	calls := 0
	live := []cluster.Member{{ID: 2, Address: "SEED.Example.:09010", Role: "voter"}, {ID: 1, Address: admissions[1].Address, Role: "stand-by"}}
	st.bindClusterMembership(id, func(ctx context.Context) ([]cluster.Member, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("unbounded live observation")
		}
		if remaining := time.Until(deadline); remaining > clusterMembershipObserveTimeout+20*time.Millisecond {
			t.Fatalf("live observation budget %s exceeds %s", remaining, clusterMembershipObserveTimeout)
		}
		calls++
		result := slices.Clone(live)
		if calls%2 == 0 {
			slices.Reverse(result)
		}
		return result, nil
	})
	revision, members, err := st.GrantClusterMembership(t.Context(), id, requester)
	expected := []ClusterTransportMember{{ID: 1, Address: admissions[1].Address, Role: "stand-by", KeyFingerprint: admissions[1].KeyFingerprint}, {ID: 2, Address: admissions[0].Address, Role: "voter", KeyFingerprint: admissions[0].KeyFingerprint}}
	if err != nil || revision != 3 || !reflect.DeepEqual(members, expected) || calls != 2 {
		t.Fatalf("live snapshot %d %#v %v calls%d", revision, members, err, calls)
	}
	granted, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, requester)
	if err != nil || granted.MaterialGrantSequence != 1 {
		t.Fatalf("discovery was not majority-granted %#v %v", granted, err)
	}
	members[0].Address = "mutated"
	if live[1].Address != admissions[1].Address {
		t.Fatal("result aliases source membership")
	}
	for _, mutate := range []func(*ClusterIdentity){func(v *ClusterIdentity) { v.ClusterID = strings.Repeat("f", 32) }, func(v *ClusterIdentity) { v.CAFingerprint = strings.Repeat("f", 64) }, func(v *ClusterIdentity) { v.Protocol++ }} {
		other := id
		mutate(&other)
		if revision, members, err := st.GrantClusterMembership(t.Context(), other, requester); !errors.Is(err, ErrClusterIdentityConflict) || revision != 0 || members != nil {
			t.Fatalf("transport authority replaced %d %#v %v", revision, members, err)
		}
	}
	if calls != 2 {
		t.Fatal("wrong bound authority queried membership")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if revision, members, err := st.GrantClusterMembership(canceled, id, requester); !errors.Is(err, context.Canceled) || revision != 0 || members != nil {
		t.Fatalf("canceled snapshot %d %#v %v", revision, members, err)
	}
}

func TestGrantClusterMembershipRejectsChangingAndUnauthorizedTopology(t *testing.T) {
	st, id, admissions := membershipFixture(t)
	requester := admissions[2].KeyFingerprint
	base := []cluster.Member{{ID: 1, Address: admissions[0].Address, Role: "voter"}, {ID: 2, Address: admissions[1].Address, Role: "spare"}}
	for _, name := range []string{"before failure", "after failure", "changed address", "changed role", "changed ID", "unknown member", "zero ID", "duplicate ID", "duplicate address", "invalid role", "empty", "oversized"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			failure := errors.New("injected live membership error")
			st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
				calls++
				live := slices.Clone(base)
				switch name {
				case "before failure":
					return nil, failure
				case "after failure":
					if calls == 2 {
						return nil, failure
					}
				case "changed address":
					if calls%2 == 0 {
						live[1].Address = admissions[2].Address
					}
				case "changed role":
					if calls%2 == 0 {
						live[1].Role = "voter"
					}
				case "changed ID":
					if calls%2 == 0 {
						live[1].ID = 3
					}
				case "unknown member":
					live[1].Address = "unknown.example:9010"
				case "zero ID":
					live[0].ID = 0
				case "duplicate ID":
					live[1].ID = live[0].ID
				case "duplicate address":
					live[1].Address = "SEED.Example.:09010"
				case "invalid role":
					live[1].Role = "standby"
				case "empty":
					return nil, nil
				case "oversized":
					return make([]cluster.Member, 513), nil
				}
				return live, nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			revision, members, err := st.GrantClusterMembership(ctx, id, requester)
			cancel()
			if err == nil || revision != 0 || members != nil {
				t.Fatalf("invalid topology partially returned %d %#v %v", revision, members, err)
			}
		})
	}
	calls := 0
	st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) { calls++; return base, nil })
	before, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, requester)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := st.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	lost := errors.New("committed membership grant acknowledgment lost")
	previous := st.db.Statement.ConnPool
	st.db.Statement.ConnPool = &catalogGCUnknownCommitPool{DB: pool, failure: lost}
	revision, members, err := st.GrantClusterMembership(t.Context(), id, requester)
	st.db.Statement.ConnPool = previous
	if !errors.Is(err, lost) || revision != 0 || members != nil || calls != 1 {
		t.Fatalf("unknown commit leaked topology %d %#v %v calls%d", revision, members, err, calls)
	}
	after, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, requester)
	if err != nil || after.MaterialGrantSequence != before.MaterialGrantSequence+1 {
		t.Fatalf("unknown-commit fixture did not commit %#v %v", after, err)
	}
	if _, _, err := st.GrantClusterMembership(t.Context(), id, requester); err != nil {
		t.Fatal("explicit fresh retry", err)
	}
	if _, err := st.RevokeClusterAdmission(t.Context(), id.ClusterID, admissions[1].KeyFingerprint); err != nil {
		t.Fatal(err)
	}
	if revision, members, err := st.GrantClusterMembership(t.Context(), id, requester); err == nil || revision != 0 || members != nil {
		t.Fatalf("revoked live member exported %d %#v %v", revision, members, err)
	}
	if _, err := st.RevokeClusterAdmission(t.Context(), id.ClusterID, requester); err != nil {
		t.Fatal(err)
	}
	if revision, members, err := st.GrantClusterMembership(t.Context(), id, requester); !errors.Is(err, ErrClusterAdmissionRejected) || revision != 0 || members != nil {
		t.Fatalf("revoked requester exported topology %d %#v %v", revision, members, err)
	}
}

func TestLiveClusterAdmissionBindingRejectsDuplicateKeysAndAddresses(t *testing.T) {
	live := []cluster.Member{{ID: 1, Address: "one.example:9010", Role: "voter"}, {ID: 2, Address: "two.example:9010", Role: "spare"}}
	valid := []ClusterAdmission{{KeyFingerprint: strings.Repeat("a", 64), Address: live[0].Address}, {KeyFingerprint: strings.Repeat("b", 64), Address: live[1].Address}}
	for _, name := range []string{"key", "address", "invalid key", "invalid address", "revoked"} {
		t.Run(name, func(t *testing.T) {
			admissions := slices.Clone(valid)
			switch name {
			case "key":
				admissions[1].KeyFingerprint = admissions[0].KeyFingerprint
			case "address":
				admissions[1].Address = admissions[0].Address
			case "invalid key":
				admissions[1].KeyFingerprint = "invalid"
			case "invalid address":
				admissions[1].Address = "TWO.example:9010"
			case "revoked":
				now := time.Now()
				admissions[1].RevokedAt = &now
			}
			if members, err := bindLiveClusterAdmissions(live, admissions); err == nil || members != nil {
				t.Fatalf("invalid admission binding %#v %v", members, err)
			}
		})
	}
	for _, role := range []string{"voter", "stand-by", "spare"} {
		members, err := normalizeLiveClusterMembers([]cluster.Member{{ID: 1, Address: "127.0.0.1:9010", Role: role}})
		if err != nil || len(members) != 1 || members[0].Role != role {
			t.Fatalf("valid role %s rejected %v", role, err)
		}
	}
}

func TestGrantClusterMembershipBoundsBothLiveObservations(t *testing.T) {
	st, id, _ := admissionFixture(t)
	probe := errors.New("stop after deadline inspection")
	checked := 0
	var expected time.Time
	st.bindClusterMembership(id, func(ctx context.Context) ([]cluster.Member, error) {
		checked++
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("live membership call has no deadline")
		}
		if !expected.IsZero() && !deadline.Equal(expected) {
			t.Fatalf("shorter caller deadline changed: %v want %v", deadline, expected)
		}
		if deadline.After(time.Now().Add(5 * time.Second)) {
			t.Fatal("live membership deadline exceeds operation bound")
		}
		return nil, probe
	})
	if revision, members, err := st.GrantClusterMembership(t.Context(), id, strings.Repeat("c", 64)); !errors.Is(err, probe) || revision != 0 || members != nil {
		t.Fatalf("bounded observation %#v %d %v", members, revision, err)
	}
	expected = time.Now().Add(200 * time.Millisecond)
	shorter, cancel := context.WithDeadline(t.Context(), expected)
	defer cancel()
	if _, _, err := st.GrantClusterMembership(shorter, id, strings.Repeat("c", 64)); !errors.Is(err, probe) {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if _, _, err := st.GrantClusterMembership(canceled, id, strings.Repeat("c", 64)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if checked != 2 {
		t.Fatalf("canceled operation invoked live callback: %d", checked)
	}
}

func TestGrantClusterMembershipKeysIncrementsTheSetInOneObservation(t *testing.T) {
	st, id, admissions := membershipFixture(t)
	live := []cluster.Member{
		{ID: 1, Address: admissions[0].Address, Role: "voter"},
		{ID: 2, Address: admissions[1].Address, Role: "voter"},
		{ID: 3, Address: admissions[2].Address, Role: "spare"},
	}
	calls := 0
	st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
		calls++
		return slices.Clone(live), nil
	})
	revision, members, err := st.GrantClusterMembershipKeys(t.Context(), id, []string{admissions[2].KeyFingerprint, admissions[0].KeyFingerprint})
	expected := []ClusterTransportMember{
		{ID: 1, Address: admissions[0].Address, Role: "voter", KeyFingerprint: admissions[0].KeyFingerprint},
		{ID: 2, Address: admissions[1].Address, Role: "voter", KeyFingerprint: admissions[1].KeyFingerprint},
		{ID: 3, Address: admissions[2].Address, Role: "spare", KeyFingerprint: admissions[2].KeyFingerprint},
	}
	if err != nil || revision != 3 || !reflect.DeepEqual(members, expected) || calls != 2 {
		t.Fatalf("set grant revision=%d members=%#v calls=%d err=%v", revision, members, calls, err)
	}
	for i, admission := range admissions {
		got, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, admission.KeyFingerprint)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if i != 1 {
			want = 1
		}
		if got.MaterialGrantSequence != want {
			t.Fatalf("key %d sequence=%d want %d", i, got.MaterialGrantSequence, want)
		}
	}
	before := calls
	for _, rejected := range [][]string{
		{admissions[0].KeyFingerprint, admissions[0].KeyFingerprint},
		{admissions[0].KeyFingerprint, "not-a-digest"},
		nil,
	} {
		if _, _, err := st.GrantClusterMembershipKeys(t.Context(), id, rejected); !errors.Is(err, ErrClusterAdmissionRejected) {
			t.Fatalf("rejected keys granted %#v: %v", rejected, err)
		}
	}
	if calls != before {
		t.Fatalf("rejected keys observed membership: %d to %d", before, calls)
	}
	for i, admission := range admissions {
		got, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, admission.KeyFingerprint)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if i != 1 {
			want = 1
		}
		if got.MaterialGrantSequence != want {
			t.Fatalf("rejected keys changed key %d sequence to %d", i, got.MaterialGrantSequence)
		}
	}

	st, id, admissions = membershipFixture(t)
	calls = 0
	live = []cluster.Member{
		{ID: 1, Address: admissions[0].Address, Role: "voter"},
		{ID: 2, Address: admissions[1].Address, Role: "voter"},
		{ID: 3, Address: admissions[2].Address, Role: "spare"},
	}
	st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
		calls++
		return slices.Clone(live), nil
	})
	if err := st.DB().Model(&ClusterAdmission{}).Where("key_fingerprint = ?", admissions[2].KeyFingerprint).UpdateColumn("material_grant_sequence", int64(math.MaxInt64)).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.GrantClusterMembershipKeys(t.Context(), id, []string{admissions[0].KeyFingerprint, admissions[2].KeyFingerprint}); !errors.Is(err, ErrClusterAdmissionRejected) || calls != 1 {
		t.Fatalf("exhausted key partially granted calls=%d err=%v", calls, err)
	}
	for i, admission := range admissions {
		got, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, admission.KeyFingerprint)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if i == 2 {
			want = math.MaxInt64
		}
		if got.MaterialGrantSequence != want {
			t.Fatalf("rolled-back key %d sequence=%d want %d", i, got.MaterialGrantSequence, want)
		}
	}
}
