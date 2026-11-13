package conn

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// laneTestServer is a faithful stand-in for the hub's /api/v1/agent/connect
// handler: it speaks the real ctrl handshake over WebSocket+yamux (issuing an
// optional SessionID) and the real data-lane hello/ack/reject line protocol, and
// records what the agent sent so tests can assert the agent's lane decisions.
type laneTestServer struct {
	sessionID       string   // returned in Registered; "" models an old server
	dataReject      string   // non-empty: reject every data hello with this reason
	dropAfterAttach bool     // close the data session right after acking it
	caps            []string // advertised in the server hello

	mu sync.Mutex
	// ctrlKinds and telemetry record, in arrival order, the envelope types
	// the agent sent on ctrl and on agent-opened telemetry streams.
	ctrlKinds []string
	telemetry []string
	dataSess  *yamux.Session // the current data lane, for tests to drop
	holdData  bool           // reject data hellos (stale_session) while set
	// stallTelemetry, while open, keeps telemetry streams from being read,
	// as a server waiting for ingest room does.
	stallTelemetry chan struct{}
	telemetryLines []string                 // log lines received on telemetry streams, in order
	telTail        map[string]chan struct{} // per batch type: the last accepted stream's done channel
	ctrlLanes      []string                 // lane query seen on each ctrl dial
	dataHellos     []proto.DataHello
	helloTimes     []time.Time // wall-clock arrival of each data hello, index-aligned with dataHellos
}

func (s *laneTestServer) handle(w http.ResponseWriter, r *http.Request) {
	lane := r.URL.Query().Get("lane")
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(-1)
	nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	defer nc.Close()
	if lane == proto.LaneData {
		s.serveData(nc)
		return
	}
	s.mu.Lock()
	s.ctrlLanes = append(s.ctrlLanes, lane)
	s.mu.Unlock()
	s.serveCtrl(nc, lane)
}

func (s *laneTestServer) serveCtrl(nc net.Conn, lane string) {
	profile := proto.LaneData
	if lane == proto.LaneCtrl {
		profile = proto.LaneCtrl
	}
	ys, err := yamux.Server(nc, laneYamuxConfig(profile, io.Discard), nil)
	if err != nil {
		return
	}
	defer ys.Close()
	stream, err := ys.AcceptStream()
	if err != nil {
		return
	}
	if _, err := readLine(stream); err != nil { // stream header
		return
	}
	dec := json.NewDecoder(stream)
	var hello proto.Envelope
	if err := dec.Decode(&hello); err != nil || hello.T != proto.THello {
		return
	}
	write := func(t string, d any) {
		b, _ := json.Marshal(d)
		line, _ := json.Marshal(proto.Envelope{T: t, D: b})
		_, _ = stream.Write(append(line, '\n'))
	}
	sid := ""
	if lane == proto.LaneCtrl {
		sid = s.sessionID
	}
	write(proto.TRegistered, proto.Registered{NodeUUID: "node-1", NodeToken: "secret", Proto: 1, SessionID: sid})
	write(proto.THello, proto.Hello{ProtoMin: 1, ProtoMax: 1, Caps: s.caps})
	// Record status/heartbeats until the agent hangs up.
	for {
		var env proto.Envelope
		if err := dec.Decode(&env); err != nil {
			return
		}
		s.mu.Lock()
		s.ctrlKinds = append(s.ctrlKinds, env.T)
		s.mu.Unlock()
	}
}

func (s *laneTestServer) serveData(nc net.Conn) {
	line, err := readLine(nc)
	if err != nil {
		return
	}
	var hello proto.DataHello
	if err := json.Unmarshal(line, &hello); err != nil {
		return
	}
	s.mu.Lock()
	s.dataHellos = append(s.dataHellos, hello)
	s.helloTimes = append(s.helloTimes, time.Now())
	drop := s.dropAfterAttach
	reject := s.dataReject
	if s.holdData {
		reject = proto.RejectStaleSession
	}
	s.mu.Unlock()
	if reject != "" {
		d, _ := json.Marshal(proto.Reject{Reason: reject})
		env, _ := json.Marshal(proto.Envelope{T: proto.TReject, D: d})
		_, _ = nc.Write(append(env, '\n'))
		return
	}
	ack, _ := json.Marshal(proto.Envelope{T: proto.TDataAck})
	if _, err := nc.Write(append(ack, '\n')); err != nil {
		return
	}
	ys, err := yamux.Server(nc, laneYamuxConfig(proto.LaneData, io.Discard), nil)
	if err != nil {
		return
	}
	if drop {
		// Simulate the lane dying right after a successful attach: the
		// agent's very next reconnect attempt must be immediate (no
		// backoff), unlike a dial failure or reject.
		_ = ys.Close()
		return
	}
	s.mu.Lock()
	s.dataSess = ys
	s.mu.Unlock()
	for {
		stream, err := ys.AcceptStream()
		if err != nil {
			return
		}
		// Like the hub: the header is read in the accept loop, and streams of
		// one batch type are read in the order they were accepted.
		line, err := readLine(stream)
		var hdr proto.StreamHeader
		if err != nil || json.Unmarshal(line, &hdr) != nil || hdr.Kind != proto.StreamTelemetry ||
			(hdr.Ref != proto.TMetrics && hdr.Ref != proto.TLogs) {
			_ = stream.Reset()
			continue
		}
		s.mu.Lock()
		if s.telTail == nil {
			s.telTail = map[string]chan struct{}{}
		}
		prev, done := s.telTail[hdr.Ref], make(chan struct{})
		s.telTail[hdr.Ref] = done
		s.mu.Unlock()
		go func() {
			defer close(done)
			if prev != nil {
				<-prev
			}
			s.serveAgentStream(stream)
		}()
	}
}

// serveAgentStream records the envelopes of an agent-opened telemetry stream
// whose header the accept loop has read: only newline-terminated lines count,
// as at the hub.
func (s *laneTestServer) serveAgentStream(stream *yamux.Stream) {
	defer stream.Close()
	s.mu.Lock()
	stall := s.stallTelemetry
	s.mu.Unlock()
	if stall != nil {
		<-stall
	}
	br := bufio.NewReader(stream)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return // an unterminated line is not a batch
		}
		var env proto.Envelope
		if json.Unmarshal(line, &env) != nil {
			return
		}
		s.mu.Lock()
		s.telemetry = append(s.telemetry, env.T)
		var batch proto.LogBatch
		if env.T == proto.TLogs && json.Unmarshal(env.D, &batch) == nil {
			for _, l := range batch.Lines {
				s.telemetryLines = append(s.telemetryLines, l.Line)
			}
		}
		s.mu.Unlock()
	}
}

func (s *laneTestServer) snapshot() ([]string, []proto.DataHello) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ctrlLanes...), append([]proto.DataHello(nil), s.dataHellos...)
}

func (s *laneTestServer) helloTimestamps() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.helloTimes...)
}

// runAgainst starts the agent Run loop against the test server and returns a
// stop func that cancels it and waits for Run to return.
func runAgainst(t *testing.T, s *laneTestServer, dataLane bool) (*Conn, func() error) {
	t.Helper()
	return runAgainstWith(t, s, dataLane, nil)
}

// runAgainstWith is runAgainst with setup applied to the Conn before it runs.
func runAgainstWith(t *testing.T, s *laneTestServer, dataLane bool, setup func(*Conn)) (*Conn, func() error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	c := New(Credentials{ServerURL: srv.URL, NodeUUID: "node-1", NodeToken: "secret", DataLane: dataLane}, nil, nil, capabilityProber{}, slog.Default())
	if setup != nil {
		setup(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	stop := func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after cancel")
			return nil
		}
	}
	return c, stop
}

func waitHellos(s *laneTestServer, n int, d time.Duration) []proto.DataHello {
	deadline := time.Now().Add(d)
	for {
		_, hellos := s.snapshot()
		if len(hellos) >= n || time.Now().After(deadline) {
			return hellos
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDataLane_OpensWithSessionIDAndFlag asserts the agent dials the ctrl lane
// with ?lane=ctrl, opens the data lane with the issued SessionID and token, and
// closes the data lane when Run returns.
func TestDataLane_OpensWithSessionIDAndFlag(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1"}
	c, stop := runAgainst(t, s, true)
	hellos := waitHellos(s, 1, 5*time.Second)
	if len(hellos) != 1 || hellos[0] != (proto.DataHello{NodeToken: "secret", SessionID: "sess-1"}) {
		t.Fatalf("data hellos = %+v, want one with token secret and session sess-1", hellos)
	}
	lanes, _ := s.snapshot()
	if len(lanes) != 1 || lanes[0] != proto.LaneCtrl {
		t.Fatalf("ctrl dial lanes = %v, want [ctrl]", lanes)
	}
	// The data session is attached on the agent side while Run is live.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.dataMu.Lock()
		live := c.dataSess != nil
		c.dataMu.Unlock()
		if live {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stop(); err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	if c.dataSess != nil {
		t.Fatal("data lane outlived Run")
	}
}

// TestDataLane_DisabledFlagStaysSingleLane asserts --data-lane=false dials with
// no lane parameter (exactly a legacy agent) and never opens a data lane.
func TestDataLane_DisabledFlagStaysSingleLane(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1"}
	_, stop := runAgainst(t, s, false)
	if hellos := waitHellos(s, 1, 1500*time.Millisecond); len(hellos) != 0 {
		t.Fatalf("data lane opened with flag disabled: %+v", hellos)
	}
	lanes, _ := s.snapshot()
	if len(lanes) != 1 || lanes[0] != "" {
		t.Fatalf("ctrl dial lanes = %q, want one dial without lane param", lanes)
	}
	_ = stop()
}

// TestDataLane_OldServerWithoutSessionID asserts a server that issues no
// SessionID (pre-dual-lane) gets no data-lane attempt at all: the agent runs
// single-lane without probing for a route that does not exist.
func TestDataLane_OldServerWithoutSessionID(t *testing.T) {
	s := &laneTestServer{sessionID: ""}
	_, stop := runAgainst(t, s, true)
	if hellos := waitHellos(s, 1, 1500*time.Millisecond); len(hellos) != 0 {
		t.Fatalf("data lane attempted against old server: %+v", hellos)
	}
	_ = stop()
}

// TestDataLane_RejectRetriesWithBackoffAndCtrlSurvives asserts a stale_session
// reject does not disturb the ctrl connection and the agent retries the data
// lane with backoff (first retry within ~1 s, second within ~3 s).
func TestDataLane_RejectRetriesWithBackoffAndCtrlSurvives(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", dataReject: proto.RejectStaleSession}
	c, stop := runAgainst(t, s, true)
	start := time.Now()
	hellos := waitHellos(s, 2, 5*time.Second)
	if len(hellos) < 2 {
		t.Fatalf("expected >= 2 data hellos after rejects, got %d in %v", len(hellos), time.Since(start))
	}
	for _, h := range hellos {
		if h.SessionID != "sess-1" {
			t.Fatalf("retry sent session %q, want sess-1", h.SessionID)
		}
	}
	// ctrl is untouched: Run is still live and the agent holds no data session.
	c.dataMu.Lock()
	live := c.dataSess
	c.dataMu.Unlock()
	if live != nil {
		t.Fatal("rejected data lane left a live data session")
	}
	if err := stop(); err != nil && err != context.Canceled {
		t.Fatalf("Run ended with %v; the ctrl lane must survive data rejects", err)
	}
}

// TestDataLane_ImmediateReconnectAfterAttachThenDrop asserts that once the
// data lane has successfully attached at least once, dropping it triggers an
// immediate reconnect attempt (no backoff): runDataLane resets its attempt
// counter to 0 on any clean return from dialDataLane, and dataLaneBackoff(0)
// is 0. This is the behavior a request blocked in the hub's
// awaitDataLane wait relies on to have a real chance of the lane coming back
// within its bounded wait, rather than riding out dataLaneBackoffBase (~1s)
// or worse.
func TestDataLane_ImmediateReconnectAfterAttachThenDrop(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", dropAfterAttach: true}
	_, stop := runAgainst(t, s, true)
	t.Cleanup(func() { _ = stop() })

	deadline := time.Now().Add(3 * time.Second)
	var times []time.Time
	for time.Now().Before(deadline) {
		times = s.helloTimestamps()
		if len(times) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(times) < 2 {
		t.Fatalf("expected >= 2 data hellos (attach+drop, then an immediate reconnect), got %d", len(times))
	}
	gap := times[1].Sub(times[0])
	if gap > 300*time.Millisecond {
		t.Fatalf("reconnect after attach-then-drop took %v, want well under dataLaneBackoffBase (%v): the first retry after any successful attach must not pay backoff", gap, dataLaneBackoffBase)
	}
}

// TestDataLaneBackoff_ZeroAfterAttachThenGrowsOnConsecutiveFailures is a
// direct unit test of the backoff function itself: attempt 0 (the state
// runDataLane resets to after any successful attach) is an immediate retry,
// and each further consecutive failure grows the delay, capped at
// dataLaneBackoffMax.
func TestDataLaneBackoff_ZeroAfterAttachThenGrowsOnConsecutiveFailures(t *testing.T) {
	if got := dataLaneBackoff(0); got != 0 {
		t.Fatalf("dataLaneBackoff(0) = %v, want 0 (immediate retry right after an attach)", got)
	}
	// attempt=1 (the first consecutive failure): full jitter over
	// [base/2, base].
	if d := dataLaneBackoff(1); d < 0 || d > dataLaneBackoffBase {
		t.Fatalf("dataLaneBackoff(1) = %v, want in [0, %v]", d, dataLaneBackoffBase)
	}
	// attempt=2: the exponential step has doubled the ceiling.
	if d := dataLaneBackoff(2); d < 0 || d > 2*dataLaneBackoffBase {
		t.Fatalf("dataLaneBackoff(2) = %v, want in [0, %v]", d, 2*dataLaneBackoffBase)
	}
	// A large attempt count must still be capped at dataLaneBackoffMax, not
	// keep growing unbounded.
	for _, attempt := range []int{6, 20, 100} {
		if d := dataLaneBackoff(attempt); d < 0 || d > dataLaneBackoffMax {
			t.Fatalf("dataLaneBackoff(%d) = %v, want in [0, %v] (capped)", attempt, d, dataLaneBackoffMax)
		}
	}
}
