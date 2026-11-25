//go:build linux && dqlite

package store

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// transportRefreshFixture binds the periodic refresh to a real admission
// store. fail decides, per grant attempt, the error injected into the grant's
// admission update; applied counts the snapshots handed to the transport.
func transportRefreshFixture(t *testing.T, fail func(attempt int32) error) (st *Store, attempts, applied *atomic.Int32) {
	t.Helper()
	st, id, admissions := membershipFixture(t)
	attempts, applied = new(atomic.Int32), new(atomic.Int32)
	const callback = "test:transport_refresh_grant"
	if err := st.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "cluster_admissions" {
			return
		}
		if err := fail(attempts.Add(1)); err != nil {
			_ = tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.db.Callback().Update().Remove(callback) })
	st.bindTransportAuthorization(id, admissions[0].KeyFingerprint, func(snapshot identity.TransportPolicy) (identity.TransportPolicy, error) {
		if snapshot.SourceRevision == 0 || len(snapshot.AllowedKeys) != len(admissions) {
			t.Errorf("applied an incomplete snapshot: %+v", snapshot)
		}
		applied.Add(1)
		return snapshot, nil
	}, nil)
	return st, attempts, applied
}

// A database leadership change interrupts one refresh grant. The next attempt
// succeeds inside the same cycle, so the refresh must not fail: a failed
// refresh on a closed member withdraws its business.
func TestTransportRefreshRepeatsAnInterruptedGrantInsideItsCycle(t *testing.T) {
	for name, interruption := range map[string]error{
		"bad connection":  driver.ErrBadConn,
		"no leader":       dqlitedriver.ErrNoAvailableLeader,
		"connection lost": io.ErrUnexpectedEOF,
	} {
		t.Run(name, func(t *testing.T) {
			st, attempts, applied := transportRefreshFixture(t, func(attempt int32) error {
				if attempt == 1 {
					return interruption
				}
				return nil
			})
			var logged bytes.Buffer
			st.SetLogger(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := st.RefreshTransportAuthorization(ctx); err != nil {
				t.Fatalf("interrupted grant failed the refresh: %v after %d attempts", err, attempts.Load())
			}
			// Only the complete second attempt reaches the transport.
			if attempts.Load() != 2 || applied.Load() != 1 {
				t.Fatalf("grant attempts = %d, snapshots applied = %d, want 2 and 1", attempts.Load(), applied.Load())
			}
			if !strings.Contains(logged.String(), `msg="transport authorization refresh repeated after an interrupted grant"`) {
				t.Fatalf("the repeat was not logged at debug: %s", logged.String())
			}
		})
	}
}

// An interruption that does not clear ends the refresh with that error while
// its cycle is still live, exactly what a single failed attempt returned
// before: the closed member then withdraws as it always did.
func TestTransportRefreshStillFailsOnAPersistentInterruption(t *testing.T) {
	previous := clusterMembershipRetryWindow
	clusterMembershipRetryWindow = func() time.Duration { return 150 * time.Millisecond }
	t.Cleanup(func() { clusterMembershipRetryWindow = previous })
	st, attempts, applied := transportRefreshFixture(t, func(int32) error { return driver.ErrBadConn })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	err := st.RefreshTransportAuthorization(ctx)
	elapsed := time.Since(started)
	if !errors.Is(err, driver.ErrBadConn) || applied.Load() != 0 || attempts.Load() < 2 {
		t.Fatalf("persistent interruption: err=%v applied=%d attempts=%d", err, applied.Load(), attempts.Load())
	}
	if ctx.Err() != nil || elapsed > time.Second {
		t.Fatalf("refresh held its cycle for %s (context %v); it must fail inside the retry window with the cycle still live", elapsed, ctx.Err())
	}
}

func TestTransportRefreshDoesNotRepeatDefinitiveFailures(t *testing.T) {
	for name, failure := range map[string]error{
		"admission rejected":                ErrClusterAdmissionRejected,
		"identity conflict":                 ErrClusterIdentityConflict,
		"maintenance not serving":           ErrMaintenanceNotServing,
		"maintenance changed":               ErrMaintenancePhaseChanged,
		"business writes closed":            ErrBusinessWritesClosed,
		"unknown error":                     errors.New("unclassified storage failure"),
		"closed joined with bad connection": errors.Join(ErrBusinessWritesClosed, driver.ErrBadConn),
	} {
		t.Run(name, func(t *testing.T) {
			st, attempts, applied := transportRefreshFixture(t, func(int32) error { return failure })
			if err := st.RefreshTransportAuthorization(t.Context()); !errors.Is(err, failure) || attempts.Load() != 1 || applied.Load() != 0 {
				t.Fatalf("definitive failure: err=%v attempts=%d applied=%d, want it returned after one attempt", err, attempts.Load(), applied.Load())
			}
		})
	}
}

// The repeat lives inside the cycle's own deadline and never extends it.
func TestTransportRefreshRepeatStaysInsideTheCycleDeadline(t *testing.T) {
	st, _, applied := transportRefreshFixture(t, func(int32) error { return driver.ErrBadConn })
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := st.RefreshTransportAuthorization(ctx)
	if elapsed := time.Since(started); err == nil || applied.Load() != 0 || elapsed > 400*time.Millisecond {
		t.Fatalf("refresh ran %s past a 120ms cycle: err=%v applied=%d", elapsed, err, applied.Load())
	}
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("refresh returned %v, want the last attempt's own error", err)
	}
}

// A cancelled cycle is never repeated.
func TestTransportRefreshDoesNotRepeatAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	st, attempts, _ := transportRefreshFixture(t, func(int32) error {
		cancel()
		return driver.ErrBadConn
	})
	if err := st.RefreshTransportAuthorization(ctx); err == nil || attempts.Load() != 1 {
		t.Fatalf("cancelled cycle: err=%v attempts=%d, want one failed attempt", err, attempts.Load())
	}
}
