package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func newServerDialReconnectNode(t *testing.T) (*Hub, *store.Node, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, token, err := st.CreateNodePending("reconnect-test", agentconf.ServerDial)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Registered after Close, so this runs first. Callers register the peer
	// shutdown even later, and that runs before the store goes away.
	t.Cleanup(h.waitSessionWork)
	return h, node, token
}

func listenForReconnect(t *testing.T, h *Hub, node *store.Node) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	node.ServerDialAddress = ln.Addr().String()
	if err := h.store.SetNodeServerDialAddress(node.UUID, node.ServerDialAddress); err != nil {
		t.Fatal(err)
	}
	return ln
}

func serveReconnectAgent(t *testing.T, ln net.Listener, node *store.Node, token string, gate <-chan struct{}, accepted chan<- struct{}) *atomic.Int32 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var count atomic.Int32
	var sessions sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			sessions.Add(1)
			go func() {
				defer sessions.Done()
				defer conn.Close()
				if accepted != nil {
					select {
					case accepted <- struct{}{}:
					case <-ctx.Done():
						return
					}
				}
				if gate != nil {
					select {
					case <-gate:
					case <-ctx.Done():
						return
					}
				}
				c := agentconn.New(agentconn.Credentials{
					NodeUUID: node.UUID, NodeToken: token, ConnectMode: agentconf.ServerDial,
				}, nil, noopHandlers{}, emptyProber{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
				_ = c.RunAccepted(ctx, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		<-done
		sessions.Wait()
	})
	return &count
}

func TestManualServerDialReconnectBypassesBackoffAndKeepsSession(t *testing.T) {
	h, node, token := newServerDialReconnectNode(t)
	ln := listenForReconnect(t, h, node)
	count := serveReconnectAgent(t, ln, node, token, nil, nil)
	h.reconnect[node.UUID] = reconnectState{failures: 5, next: time.Now().Add(time.Hour)}
	h.reconnectServerDial(context.Background())
	if h.Online(node.UUID) || count.Load() != 0 {
		t.Fatal("automatic reconnect ignored backoff")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.ReconnectServerDial(ctx, node.UUID); err != nil {
		t.Fatal(err)
	}
	cancel()
	session := h.session(node.UUID)
	if session == nil {
		t.Fatal("manual reconnect did not establish a session")
	}
	if _, err := session.ysess.Ping(); err != nil {
		t.Fatalf("request cancellation closed the established connection: %v", err)
	}
	if err := h.ReconnectServerDial(context.Background(), node.UUID); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 || h.session(node.UUID) != session {
		t.Fatal("an online node was connected again")
	}
	updated, err := h.store.NodeByUUID(node.UUID)
	if err != nil || updated.State != store.NodeStateOnline || updated.RecoveryPhase != "" {
		t.Fatalf("successful session not persisted: node=%+v err=%v", updated, err)
	}
	h.reconnectMu.Lock()
	_, waiting := h.reconnect[node.UUID]
	h.reconnectMu.Unlock()
	if waiting {
		t.Fatal("successful reconnect retained backoff")
	}
}

type reconnectWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *reconnectWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestManualServerDialReconnectJoinsAutomaticAttempt(t *testing.T) {
	h, node, token := newServerDialReconnectNode(t)
	ln := listenForReconnect(t, h, node)
	gate, accepted := make(chan struct{}), make(chan struct{}, 1)
	count := serveReconnectAgent(t, ln, node, token, gate, accepted)
	automatic := make(chan struct{})
	go func() { h.reconnectServerDial(context.Background()); close(automatic) }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic reconnect did not start")
	}
	h.ResetServerDialBackoff(node.UUID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := &reconnectWaitContext{Context: ctx, entered: make(chan struct{})}
	manual := make(chan error, 1)
	go func() { manual <- h.ReconnectServerDial(waiting, node.UUID) }()
	select {
	case <-waiting.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("manual reconnect did not join the current attempt")
	}
	close(gate)
	select {
	case err := <-manual:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manual reconnect did not finish")
	}
	<-automatic
	if count.Load() != 1 || !h.Online(node.UUID) {
		t.Fatalf("concurrent callers opened %d connections, online=%v", count.Load(), h.Online(node.UUID))
	}
}

func TestServerDialReconnectCancellationCoversHandshake(t *testing.T) {
	for _, stage := range []string{"authentication", "yamux"} {
		t.Run(stage, func(t *testing.T) {
			h, node, _ := newServerDialReconnectNode(t)
			ln := listenForReconnect(t, h, node)
			ready, peerDone := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(peerDone)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if stage == "yamux" {
					// A real agent's nonce: 32 random bytes in hex. The server
					// refuses to sign any other shape.
					_ = json.NewEncoder(conn).Encode(proto.ServerDialChallenge{NodeUUID: node.UUID, Nonce: strings.Repeat("5a", 32)})
					var proof proto.ServerDialProof
					if err := json.NewDecoder(conn).Decode(&proof); err != nil {
						return
					}
				}
				close(ready)
				_, _ = io.Copy(io.Discard, conn)
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- h.ReconnectServerDial(ctx, node.UUID) }()
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("connection did not reach the stalled stage")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled reconnect returned %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not interrupt the handshake")
			}
			<-peerDone
			if h.Online(node.UUID) {
				t.Fatal("incomplete handshake marked the node online")
			}
		})
	}
}

func TestManualServerDialReconnectRejectsWrongTokenAndBacksOff(t *testing.T) {
	h, node, _ := newServerDialReconnectNode(t)
	ln := listenForReconnect(t, h, node)
	count := serveReconnectAgent(t, ln, node, "different-node-token", nil, nil)
	if err := h.ReconnectServerDial(context.Background(), node.UUID); err == nil {
		t.Fatal("incorrect credentials established a session")
	}
	if h.Online(node.UUID) {
		t.Fatal("failed authentication marked the node online")
	}
	h.reconnectMu.Lock()
	state := h.reconnect[node.UUID]
	h.reconnectMu.Unlock()
	if state.failures != 1 || !state.next.After(time.Now()) {
		t.Fatalf("failed reconnect lost retry policy: %+v", state)
	}
	h.reconnectServerDial(context.Background())
	if count.Load() != 1 {
		t.Fatal("automatic retry bypassed the failed manual attempt's backoff")
	}
}
