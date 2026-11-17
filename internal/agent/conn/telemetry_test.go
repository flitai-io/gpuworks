package conn

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func (s *laneTestServer) kinds() (ctrl, telemetry []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ctrlKinds...), append([]string(nil), s.telemetry...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDataLane(t *testing.T, c *Conn, attached bool) {
	t.Helper()
	waitFor(t, "data lane state", func() bool {
		c.dataMu.Lock()
		defer c.dataMu.Unlock()
		return (c.dataSess != nil && !c.dataSess.IsClosed()) == attached
	})
}

func logBatch(line string) proto.LogBatch {
	return proto.LogBatch{Lines: []proto.LogLine{{Line: line}}}
}

// With a server that accepts it and a live data lane, metric and log batches
// ride the telemetry stream and nothing else does.
func TestTelemetryRidesTheDataLaneWhenTheServerAcceptsIt(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", caps: []string{proto.CapTelemetryStream}}
	c, stop := runAgainst(t, s, true)
	defer stop()
	waitDataLane(t, c, true)
	send := c.handlerSend(t.Context())

	if err := send(proto.TLogs, logBatch("one")); err != nil {
		t.Fatal(err)
	}
	if err := send(proto.TMetrics, proto.MetricBatch{Samples: []proto.MetricSample{{Metric: "m", Value: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := send(proto.TLogs, logBatch("two")); err != nil {
		t.Fatal(err)
	}
	if err := send(proto.TStatusInstance, proto.InstanceStatus{UUID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three telemetry batches", func() bool { _, tel := s.kinds(); return len(tel) == 3 })
	waitFor(t, "the instance status on ctrl", func() bool { ctrl, _ := s.kinds(); return slices.Contains(ctrl, proto.TStatusInstance) })
	ctrl, tel := s.kinds()
	slices.Sort(tel)
	if !slices.Equal(tel, []string{proto.TLogs, proto.TLogs, proto.TMetrics}) {
		t.Fatalf("telemetry streams carried %v", tel)
	}
	if got := s.logLines(); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("log lines = %v, want one then two", got)
	}
	if slices.Contains(ctrl, proto.TLogs) || slices.Contains(ctrl, proto.TMetrics) {
		t.Fatalf("ctrl carried telemetry: %v", ctrl)
	}
}

// A server that does not advertise the capability never sees a telemetry
// stream: the batches stay on ctrl, as they always have.
func TestTelemetryStaysOnCtrlWithAnOlderServer(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1"}
	c, stop := runAgainst(t, s, true)
	defer stop()
	waitDataLane(t, c, true)
	if err := c.handlerSend(t.Context())(proto.TLogs, logBatch("old server")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch on ctrl", func() bool { ctrl, _ := s.kinds(); return slices.Contains(ctrl, proto.TLogs) })
	if _, tel := s.kinds(); len(tel) != 0 {
		t.Fatalf("an older server got telemetry stream traffic: %v", tel)
	}
}

// A connection that never had a data lane keeps the batches on ctrl.
func TestTelemetryStaysOnCtrlWithoutADataLane(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", caps: []string{proto.CapTelemetryStream}}
	c, stop := runAgainst(t, s, false)
	defer stop()
	waitFor(t, "the handshake", func() bool { ctrl, _ := s.kinds(); return len(ctrl) > 0 })
	if err := c.handlerSend(t.Context())(proto.TLogs, logBatch("single lane")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch on ctrl", func() bool { ctrl, _ := s.kinds(); return slices.Contains(ctrl, proto.TLogs) })
}

// Once the data lane has been attached, its loss does not move telemetry onto
// ctrl: the send fails so the caller can hold the batch, and the next batch
// after the lane returns opens a fresh stream on the new lane.
func TestTelemetryWaitsForADetachedDataLane(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", caps: []string{proto.CapTelemetryStream}}
	c, stop := runAgainst(t, s, true)
	defer stop()
	waitDataLane(t, c, true)
	send := c.handlerSend(t.Context())
	if err := send(proto.TLogs, logBatch("before")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first batch", func() bool { _, tel := s.kinds(); return len(tel) == 1 })

	s.mu.Lock()
	first := s.dataSess
	s.holdData = true
	s.mu.Unlock()
	_ = first.Close()
	waitDataLane(t, c, false)
	if err := send(proto.TLogs, logBatch("while detached")); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("send while detached = %v, want ErrTelemetryUnavailable", err)
	}
	if ctrl, _ := s.kinds(); slices.Contains(ctrl, proto.TLogs) {
		t.Fatalf("a detached data lane moved telemetry onto ctrl: %v", ctrl)
	}

	s.mu.Lock()
	s.holdData = false
	s.mu.Unlock()
	waitDataLane(t, c, true)
	waitFor(t, "a send on the reattached lane", func() bool { return send(proto.TLogs, logBatch("after")) == nil })
	waitFor(t, "the batch on the new lane", func() bool { _, tel := s.kinds(); return len(tel) == 2 })
}

// The capability is read from the server hello itself.
func TestHandshakeRecordsServerCapabilities(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", caps: []string{proto.CapTelemetryStream, "future.cap"}}
	c, stop := runAgainst(t, s, false)
	defer stop()
	waitFor(t, "the handshake", func() bool { ctrl, _ := s.kinds(); return len(ctrl) > 0 })
	if got := c.tel.serverCaps.Load(); got == nil || !slices.Equal(*got, s.caps) {
		t.Fatalf("server caps = %v, want %v", got, s.caps)
	}
}

// A connection that expects a data lane does not put telemetry on ctrl while
// the lane is still coming up: the batch waits (logs are held by the caller),
// and once the lane attaches it carries everything.
func TestTelemetryWaitsForTheExpectedDataLane(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", caps: []string{proto.CapTelemetryStream}, holdData: true}
	c, stop := runAgainst(t, s, true)
	defer stop()
	waitFor(t, "the handshake", func() bool { ctrl, _ := s.kinds(); return len(ctrl) > 0 })
	send := c.handlerSend(t.Context())
	if err := send(proto.TLogs, logBatch("early")); !errors.Is(err, ErrTelemetryUnavailable) {
		t.Fatalf("send before the lane = %v, want ErrTelemetryUnavailable", err)
	}
	s.mu.Lock()
	s.holdData = false
	s.mu.Unlock()
	waitDataLane(t, c, true)
	if err := send(proto.TLogs, logBatch("on the lane")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch on the lane", func() bool { _, tel := s.kinds(); return len(tel) == 1 })
	if ctrl, _ := s.kinds(); slices.Contains(ctrl, proto.TLogs) {
		t.Fatalf("telemetry reached ctrl while the lane was coming up: %v", ctrl)
	}
}

// A data lane that has not attached after the wait does not hold the node's
// telemetry: it goes on ctrl, and moves to the lane once the lane attaches.
func TestTelemetryUsesCtrlUntilALateDataLaneAttaches(t *testing.T) {
	s := &laneTestServer{sessionID: "sess-1", caps: []string{proto.CapTelemetryStream}, holdData: true}
	c, stop := runAgainstWith(t, s, true, func(c *Conn) { c.tel.laneWait = 100 * time.Millisecond })
	defer stop()
	waitFor(t, "the handshake", func() bool { ctrl, _ := s.kinds(); return len(ctrl) > 0 })
	send := c.handlerSend(t.Context())
	time.Sleep(150 * time.Millisecond)
	if err := send(proto.TLogs, logBatch("after the wait")); err != nil {
		t.Fatalf("send after the wait = %v, want it on ctrl", err)
	}
	waitFor(t, "the batch on ctrl", func() bool { ctrl, _ := s.kinds(); return slices.Contains(ctrl, proto.TLogs) })

	s.mu.Lock()
	s.holdData = false
	s.mu.Unlock()
	waitDataLane(t, c, true)
	if err := send(proto.TLogs, logBatch("lane attached later")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch on the lane", func() bool { _, tel := s.kinds(); return len(tel) == 1 })
}

// A write the server's backpressure times out closes the stream rather than
// resetting it: the complete batches already sent are still read, the cut
// one is not taken, and the caller's resend goes on a new stream.
func TestTelemetryWriteTimeoutKeepsTheBatchesAlreadySent(t *testing.T) {
	stall := make(chan struct{})
	s := &laneTestServer{sessionID: "sess-1", caps: []string{proto.CapTelemetryStream}, stallTelemetry: stall}
	c, stop := runAgainstWith(t, s, true, func(c *Conn) { c.tel.writeTimeout = 200 * time.Millisecond })
	defer stop()
	waitDataLane(t, c, true)
	send := c.handlerSend(t.Context())
	if err := send(proto.TLogs, logBatch("sent before the stall")); err != nil {
		t.Fatal(err)
	}
	// Larger than the data lane's initial stream window: it cannot all be
	// sent while the server is not reading.
	if err := send(proto.TLogs, logBatch(strings.Repeat("x", 512<<10))); err == nil {
		t.Fatal("a write past the window succeeded while the server was not reading")
	}
	// A real server stays in its ingest wait far longer than the agent's
	// close takes to arrive; let it arrive before the server reads on.
	time.Sleep(200 * time.Millisecond)
	s.mu.Lock()
	s.stallTelemetry = nil
	s.mu.Unlock()
	close(stall)
	if err := send(proto.TLogs, logBatch("resent after the stall")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both complete batches", func() bool { return len(s.logLines()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := s.logLines(); !slices.Equal(got, []string{"sent before the stall", "resent after the stall"}) {
		t.Fatalf("server read %d log lines, want the batch from before the stall and the resend", len(got))
	}
}

func (s *laneTestServer) logLines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.telemetryLines...)
}
