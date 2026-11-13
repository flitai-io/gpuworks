package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Delay one frame in the real Agent protocol. Delaying its challenge holds
// authentication; delaying its proof read holds the following session setup.
type delayedServerDialFrame struct {
	net.Conn
	ctx      context.Context
	stage    string
	delay    time.Duration
	once     sync.Once
	delayErr error
}

func (c *delayedServerDialFrame) wait() error {
	c.once.Do(func() {
		timer := time.NewTimer(c.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.ctx.Done():
			c.delayErr = c.ctx.Err()
		}
	})
	return c.delayErr
}

func (c *delayedServerDialFrame) Write(p []byte) (int, error) {
	if c.stage == "authentication" {
		if err := c.wait(); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

func (c *delayedServerDialFrame) Read(p []byte) (int, error) {
	if c.stage == "session" {
		if err := c.wait(); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

func serveDelayedReconnectAgent(t *testing.T, ln net.Listener, node *store.Node, token, stage string, delay time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		c := agentconn.New(agentconn.Credentials{
			NodeUUID: node.UUID, NodeToken: token, ConnectMode: agentconf.ServerDial,
		}, nil, noopHandlers{}, emptyProber{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		_ = c.RunAccepted(ctx, &delayedServerDialFrame{Conn: conn, ctx: ctx, stage: stage, delay: delay})
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		<-done
	})
}

func TestServerDialReconnectKeepsHandshakeBeyondTCPDeadline(t *testing.T) {
	for _, stage := range []string{"authentication", "session"} {
		for _, mode := range []string{"automatic", "manual"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				t.Parallel()
				h, node, token := newServerDialReconnectNode(t)
				ln := listenForReconnect(t, h, node)
				// Six seconds exceeds the reconnect TCP limit (5s), but is
				// within the existing authentication allowance (15s).
				serveDelayedReconnectAgent(t, ln, node, token, stage, 6*time.Second)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "manual" {
					if err := h.ReconnectServerDial(ctx, node.UUID); err != nil {
						t.Fatal(err)
					}
				} else {
					h.reconnectServerDial(ctx)
				}
				cancel()
				session := h.session(node.UUID)
				if session == nil {
					t.Fatal("TCP connect deadline interrupted the subsequent handshake")
				}
				if _, err := session.ysess.Ping(); err != nil {
					t.Fatalf("request cancellation closed the established session: %v", err)
				}
				h.reconnectMu.Lock()
				_, waiting := h.reconnect[node.UUID]
				h.reconnectMu.Unlock()
				if waiting {
					t.Fatal("successful delayed handshake retained backoff")
				}
			})
		}
	}
}

func TestServerDialReconnectHonorsCallerDeadlineDuringHandshake(t *testing.T) {
	for _, stage := range []string{"authentication", "session"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			h, node, token := newServerDialReconnectNode(t)
			ln := listenForReconnect(t, h, node)
			serveDelayedReconnectAgent(t, ln, node, token, stage, 6*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := h.ReconnectServerDial(ctx, node.UUID); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("caller deadline returned %v", err)
			}
			if h.Online(node.UUID) {
				t.Fatal("cancelled incomplete handshake marked the node online")
			}
		})
	}
}

func TestServerDialReconnectRetainsAuthenticationTimeout(t *testing.T) {
	h, node, token := newServerDialReconnectNode(t)
	ln := listenForReconnect(t, h, node)
	// The peer answers after the existing 15s authentication deadline.
	serveDelayedReconnectAgent(t, ln, node, token, "authentication", 16*time.Second)
	err := h.ReconnectServerDial(context.Background(), node.UUID)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("authentication past its deadline returned %v", err)
	}
	if h.Online(node.UUID) {
		t.Fatal("timed-out authentication marked the node online")
	}
}

func TestServerDialReconnectResetSurvivesFailedAttempt(t *testing.T) {
	h, node, _ := newServerDialReconnectNode(t)
	ln := listenForReconnect(t, h, node)
	gate, accepted := make(chan struct{}), make(chan struct{}, 1)
	serveReconnectAgent(t, ln, node, "invalid-token", gate, accepted)
	h.reconnect[node.UUID] = reconnectState{failures: 5}
	done := make(chan struct{})
	go func() { h.reconnectServerDial(context.Background()); close(done) }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic reconnect did not start")
	}
	h.ResetServerDialBackoff(node.UUID)
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("failed reconnect did not finish")
	}
	h.reconnectMu.Lock()
	_, waiting := h.reconnect[node.UUID]
	_, active := h.reconnectAttempts[node.UUID]
	h.reconnectMu.Unlock()
	if waiting || active || h.Online(node.UUID) {
		t.Fatalf("old failure undid reset: backoff=%v active=%v online=%v", waiting, active, h.Online(node.UUID))
	}
}
