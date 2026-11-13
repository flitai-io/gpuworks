package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

type telemetrySinks struct {
	mu      sync.Mutex
	logs    []proto.LogLine
	samples []proto.MetricSample
}

func (s *telemetrySinks) install(h *Hub) {
	h.SetSinks(func(_ string, samples []proto.MetricSample) {
		s.mu.Lock()
		s.samples = append(s.samples, samples...)
		s.mu.Unlock()
	}, func(_ string, lines []proto.LogLine) {
		s.mu.Lock()
		s.logs = append(s.logs, lines...)
		s.mu.Unlock()
	})
}

func (s *telemetrySinks) wait(t *testing.T, logs, samples int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := len(s.logs) >= logs && len(s.samples) >= samples
		s.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("sinks got %d log lines and %d samples, want %d and %d", len(s.logs), len(s.samples), logs, samples)
}

// dialDataLane completes a real ?lane=data handshake and returns the agent's
// yamux client on it.
func dialDataLane(t *testing.T, srv *httptest.Server, hello proto.DataHello) *yamux.Session {
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
	reply, err := readLine(netConn)
	if err != nil {
		t.Fatalf("read data reply: %v", err)
	}
	var env proto.Envelope
	if err := json.Unmarshal(reply, &env); err != nil || env.T != proto.TDataAck {
		t.Fatalf("data reply = %s, %v; want ack", reply, err)
	}
	client, err := yamux.Client(netConn, laneYamuxConfig(proto.LaneData, io.Discard), nil)
	if err != nil {
		t.Fatalf("yamux: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func openAgentStream(t *testing.T, data *yamux.Session, kind string) *yamux.Stream {
	t.Helper()
	ref := ""
	if kind == proto.StreamTelemetry {
		ref = proto.TLogs
	}
	return openAgentStreamRef(t, data, kind, ref)
}

func openAgentStreamRef(t *testing.T, data *yamux.Session, kind, ref string) *yamux.Stream {
	t.Helper()
	stream, err := data.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("open %s stream: %v", kind, err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	hdr, _ := json.Marshal(proto.StreamHeader{Kind: kind, Ref: ref})
	if _, err := stream.Write(append(hdr, '\n')); err != nil {
		t.Fatalf("write %s header: %v", kind, err)
	}
	return stream
}

func TestServerHelloAdvertisesTelemetryStream(t *testing.T) {
	h := newTestHub(t)
	if caps := h.serverHello().Caps; !slices.Contains(caps, proto.CapTelemetryStream) {
		t.Fatalf("server hello caps = %v, want %q", caps, proto.CapTelemetryStream)
	}
}

// An agent's telemetry stream on its data lane delivers metric and log
// batches to the same sinks as ctrl does, in order; a control envelope
// slipped onto it does not end the stream.
func TestTelemetryStreamOnDataLaneReachesTheSinks(t *testing.T) {
	h := newTestHub(t)
	sinks := &telemetrySinks{}
	sinks.install(h)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	stream := openAgentStream(t, data, proto.StreamTelemetry)
	enc := json.NewEncoder(stream)
	lines := []proto.LogLine{{Instance: "inst-1", Line: "first", Source: "instance"}, {Instance: "inst-1", Line: "second", Source: "instance"}}
	mustEncode(t, enc, proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: lines[:1]})})
	mustEncode(t, enc, proto.Envelope{T: proto.TStatusInstance, D: mustJSON(t, proto.InstanceStatus{UUID: "inst-1", State: proto.InstFailed})})
	mustEncode(t, enc, proto.Envelope{T: proto.TMetrics, D: mustJSON(t, proto.MetricBatch{Samples: []proto.MetricSample{{Metric: "gpuworks_gpu_utilization", Value: 42, TimeMS: 1}}})})
	mustEncode(t, enc, proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: lines[1:]})})

	sinks.wait(t, 2, 1)
	sinks.mu.Lock()
	defer sinks.mu.Unlock()
	if sinks.logs[0].Line != "first" || sinks.logs[1].Line != "second" {
		t.Fatalf("logs = %+v, want first then second", sinks.logs)
	}
	if sinks.samples[0].Metric != "gpuworks_gpu_utilization" || sinks.samples[0].Value != 42 {
		t.Fatalf("samples = %+v", sinks.samples)
	}
}

// The data lane accepts no other agent-opened kind: such a stream is reset,
// and the lane stays usable for telemetry.
func TestAgentOpenedDataStreamOfOtherKindIsReset(t *testing.T) {
	h := newTestHub(t)
	sinks := &telemetrySinks{}
	sinks.install(h)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	bogus := openAgentStream(t, data, proto.StreamTunnel)
	done := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(bogus).ReadByte()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a refused stream returned data")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an agent-opened tunnel stream was neither served nor reset")
	}

	stream := openAgentStream(t, data, proto.StreamTelemetry)
	mustEncode(t, json.NewEncoder(stream), proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "after the refusal"}}})})
	sinks.wait(t, 1, 0)
}

func mustEncode(t *testing.T, enc *json.Encoder, env proto.Envelope) {
	t.Helper()
	if err := enc.Encode(env); err != nil {
		t.Fatalf("encode %s: %v", env.T, err)
	}
}

// After a failed write the agent closes the stream: the complete lines it
// already sent are still read, and the unterminated one the failure left is
// not taken as a batch, since the agent sends it again.
func TestTelemetryKeepsCompleteLinesAndDropsAnUnterminatedOne(t *testing.T) {
	h := newTestHub(t)
	sinks := &telemetrySinks{}
	sinks.install(h)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	stream := openAgentStream(t, data, proto.StreamTelemetry)
	mustEncode(t, json.NewEncoder(stream), proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "complete"}}})})
	cut, _ := json.Marshal(proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "cut short"}}})})
	if _, err := stream.Write(cut); err != nil { // complete JSON, no newline
		t.Fatal(err)
	}
	_ = stream.Close()

	again := openAgentStream(t, data, proto.StreamTelemetry)
	mustEncode(t, json.NewEncoder(again), proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "sent again"}}})})
	sinks.wait(t, 2, 0)
	time.Sleep(50 * time.Millisecond)
	sinks.mu.Lock()
	defer sinks.mu.Unlock()
	if len(sinks.logs) != 2 || sinks.logs[0].Line != "complete" || sinks.logs[1].Line != "sent again" {
		t.Fatalf("logs = %+v, want complete then sent again", sinks.logs)
	}
}

// gatedIngest is a waiting log sink that holds every batch until released,
// recording the order batches were ingested in.
type gatedIngest struct {
	release chan struct{}
	entered chan string
	mu      sync.Mutex
	lines   []string
}

func newGatedIngest(h *Hub) *gatedIngest {
	g := &gatedIngest{release: make(chan struct{}), entered: make(chan string, 64)}
	h.SetLogIngestWait(func(ctx context.Context, _ string, lines []proto.LogLine) error {
		g.entered <- lines[0].Line
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.mu.Lock()
		for _, l := range lines {
			g.lines = append(g.lines, l.Line)
		}
		g.mu.Unlock()
		return nil
	})
	return g
}

func (g *gatedIngest) waitLines(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.mu.Lock()
		got := append([]string(nil), g.lines...)
		g.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// While ingest makes the server wait, the agent may give up on a stream and
// open another: the lines buffered on the first are still ingested, and
// before the second stream's.
func TestTelemetryIngestsAStreamsBufferedLinesBeforeTheNextStream(t *testing.T) {
	h := newTestHub(t)
	g := newGatedIngest(h)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	first := openAgentStream(t, data, proto.StreamTelemetry)
	enc := json.NewEncoder(first)
	for _, line := range []string{"first-1", "first-2", "first-3"} {
		mustEncode(t, enc, proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: line}}})})
	}
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the first batch never reached ingest")
	}
	_ = first.Close()
	second := openAgentStream(t, data, proto.StreamTelemetry)
	mustEncode(t, json.NewEncoder(second), proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "second-1"}}})})
	time.Sleep(100 * time.Millisecond)
	select {
	case line := <-g.entered:
		t.Fatalf("%q reached ingest while the first stream was still being read", line)
	default:
	}
	close(g.release)

	got := g.waitLines(t, 4)
	if strings.Join(got, ",") != "first-1,first-2,first-3,second-1" {
		t.Fatalf("ingested %v, want the first stream's lines, then the second's", got)
	}
}

// A batch already read when the node's session ends still gets its grace
// period to find ingest room.
func TestTelemetryBatchSurvivesASessionCloseWithinTheGrace(t *testing.T) {
	h := newTestHub(t)
	g := newGatedIngest(h)
	uuid, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	stream := openAgentStream(t, data, proto.StreamTelemetry)
	mustEncode(t, json.NewEncoder(stream), proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "in flight"}}})})
	<-g.entered
	h.session(uuid).Close()
	time.Sleep(100 * time.Millisecond)
	close(g.release)
	if got := g.waitLines(t, 1); len(got) != 1 || got[0] != "in flight" {
		t.Fatalf("ingested %v after the session closed, want the in-flight batch", got)
	}
}

// A line past the size bound ends the stream instead of growing without end.
func TestTelemetryRefusesAnOversizedLine(t *testing.T) {
	old := telemetryMaxLine.Load()
	telemetryMaxLine.Store(1 << 10)
	t.Cleanup(func() { telemetryMaxLine.Store(old) })
	h := newTestHub(t)
	sinks := &telemetrySinks{}
	sinks.install(h)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	stream := openAgentStream(t, data, proto.StreamTelemetry)
	enc := json.NewEncoder(stream)
	mustEncode(t, enc, proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: strings.Repeat("x", 4<<10)}}})})
	// The server may already have reset the stream: this write may fail.
	_ = enc.Encode(proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "after the oversized line"}}})})
	time.Sleep(200 * time.Millisecond)
	sinks.mu.Lock()
	n := len(sinks.logs)
	sinks.mu.Unlock()
	if n != 0 {
		t.Fatalf("logs = %d lines, want the stream ended at the oversized line", n)
	}
	// Reset, not closed: the agent's next write fails, so it sends that
	// batch again instead of losing it in an unread receive window.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := stream.Write([]byte("{}\n"))
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writes after the refused line kept succeeding; the stream was not reset")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Telemetry logs go to the waiting sink, never the dropping one, and a wait
// ends a grace period after the node's session does.
func TestTelemetryLogsWaitForIngestUntilTheSessionCloses(t *testing.T) {
	old := telemetryCloseGrace.Load()
	telemetryCloseGrace.Store(int64(50 * time.Millisecond))
	t.Cleanup(func() { telemetryCloseGrace.Store(old) })
	h := newTestHub(t)
	sinks := &telemetrySinks{}
	sinks.install(h)
	entered := make(chan struct{}, 1)
	released := make(chan error, 1)
	h.SetLogIngestWait(func(ctx context.Context, _ string, _ []proto.LogLine) error {
		entered <- struct{}{}
		<-ctx.Done()
		released <- ctx.Err()
		return ctx.Err()
	})
	uuid, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	stream := openAgentStream(t, data, proto.StreamTelemetry)
	mustEncode(t, json.NewEncoder(stream), proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "waits"}}})})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the telemetry log batch never reached the waiting sink")
	}
	h.session(uuid).Close()
	select {
	case err := <-released:
		if err == nil {
			t.Fatal("the wait ended without an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closing the session did not end the ingest wait")
	}
	sinks.mu.Lock()
	defer sinks.mu.Unlock()
	if len(sinks.logs) != 0 {
		t.Fatalf("the dropping sink got %+v as well", sinks.logs)
	}
}

// A telemetry stream names its batch type; one that does not is refused.
func TestTelemetryStreamWithoutABatchTypeIsReset(t *testing.T) {
	h := newTestHub(t)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})
	for _, ref := range []string{"", "status.instance"} {
		stream := openAgentStreamRef(t, data, proto.StreamTelemetry, ref)
		done := make(chan error, 1)
		go func() {
			_, err := bufio.NewReader(stream).ReadByte()
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("ref %q: a refused stream returned data", ref)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("ref %q: the stream was neither served nor reset", ref)
		}
	}
}

// Metrics keep arriving while the log stream waits for ingest room: a slow
// log store must not stall the monitoring and routing that metrics feed.
func TestTelemetryMetricsFlowWhileLogsWaitForIngest(t *testing.T) {
	h := newTestHub(t)
	sinks := &telemetrySinks{}
	sinks.install(h)
	g := newGatedIngest(h)
	defer close(g.release)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	logs := openAgentStreamRef(t, data, proto.StreamTelemetry, proto.TLogs)
	mustEncode(t, json.NewEncoder(logs), proto.Envelope{T: proto.TLogs, D: mustJSON(t, proto.LogBatch{Lines: []proto.LogLine{{Line: "held"}}})})
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the log batch never reached ingest")
	}
	metrics := openAgentStreamRef(t, data, proto.StreamTelemetry, proto.TMetrics)
	mustEncode(t, json.NewEncoder(metrics), proto.Envelope{T: proto.TMetrics, D: mustJSON(t, proto.MetricBatch{Samples: []proto.MetricSample{{Metric: "m", Value: 1}}})})
	sinks.wait(t, 0, 1)
}

// A malformed line resets the stream like an oversized one: the agent's next
// write fails and it sends that batch again on a new stream.
func TestTelemetryResetsAStreamAfterAMalformedLine(t *testing.T) {
	h := newTestHub(t)
	_, token := newTestNode(t, h.store)
	srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(srv.Close)
	_, reg := dialCtrlLane(t, srv, token)
	data := dialDataLane(t, srv, proto.DataHello{NodeToken: token, SessionID: reg.SessionID})

	stream := openAgentStream(t, data, proto.StreamTelemetry)
	if _, err := stream.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := stream.Write([]byte("{}\n")); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("writes after the malformed line kept succeeding; the stream was not reset")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
