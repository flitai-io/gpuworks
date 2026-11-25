//go:build linux && dqlite

package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

func membershipRetryFixture(t *testing.T) (*Store, ClusterIdentity, string, []cluster.Member) {
	t.Helper()
	st, id, admissions := membershipFixture(t)
	live := []cluster.Member{{ID: 1, Address: admissions[0].Address, Role: "voter"}, {ID: 2, Address: admissions[1].Address, Role: "voter"}}
	return st, id, admissions[2].KeyFingerprint, live
}

// A database leadership change interrupts one live observation. A majority
// is available again well inside the request's budget, so the grant must
// complete instead of closing the member's business for that request.
func TestGrantClusterMembershipRetriesAnInterruptedLeaderObservation(t *testing.T) {
	for name, interruption := range map[string]error{
		"no leader":       dqlitedriver.ErrNoAvailableLeader,
		"bad connection":  driver.ErrBadConn,
		"connection lost": io.ErrUnexpectedEOF,
	} {
		for _, at := range []int32{1, 2} {
			t.Run(name+"/observation "+string(rune('0'+at)), func(t *testing.T) {
				st, id, requester, live := membershipRetryFixture(t)
				var calls atomic.Int32
				st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
					if calls.Add(1) == at {
						return nil, errors.Join(errors.New("live membership read"), interruption)
					}
					return slices.Clone(live), nil
				})
				revision, members, err := st.GrantClusterMembership(t.Context(), id, requester)
				if err != nil || revision == 0 || len(members) != 2 {
					t.Fatalf("interrupted observation escaped: rev=%d members=%d err=%v calls=%d", revision, len(members), err, calls.Load())
				}
				// The successful result comes from one complete sequence
				// started after the interruption: before and after.
				if want := at + 2; calls.Load() != want {
					t.Fatalf("live observations = %d, want %d (a full observe-grant-observe after the interruption)", calls.Load(), want)
				}
			})
		}
	}
}

// The grant transaction itself can lose its leader. The admission sequence
// only increases, so repeating the whole sequence is safe.
func TestGrantClusterMembershipRetriesAnInterruptedGrantTransaction(t *testing.T) {
	st, id, requester, live := membershipRetryFixture(t)
	var observations atomic.Int32
	st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
		observations.Add(1)
		return slices.Clone(live), nil
	})
	var failed atomic.Bool
	const callback = "test:membership_grant_interrupted"
	if err := st.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "cluster_admissions" && failed.CompareAndSwap(false, true) {
			_ = tx.AddError(driver.ErrBadConn)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.db.Callback().Update().Remove(callback) })
	before, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, requester)
	if err != nil {
		t.Fatal(err)
	}
	revision, members, err := st.GrantClusterMembership(t.Context(), id, requester)
	if err != nil || revision == 0 || len(members) != 2 || !failed.Load() {
		t.Fatalf("interrupted grant escaped: rev=%d members=%d err=%v injected=%v", revision, len(members), err, failed.Load())
	}
	if observations.Load() != 3 {
		t.Fatalf("live observations = %d, want 3: the failed attempt's first, then a complete pair", observations.Load())
	}
	after, err := st.ClusterAdmissionForVerifiedKey(t.Context(), id.ClusterID, requester)
	if err != nil || after.MaterialGrantSequence != before.MaterialGrantSequence+1 {
		t.Fatalf("grant sequence %d -> %d, want exactly one committed grant", before.MaterialGrantSequence, after.MaterialGrantSequence)
	}
}

// A member cut off from the majority sees the same interruption on every
// observation. It must still refuse inside its bounded window, well before
// the grant budget, so a direct write to it fails within three seconds.
func TestGrantClusterMembershipKeepsThePartitionedRefusalBounded(t *testing.T) {
	st, id, requester, _ := membershipRetryFixture(t)
	previousObserve, previousWindow := clusterMembershipObserveTimeout, clusterMembershipRetryWindow
	clusterMembershipObserveTimeout, clusterMembershipRetryWindow = 150*time.Millisecond, func() time.Duration { return 250 * time.Millisecond }
	t.Cleanup(func() {
		clusterMembershipObserveTimeout, clusterMembershipRetryWindow = previousObserve, previousWindow
	})
	var calls atomic.Int32
	st.bindClusterMembership(id, func(ctx context.Context) ([]cluster.Member, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, dqlitedriver.ErrNoAvailableLeader
	})
	started := time.Now()
	revision, members, err := st.GrantClusterMembership(t.Context(), id, requester)
	elapsed := time.Since(started)
	if !errors.Is(err, dqlitedriver.ErrNoAvailableLeader) || revision != 0 || members != nil {
		t.Fatalf("partitioned member returned rev=%d members=%v err=%v", revision, members, err)
	}
	if elapsed < 240*time.Millisecond || elapsed > 400*time.Millisecond || calls.Load() != 2 {
		t.Fatalf("partitioned refusal took %s over %d observations, want the 250ms window and 2 observations", elapsed, calls.Load())
	}
	if production := previousWindow(); production >= 3*time.Second || production <= previousObserve {
		t.Fatalf("retry window %s must exceed one observation (%s) and stay under the three-second isolated refusal", production, previousObserve)
	}
}

func TestGrantClusterMembershipDoesNotRetryDefinitiveFailures(t *testing.T) {
	unknown := errors.New("unclassified storage failure")
	for name, failure := range map[string]error{
		"admission rejected":      ErrClusterAdmissionRejected,
		"identity conflict":       ErrClusterIdentityConflict,
		"maintenance not serving": ErrMaintenanceNotServing,
		"business writes closed":  ErrBusinessWritesClosed,
		"maintenance changed":     ErrMaintenancePhaseChanged,
		"unknown error":           unknown,
		// A definitive refusal stays definitive when a driver error is joined to it.
		"closed joined with bad connection": errors.Join(ErrBusinessWritesClosed, driver.ErrBadConn),
	} {
		t.Run(name, func(t *testing.T) {
			st, id, requester, _ := membershipRetryFixture(t)
			var calls atomic.Int32
			st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
				calls.Add(1)
				return nil, failure
			})
			revision, members, err := st.GrantClusterMembership(t.Context(), id, requester)
			if !errors.Is(err, failure) || revision != 0 || members != nil || calls.Load() != 1 {
				t.Fatalf("definitive failure: rev=%d members=%v err=%v observations=%d, want it returned after one", revision, members, err, calls.Load())
			}
		})
	}
	t.Run("invalid topology", func(t *testing.T) {
		st, id, requester, live := membershipRetryFixture(t)
		var calls atomic.Int32
		st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
			calls.Add(1)
			bad := slices.Clone(live)
			bad[1].ID = bad[0].ID
			return bad, nil
		})
		if _, _, err := st.GrantClusterMembership(t.Context(), id, requester); err == nil || calls.Load() != 1 {
			t.Fatalf("invalid topology err=%v observations=%d, want a refusal after one", err, calls.Load())
		}
	})
	t.Run("revoked requester", func(t *testing.T) {
		st, id, requester, live := membershipRetryFixture(t)
		if _, err := st.RevokeClusterAdmission(t.Context(), id.ClusterID, requester); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
			calls.Add(1)
			return slices.Clone(live), nil
		})
		if _, _, err := st.GrantClusterMembership(t.Context(), id, requester); !errors.Is(err, ErrClusterAdmissionRejected) || calls.Load() != 1 {
			t.Fatalf("revoked requester err=%v observations=%d, want one rejected attempt", err, calls.Load())
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		st, id, requester, _ := membershipRetryFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		var calls atomic.Int32
		st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
			calls.Add(1)
			cancel()
			return nil, dqlitedriver.ErrNoAvailableLeader
		})
		if _, _, err := st.GrantClusterMembership(ctx, id, requester); err == nil || calls.Load() != 1 {
			t.Fatalf("canceled caller err=%v observations=%d, want a failure after one", err, calls.Load())
		}
	})
}

// The retry never extends the caller's own deadline.
func TestGrantClusterMembershipRetryStaysInsideTheCallerDeadline(t *testing.T) {
	st, id, requester, _ := membershipRetryFixture(t)
	st.bindClusterMembership(id, func(context.Context) ([]cluster.Member, error) {
		return nil, driver.ErrBadConn
	})
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := st.GrantClusterMembership(ctx, id, requester)
	if elapsed := time.Since(started); err == nil || elapsed > 400*time.Millisecond {
		t.Fatalf("retry ran %s past a 120ms caller deadline: %v", elapsed, err)
	}
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("exhausted retry returned %v, want the interruption that caused it", err)
	}
}
