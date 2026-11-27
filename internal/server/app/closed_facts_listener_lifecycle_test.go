//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

type closedFactsClassifySession struct {
	*closedFactsRouteSession
	classify func(context.Context) (*store.Store, bool, error)
}

func (s *closedFactsClassifySession) OpenClassifiedStore(ctx context.Context, _ *bus.Bus) (*store.Store, bool, error) {
	return s.classify(ctx)
}

func TestClosedFactsListenerReplacementRetainsTimedOutOwnerThenReusesAddress(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	f.bundle.info.ListenAddress = testport.Address(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	f.session.hold = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}
	old := f.start(t)
	a := newWaitingClosedMemberApp(&memberRuntime{member: closedMember{cfg: Config{Addr: "127.0.0.1:0"}, log: nil}}, f.session)
	a.enrollment = old
	requests := newMaterialRequestTracker()
	handler, err := closedQuorumFactsHandler(f.bundle, f.session, requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Post(enrollment.ClusterQuorumFactsPath, handler)
	prepared, err := newEnrollmentServer(f.bundle.info.ListenAddress, router, f.bundle.tls, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared.materialRequests = requests
	prepared.materialReceiver, err = newMaterialReceiver(t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	client := f.client(t, &f.requesterPair)
	outcome := make(chan error, 1)
	go func() {
		response, _, err := closedFactsRouteRequest(t, client, old, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
		if err == nil && response.StatusCode == http.StatusOK {
			err = errors.New("canceled facts returned HTTP200")
		}
		outcome <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("original signed reader not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	err = a.replaceClosedEnrollment(ctx, prepared)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held cleanup cause lost: %v", err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("original tracker did not cancel held reader")
	}
	if a.enrollment != old || prepared.run != nil || old.materialReceiver != nil {
		t.Fatal("timeout discarded old owner or started prepared listener")
	}
	old.materialRequests.mu.Lock()
	active, closed := old.materialRequests.active, old.materialRequests.closed
	old.materialRequests.mu.Unlock()
	if active != 1 || !closed {
		t.Fatalf("timed out owner tracking active=%d closed=%v", active, closed)
	}
	finish()
	select {
	case <-outcome:
	case <-time.After(time.Second):
		t.Fatal("actual held handler did not return")
	}
	f.session.mu.Lock()
	f.session.hold = nil
	f.session.mu.Unlock()
	retry, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := a.replaceClosedEnrollment(retry, prepared); err != nil {
		t.Fatal("actual drained owner could not transfer", err)
	}
	if a.enrollment != prepared || prepared.run != nil {
		t.Fatal("replacement was not published unstarted")
	}
	if err := prepared.Start(t.Context()); err != nil {
		t.Fatal("same-address material listener did not start", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := prepared.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if prepared.Addr() != old.Addr() {
		t.Fatalf("listener changed original address old=%s new=%s", old.Addr(), prepared.Addr())
	}
	response, body, err := closedFactsRouteRequest(t, client, prepared, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("material listener lost fixed facts route response=%v body=%s err=%v", response, body, err)
	}
	if _, err := enrollment.DecodeClusterQuorumFacts(body); err != nil {
		t.Fatal(err)
	}
	// The material listener keeps optional bootstrap TLS while the route still
	// requires the actual client certificate inside the SAME work tracker.
	response, _, err = closedFactsRouteRequest(t, f.client(t, nil), prepared, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
	if err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("material optional TLS/client auth changed: %v %v", response, err)
	}
}

func TestClosedFactsListenerLifecycleStartsBeforeClassificationAndCleansEachRun(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	entered := make(chan struct{}, 2)
	session := &closedFactsClassifySession{closedFactsRouteSession: f.session, classify: func(ctx context.Context) (*store.Store, bool, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, false, ctx.Err()
	}}
	a := newWaitingClosedMemberApp(&memberRuntime{member: closedMember{cfg: Config{Addr: "127.0.0.1:0"}, log: nil}}, session)
	if err := a.prepareClosedFactsListener(f.bundle); err != nil {
		t.Fatal(err)
	}
	original := a.enrollment
	if original.run != nil {
		t.Fatal("constructor started facts")
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- a.runJoinedMaterialLifecycle(ctx, time.Minute) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("classification did not enter")
		}
		response, body, err := closedFactsRouteRequest(t, f.client(t, &f.requesterPair), original, http.MethodPost, enrollment.ClusterQuorumFactsPath, closedFactsRouteBody)
		if err != nil || response.StatusCode != http.StatusOK {
			cancel()
			t.Fatalf("facts unavailable during classification wait: %v %s %v", response, body, err)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("classification cleanup did not return")
		}
		if a.enrollment != original || a.joinedRaft != session || a.store != nil {
			t.Fatal("Run replaced original ownership")
		}
		if err := original.Wait(); err != nil {
			t.Fatal(err)
		}
		original.materialRequests.mu.Lock()
		closed, active := original.materialRequests.closed, original.materialRequests.active
		original.materialRequests.mu.Unlock()
		if !closed || active != 0 {
			t.Fatalf("Run cleanup left tracked work: closed=%v active=%d", closed, active)
		}
	}
}

func TestClosedFactsListenerLifecycleCleansClassificationFailure(t *testing.T) {
	f := newClosedFactsTLSFixture(t)
	failure := errors.New("actual classification fixture failure")
	session := &closedFactsClassifySession{closedFactsRouteSession: f.session, classify: func(context.Context) (*store.Store, bool, error) { return nil, false, failure }}
	a := newWaitingClosedMemberApp(&memberRuntime{member: closedMember{cfg: Config{Addr: "127.0.0.1:0"}, log: nil}}, session)
	if err := a.prepareClosedFactsListener(f.bundle); err != nil {
		t.Fatal(err)
	}
	original := a.enrollment
	if err := a.runJoinedMaterialLifecycle(t.Context(), time.Minute); !errors.Is(err, failure) {
		t.Fatalf("classification cause lost: %v", err)
	}
	if original.run == nil {
		t.Fatal("facts not started before classification")
	}
	if err := original.Wait(); err != nil {
		t.Fatal(err)
	}
	original.materialRequests.mu.Lock()
	closed := original.materialRequests.closed
	original.materialRequests.mu.Unlock()
	if !closed || a.enrollment != original {
		t.Fatal("failure cleanup lost original tracker")
	}
}

func TestClosedFactsListenerMissingCoordinatesKeepsHealthOnly(t *testing.T) {
	for _, missing := range []string{"origin", "listener"} {
		t.Run(missing, func(t *testing.T) {
			f := newClosedFactsTLSFixture(t)
			if missing == "origin" {
				f.bundle.info.PublicURL = ""
			} else {
				f.bundle.info.ListenAddress = ""
			}
			a := newWaitingClosedMemberApp(&memberRuntime{member: closedMember{cfg: Config{Addr: "127.0.0.1:0"}, log: nil}}, f.session)
			if err := a.prepareClosedFactsListener(f.bundle); err != nil {
				t.Fatal(err)
			}
			if a.enrollment != nil || a.store != nil || a.joinedRaft != f.session {
				t.Fatal("missing coordinates manufactured owner")
			}
		})
	}
}
