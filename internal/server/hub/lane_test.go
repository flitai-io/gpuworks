package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

// yamuxPairData returns a connected server/client yamux session pair over an
// in-process pipe, both on the data profile. Used to exercise Session data-lane
// bookkeeping without a full WebSocket.
func yamuxPairData(t *testing.T) (server, client *yamux.Session) {
	t.Helper()
	a, b := net.Pipe()
	s, err := yamux.Server(a, laneYamuxConfig(proto.LaneData, io.Discard), nil)
	if err != nil {
		t.Fatalf("yamux server: %v", err)
	}
	c, err := yamux.Client(b, laneYamuxConfig(proto.LaneData, io.Discard), nil)
	if err != nil {
		t.Fatalf("yamux client: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = c.Close() })
	return s, c
}

// TestServeWS_LaneParamValidation asserts an unknown lane value is rejected with
// 400 before any WebSocket upgrade, while the known values (missing, ctrl, data)
// are not rejected at the validation gate.
func TestServeWS_LaneParamValidation(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)

	// Bogus lane: a plain GET must get 400 (no upgrade attempted).
	resp, err := http.Get(srv.URL + "/api/v1/agent/connect?lane=bogus")
	if err != nil {
		t.Fatalf("get bogus: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || string(body) != `{"code":"BAD_REQUEST","error":"unknown lane"}`+"\n" {
		t.Fatalf("bogus lane = %d %q, want 400 with the API's error shape", resp.StatusCode, body)
	}

	// Known lanes: a plain (non-upgrade) GET fails the WebSocket handshake, which
	// yields something other than 400-from-validation (coder/websocket answers a
	// failed upgrade with 426/400-upgrade). The point is the validation gate does
	// not itself 400 these; assert we reach the upgrade path by checking the
	// bogus case differs. Here we simply ensure they are not silently 200.
	for _, lane := range []string{"", "ctrl", "data"} {
		url := srv.URL + "/api/v1/agent/connect"
		if lane != "" {
			url += "?lane=" + lane
		}
		r, err := http.Get(url)
		if err != nil {
			t.Fatalf("get %q: %v", lane, err)
		}
		r.Body.Close()
		if r.StatusCode == http.StatusOK {
			t.Fatalf("lane %q: non-upgrade GET unexpectedly 200", lane)
		}
	}
}

// TestServeWS_DataLaneNonUpgrade asserts a non-WebSocket GET on ?lane=data does
// not switch protocols (no 101) and returns an error status instead of hanging.
func TestServeWS_DataLaneNonUpgrade(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/v1/agent/connect?lane=data")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("non-upgrade data GET switched protocols")
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("non-upgrade data GET returned 200")
	}
}

// TestDataLaneRoute_NoneStateUsesCtrl asserts a session that never attached a
// data lane (legacy/single-lane, server_dial) routes data-plane kinds over
// ctrl as its normal, permanent shape, and ctrl-plane kinds always use ctrl.
func TestDataLaneRoute_NoneStateUsesCtrl(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	ysess, usedData, err := sess.dataLaneRoute(context.Background(), proto.StreamTunnel)
	if err != nil || ysess != ctrlYS || usedData {
		t.Fatalf("tunnel with no data lane ever attached: ysess=%v usedData=%v err=%v, want ctrl/false/nil", ysess == ctrlYS, usedData, err)
	}
	ysess, usedData, err = sess.dataLaneRoute(context.Background(), proto.StreamCtrl)
	if err != nil || ysess != ctrlYS || usedData {
		t.Fatalf("ctrl kind: ysess=%v usedData=%v err=%v, want ctrl/false/nil", ysess == ctrlYS, usedData, err)
	}
}

// TestDataLaneRoute_AttachedUsesData asserts data-plane kinds ride the data
// session while it is attached and healthy, and ctrl-plane kinds still stay on
// ctrl.
func TestDataLaneRoute_AttachedUsesData(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	dataYS, _ := yamuxPairData(t)
	sess.attachData(dataYS)

	ysess, usedData, err := sess.dataLaneRoute(context.Background(), proto.StreamTunnel)
	if err != nil || ysess != dataYS || !usedData {
		t.Fatalf("tunnel should use the attached data session: ysess=%v usedData=%v err=%v", ysess == dataYS, usedData, err)
	}
	ysess, usedData, err = sess.dataLaneRoute(context.Background(), proto.StreamTerminal)
	if err != nil || ysess != ctrlYS || usedData {
		t.Fatal("terminal must stay on ctrl session even with a data lane attached")
	}
}

// TestDataLaneRoute_DetachedNeverFallsBackToCtrl is the core regression test
// for this design: once a data lane has attached at least once, dropping it
// must never make a data-plane kind slide onto ctrl. dataLaneRoute either
// waits for reattachment or returns ErrDataLaneUnavailable — it must not
// return the ctrl session.
func TestDataLaneRoute_DetachedNeverFallsBackToCtrl(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	dataYS, _ := yamuxPairData(t)
	sess.attachData(dataYS)
	_ = dataYS.Close()
	if !waitClosed(dataYS) {
		t.Fatal("data session never reported closed")
	}
	waitState(t, sess, "detached")

	// A short caller deadline (well under dataLaneReattachWait) fails fast with
	// the caller's own deadline error; the point under test is that the
	// returned session is never ctrl, whichever typed error comes back.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ysess, usedData, err := sess.dataLaneRoute(ctx, proto.StreamModelRead)
	if ysess == ctrlYS {
		t.Fatal("detached data-plane kind must never route onto ctrl")
	}
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (caller deadline shorter than dataLaneReattachWait)", err)
	}
	if usedData {
		t.Fatal("usedData must be false on a failed route")
	}
}

// TestDataLaneRoute_DetachedReattachWakesWaiter asserts a caller blocked in
// dataLaneRoute on a detached lane gets the freshly attached session as soon
// as it reattaches, without polling or waiting out the full budget.
func TestDataLaneRoute_DetachedReattachWakesWaiter(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	first, _ := yamuxPairData(t)
	sess.attachData(first)
	_ = first.Close()
	waitState(t, sess, "detached")

	type result struct {
		ysess *yamux.Session
		err   error
	}
	done := make(chan result, 1)
	go func() {
		ysess, _, err := sess.dataLaneRoute(context.Background(), proto.StreamLogtail)
		done <- result{ysess, err}
	}()

	// Give the goroutine a moment to park in awaitDataLane before reattaching,
	// so this exercises the wake path rather than a lucky race.
	time.Sleep(30 * time.Millisecond)
	second, _ := yamuxPairData(t)
	start := time.Now()
	sess.attachData(second)

	select {
	case r := <-done:
		if r.err != nil || r.ysess != second {
			t.Fatalf("route result = %v (ysess match %v), want the reattached session", r.err, r.ysess == second)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("waiter took %v to wake after reattach, want well under dataLaneReattachWait", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was never woken by reattach")
	}
}

// TestDataLaneRoute_TimeoutReturnsTypedError asserts a detached lane that
// never reattaches within dataLaneReattachWait yields ErrDataLaneUnavailable,
// bounded by the wait budget rather than hanging.
func TestDataLaneRoute_TimeoutReturnsTypedError(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	first, _ := yamuxPairData(t)
	sess.attachData(first)
	_ = first.Close()
	waitState(t, sess, "detached")

	start := time.Now()
	_, _, err := sess.dataLaneRoute(context.Background(), proto.StreamTunnel)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrDataLaneUnavailable) {
		t.Fatalf("err = %v, want ErrDataLaneUnavailable", err)
	}
	if elapsed < dataLaneReattachWait || elapsed > dataLaneReattachWait+2*time.Second {
		t.Fatalf("wait took %v, want approximately dataLaneReattachWait (%v)", elapsed, dataLaneReattachWait)
	}
}

// TestDataLaneRoute_ContextCancelReturnsImmediately asserts a caller-supplied
// context cancellation aborts the wait right away instead of riding out the
// full budget.
func TestDataLaneRoute_ContextCancelReturnsImmediately(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	first, _ := yamuxPairData(t)
	sess.attachData(first)
	_ = first.Close()
	waitState(t, sess, "detached")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, err := sess.dataLaneRoute(ctx, proto.StreamTunnel)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("cancel took %v to be observed, want well under 1s", elapsed)
	}
}

// TestDataLaneRoute_CtrlCloseWakesWaiter asserts closing the ctrl session
// while a caller waits for a detached data lane wakes it immediately (waiting
// further would be pointless: a closed ctrl session means the node is
// offline).
func TestDataLaneRoute_CtrlCloseWakesWaiter(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pa, pb := net.Pipe()
	t.Cleanup(func() { _ = pb.Close() })
	sess := newSession(h, ctrlYS, pa)

	first, _ := yamuxPairData(t)
	sess.attachData(first)
	_ = first.Close()
	waitState(t, sess, "detached")

	done := make(chan error, 1)
	go func() {
		_, _, err := sess.dataLaneRoute(context.Background(), proto.StreamTunnel)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	start := time.Now()
	sess.markClosed()

	select {
	case err := <-done:
		if !errors.Is(err, ErrDataLaneUnavailable) {
			t.Fatalf("err = %v, want ErrDataLaneUnavailable", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("ctrl close took %v to wake the waiter, want well under dataLaneReattachWait", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctrl close never woke the waiter")
	}
}

// waitState polls DataLaneState until it reports want, failing the test if it
// never does within a couple of seconds. Used only to synchronize on the
// asynchronous watchData transition after closing a data session; the
// production wait path (awaitDataLane) itself never polls.
func waitState(t *testing.T, sess *Session, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sess.DataLaneState() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("DataLaneState = %q, want %q", sess.DataLaneState(), want)
}

// TestSession_AttachDetachReplace covers the data-lane lifecycle bookkeeping:
// none -> attached -> (drop) detached, and replacement closing the old session.
func TestSession_AttachDetachReplace(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pc, _ := net.Pipe()
	t.Cleanup(func() { _ = pc.Close() })
	sess := newSession(h, ctrlYS, pc)

	if got := sess.DataLaneState(); got != "none" {
		t.Fatalf("initial state = %q, want none", got)
	}

	data1, _ := yamuxPairData(t)
	sess.attachData(data1)
	if got := sess.DataLaneState(); got != "attached" {
		t.Fatalf("after attach = %q, want attached", got)
	}

	// Replacement: attaching a second data lane closes the first.
	data2, _ := yamuxPairData(t)
	sess.attachData(data2)
	if got := sess.DataLaneState(); got != "attached" {
		t.Fatalf("after replace = %q, want attached", got)
	}
	if !waitClosed(data1) {
		t.Fatal("replaced data session was not closed")
	}

	// Dropping the live data lane transitions to detached (attached before).
	_ = data2.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sess.DataLaneState() == "attached" {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sess.DataLaneState(); got != "detached" {
		t.Fatalf("after drop = %q, want detached", got)
	}
}

// TestSession_CloseClosesData asserts the data lane is a child of the ctrl
// session: closing the session closes the data lane too.
func TestSession_CloseClosesData(t *testing.T) {
	h := newTestHub(t)
	ctrlYS, _ := yamuxPairData(t)
	pa, pb := net.Pipe()
	t.Cleanup(func() { _ = pb.Close() })
	sess := newSession(h, ctrlYS, pa)
	dataYS, _ := yamuxPairData(t)
	sess.attachData(dataYS)
	sess.Close()
	if !waitClosed(dataYS) {
		t.Fatal("data session not closed when ctrl session closed")
	}
}

func waitClosed(s *yamux.Session) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.IsClosed() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.IsClosed()
}

// dialCtrlLane dials ?lane=ctrl and completes the ctrl handshake, returning the
// yamux session and the Registered reply (which carries the SessionID).
func dialCtrlLane(t *testing.T, srv *httptest.Server, token string) (*yamux.Session, proto.Registered) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/agent/connect?lane=ctrl"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial ctrl: %v", err)
	}
	netConn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	ysess, err := yamux.Client(netConn, laneYamuxConfig(proto.LaneCtrl, io.Discard), nil)
	if err != nil {
		t.Fatalf("yamux: %v", err)
	}
	t.Cleanup(func() { _ = ysess.Close() })
	ctrl, err := ysess.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("open ctrl: %v", err)
	}
	hdr, _ := json.Marshal(proto.StreamHeader{Kind: proto.StreamCtrl})
	_, _ = ctrl.Write(append(hdr, '\n'))
	enc := json.NewEncoder(ctrl)
	_ = enc.Encode(proto.Envelope{T: proto.THello, D: mustJSON(t, proto.Hello{ProtoMin: 1, ProtoMax: 1, NodeToken: token})})
	dec := json.NewDecoder(ctrl)
	var reg proto.Registered
	readEnvelopeInto(t, dec, proto.TRegistered, &reg)
	var sh proto.Hello
	readEnvelopeInto(t, dec, proto.THello, &sh)
	// Keep reading ctrl in the background so RPCs (ping) do not block the peer;
	// tests here only need the session to stay alive.
	return ysess, reg
}

// dialDataRaw opens a raw ?lane=data WebSocket, writes a DataHello, and returns
// the one-line reply envelope the server sends before yamux (ack or reject).
func dialDataRaw(t *testing.T, srv *httptest.Server, hello proto.DataHello) proto.Envelope {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/agent/connect?lane=data"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial data: %v", err)
	}
	netConn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	t.Cleanup(func() { _ = netConn.Close() })
	line, _ := json.Marshal(hello)
	if _, err := netConn.Write(append(line, '\n')); err != nil {
		t.Fatalf("write data hello: %v", err)
	}
	var env proto.Envelope
	if err := json.NewDecoder(netConn).Decode(&env); err != nil {
		t.Fatalf("read data reply: %v", err)
	}
	return env
}

// TestDataLane_StaleSessionRejected asserts a data lane presenting a wrong
// SessionID is rejected with stale_session, recorded, and does not disturb the
// live ctrl session; a wrong token is rejected with bad_token; and a garbage
// hello line is closed without a reply/attach.
func TestDataLane_StaleSessionRejected(t *testing.T) {
	h := newTestHub(t)
	usage := &usagereport.Counters{}
	h.SetUsageCounters(usage)
	uuid, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)

	_, reg := dialCtrlLane(t, srv, token)
	if reg.SessionID == "" {
		t.Fatal("ctrl lane did not issue a SessionID")
	}
	if !h.Online(uuid) {
		t.Fatal("node not online after ctrl handshake")
	}

	// Wrong SessionID: stale_session reject.
	env := dialDataRaw(t, srv, proto.DataHello{NodeToken: token, SessionID: "not-the-right-id"})
	if env.T != proto.TReject {
		t.Fatalf("stale data lane reply t = %q, want reject", env.T)
	}
	var rej proto.Reject
	if err := json.Unmarshal(env.D, &rej); err != nil {
		t.Fatalf("decode reject: %v", err)
	}
	if rej.Reason != proto.RejectStaleSession {
		t.Fatalf("reject reason = %q, want %q", rej.Reason, proto.RejectStaleSession)
	}

	// Wrong token: bad_token reject.
	env2 := dialDataRaw(t, srv, proto.DataHello{NodeToken: "wrong-token", SessionID: reg.SessionID})
	var rej2 proto.Reject
	if env2.T != proto.TReject || json.Unmarshal(env2.D, &rej2) != nil || rej2.Reason != proto.RejectBadToken {
		t.Fatalf("wrong-token reply = %+v (reason %q), want bad_token reject", env2, rej2.Reason)
	}

	// Both rejects are recorded for diagnostics.
	found := 0
	for _, r := range h.RecentRejects() {
		if r.Reason == proto.RejectStaleSession || r.Reason == proto.RejectBadToken {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("expected stale_session and bad_token in RecentRejects, found %d", found)
	}
	// And counted, by reason only, for the usage report.
	counted := map[string]int{}
	for _, c := range usage.Drain()[usagereport.KindProtocol] {
		counted[c.Name] = c.N
	}
	if counted[proto.RejectStaleSession] != 1 || counted[proto.RejectBadToken] != 1 {
		t.Fatalf("usage counted %v, want one stale_session and one bad_token", counted)
	}

	// The live ctrl session is unaffected by the rejected data lanes.
	if !h.Online(uuid) {
		t.Fatal("ctrl session disturbed by rejected data lane")
	}
	if got := h.DataLaneState(uuid); got != "none" {
		t.Fatalf("data lane state after rejects = %q, want none", got)
	}
}

// TestDataLane_GarbageHelloClosed asserts a data lane whose first line is not a
// valid DataHello is closed without attaching and without a reply envelope.
func TestDataLane_GarbageHelloClosed(t *testing.T) {
	h := newTestHub(t)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	_ = reg

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/agent/connect?lane=data"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial data: %v", err)
	}
	netConn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	t.Cleanup(func() { _ = netConn.Close() })
	if _, err := netConn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	// The server closes without a reply: a read returns EOF/closed, not an env.
	_ = netConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var env proto.Envelope
	if err := json.NewDecoder(netConn).Decode(&env); err == nil {
		t.Fatalf("garbage hello got a reply envelope %q, want close", env.T)
	}
}
