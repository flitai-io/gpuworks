package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	hubpkg "gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// midRequestFailConn is a tunnel stream that opens fine and takes the whole
// request, then fails the response read with readErr — the shape of a data
// lane dying while the engine is still working on the request.
type midRequestFailConn struct {
	readErr  error
	written  chan struct{}
	once     sync.Once
	closed   chan struct{}
	closeOne sync.Once
}

func newMidRequestFailConn(readErr error) *midRequestFailConn {
	return &midRequestFailConn{readErr: readErr, written: make(chan struct{}), closed: make(chan struct{})}
}

func (c *midRequestFailConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.written) })
	return len(p), nil
}

func (c *midRequestFailConn) Read([]byte) (int, error) {
	select {
	case <-c.written:
		return 0, c.readErr
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *midRequestFailConn) Close() error {
	c.closeOne.Do(func() { close(c.closed) })
	return nil
}
func (c *midRequestFailConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *midRequestFailConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *midRequestFailConn) SetDeadline(time.Time) error      { return nil }
func (c *midRequestFailConn) SetReadDeadline(time.Time) error  { return nil }
func (c *midRequestFailConn) SetWriteDeadline(time.Time) error { return nil }

type midRequestFailHub struct{ readErr error }

func (h midRequestFailHub) OpenTunnel(context.Context, string, string) (net.Conn, error) {
	return newMidRequestFailConn(h.readErr), nil
}
func (midRequestFailHub) Online(string) bool { return true }

// newMidRequestProxy is a one-instance deployment behind a proxy whose tunnels
// fail the way hub describes; one instance leaves the retry transport no other
// replica to switch to, so the status the client gets is the classification.
func newMidRequestProxy(t *testing.T, name string, hub Tunneler) (*Proxy, string) {
	t.Helper()
	st, err := store.Open("file:proxy_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/" + name, Revision: "main", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode(name+"-node", store.HashToken(name), name+"-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: name, Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: name + "-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := st.CreateAPIKey(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), hub, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(p.Close)
	return p, apiKey
}

func proxyChat(t *testing.T, p *Proxy, apiKey, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { p.Handler()(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not answer")
	}
	return rec
}

// T56: the stream opened and carried the request, then the data lane died
// before the response arrived. Once hub reports that as
// ErrDataLaneUnavailable, the condition has to survive net/http's transport
// error wrapping all the way to the proxy's error mapping, and the client gets
// the same retryable 503 with Retry-After as a lane that was gone before the
// stream opened — not 502.
func TestProxyDataLaneLostMidRequestReturns503WithRetryAfter(t *testing.T) {
	lost := fmt.Errorf("%w: node lane-lost-node data lane closed under an open stream: %w",
		hubpkg.ErrDataLaneUnavailable, errors.New("stream reset: connection closed: EOF"))
	p, apiKey := newMidRequestProxy(t, "lanelost", midRequestFailHub{readErr: lost})
	rec := proxyChat(t, p, apiKey, "lanelost")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 for a data lane lost mid-request must carry Retry-After")
	}
}

// The other side of the classification: a stream that fails on a lane that
// is still alive (the agent reset it because the engine went away) is an
// upstream failure and stays 502.
func TestProxyUnclassifiedMidRequestStreamResetStays502(t *testing.T) {
	p, apiKey := newMidRequestProxy(t, "upstreamreset", midRequestFailHub{readErr: errors.New("stream reset")})
	rec := proxyChat(t, p, apiKey, "upstreamreset")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}
}
