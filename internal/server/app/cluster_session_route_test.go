//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

// recordingSessionHub is a member-session router that records what the route
// configured and can fail to close.
type recordingSessionHub struct {
	mu       sync.Mutex
	identity uint64
	key      string
	routes   map[uint64]string
	served   chan net.Listener
	closed   int
	closeErr error
	serveErr error
}

func newRecordingSessionHub() *recordingSessionHub {
	return &recordingSessionHub{routes: map[uint64]string{}, served: make(chan net.Listener, 1)}
}

func (h *recordingSessionHub) SetMemberIdentity(memberID uint64, key []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.identity, h.key = memberID, string(key)
}

func (h *recordingSessionHub) SetMemberSecureTransport(uint64, *tls.Config, func(context.Context, uint64, string) (*tls.Config, error), func(context.Context, *x509.Certificate) error) error {
	return errors.New("secure transport is not part of this fixture")
}

func (h *recordingSessionHub) SetMemberRoute(memberID uint64, addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[memberID] = addr
}

func (h *recordingSessionHub) ServeMemberSessions(ln net.Listener) error {
	h.served <- ln
	return h.serveErr
}

func (h *recordingSessionHub) CloseMemberSessions(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed++
	return h.closeErr
}

func sessionRouteApp(t *testing.T, journal *bytes.Buffer) *App {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "route.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &App{store: st, log: slog.New(slog.NewTextHandler(journal, nil))}
}

func TestMemberSessionRouteIsServedThroughTheHub(t *testing.T) {
	var journal bytes.Buffer
	a := sessionRouteApp(t, &journal)
	a.store.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("local-key"), Listen: "127.0.0.1:0",
		Peers: []store.ClusterSessionPeer{{ID: 7, Address: "127.0.0.1:1"}, {ID: 8, Address: "127.0.0.1:9012"}, {ID: 0, Address: "127.0.0.1:2"}, {ID: 9, Address: " "}}})
	hub := newRecordingSessionHub()
	stop, err := a.routeClusterMemberSessions(t.Context(), hub)
	if err != nil || stop == nil {
		t.Fatalf("route: stop=%v err=%v", stop != nil, err)
	}
	ln := <-hub.served
	hub.mu.Lock()
	identity, key, own, peer, routes := hub.identity, hub.key, hub.routes[7], hub.routes[8], len(hub.routes)
	hub.mu.Unlock()
	if identity != 7 || key != "local-key" {
		t.Fatalf("member identity = %d %q", identity, key)
	}
	// The member's own route is the bound address; a peer entry for itself,
	// for no member, or without an address is not a route.
	if own != ln.Addr().String() || peer != "127.0.0.1:9012" || routes != 2 {
		t.Fatalf("routes: own=%q listener=%q peer=%q count=%d", own, ln.Addr(), peer, routes)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped route still listens: %v", err)
	}
	if hub.closed != 1 {
		t.Fatalf("member sessions closed %d times", hub.closed)
	}
}

func TestMemberSessionRouteFailuresAndFallbacks(t *testing.T) {
	t.Run("no cluster route starts nothing", func(t *testing.T) {
		var journal bytes.Buffer
		a := sessionRouteApp(t, &journal)
		hub := newRecordingSessionHub()
		stop, err := a.routeClusterMemberSessions(t.Context(), hub)
		if stop != nil || err != nil || len(hub.routes) != 0 || journal.Len() != 0 {
			t.Fatalf("single node routed sessions: stop=%v err=%v routes=%d log=%s", stop != nil, err, len(hub.routes), journal.String())
		}
	})
	t.Run("hub without a router keeps sessions local and says so", func(t *testing.T) {
		var journal bytes.Buffer
		a := sessionRouteApp(t, &journal)
		a.store.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("k"), Listen: "127.0.0.1:0"})
		stop, err := a.routeClusterMemberSessions(t.Context(), nil)
		if stop != nil || err != nil {
			t.Fatalf("unrouted member: stop=%v err=%v", stop != nil, err)
		}
		if !strings.Contains(journal.String(), "level=WARN") || !strings.Contains(journal.String(), "member sessions are not routed between members") {
			t.Fatalf("unrouted member was not journaled: %s", journal.String())
		}
	})
	t.Run("canceled start", func(t *testing.T) {
		var journal bytes.Buffer
		a := sessionRouteApp(t, &journal)
		a.store.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("k"), Listen: "127.0.0.1:0"})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		hub := newRecordingSessionHub()
		if stop, err := a.routeClusterMemberSessions(ctx, hub); stop != nil || !errors.Is(err, context.Canceled) || len(hub.routes) != 0 {
			t.Fatalf("canceled route: stop=%v err=%v", stop != nil, err)
		}
	})
	t.Run("address in use", func(t *testing.T) {
		var journal bytes.Buffer
		a := sessionRouteApp(t, &journal)
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer taken.Close()
		a.store.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("k"), Listen: taken.Addr().String()})
		hub := newRecordingSessionHub()
		if stop, err := a.routeClusterMemberSessions(t.Context(), hub); stop != nil || err == nil || len(hub.routes) != 0 {
			t.Fatalf("route on a taken address: stop=%v err=%v routes=%d", stop != nil, err, len(hub.routes))
		}
	})
	t.Run("secure transport refused releases the listener", func(t *testing.T) {
		var journal bytes.Buffer
		a := sessionRouteApp(t, &journal)
		a.cfg.DataDir = t.TempDir()
		addr := testport.Address(t)
		a.store.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Listen: addr, Secure: true})
		hub := newRecordingSessionHub()
		if stop, err := a.routeClusterMemberSessions(t.Context(), hub); stop != nil || err == nil {
			t.Fatalf("secure route without enrollment identity: stop=%v err=%v", stop != nil, err)
		}
		again, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("failed route kept its listener: %v", err)
		}
		_ = again.Close()
	})
	t.Run("close failure is reported", func(t *testing.T) {
		var journal bytes.Buffer
		a := sessionRouteApp(t, &journal)
		a.store.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("k"), Listen: "127.0.0.1:0"})
		hub := newRecordingSessionHub()
		hub.closeErr = errors.New("sessions still draining")
		stop, err := a.routeClusterMemberSessions(t.Context(), hub)
		if err != nil {
			t.Fatal(err)
		}
		<-hub.served
		if err := stop(); !errors.Is(err, hub.closeErr) {
			t.Fatalf("stop = %v", err)
		}
	})
}
