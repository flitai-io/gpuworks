package conn

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// telemetryOpenTimeout bounds opening a telemetry stream; telemetryWriteTimeout
// bounds one batch write, which flow control holds while the link drains or
// the server waits for ingest room. telemetryLaneWait is how long a
// connection that expects a data lane waits for it before sending on ctrl.
const (
	telemetryOpenTimeout  = 10 * time.Second
	telemetryWriteTimeout = 30 * time.Second
	telemetryLaneWait     = 30 * time.Second
)

// ErrTelemetryUnavailable is returned for a metric or log batch while the
// connection's telemetry belongs on a data lane that is not attached: not yet
// after the handshake, or down after having been attached. The batch is not
// moved onto ctrl; the caller keeps it (logs) or drops it (metrics) until the
// lane is there, as the server routes data-plane kinds (ADR 0024).
var ErrTelemetryUnavailable = errors.New("telemetry stream unavailable: data lane not attached")

type telemetryRoute int

const (
	telemetryUndecided telemetryRoute = iota
	telemetryOnCtrl
	telemetryOnData
)

// telemetry routes one connection's metric and log batches:
//
//   - on ctrl when the server does not advertise CapTelemetryStream, or the
//     connection will have no data lane;
//   - otherwise on the data lane, one StreamTelemetry stream per batch type
//     (the header's Ref names it), so a log stream the server holds back
//     while it waits for ingest room never holds metrics back;
//   - while the expected lane has not attached, sends fail with
//     ErrTelemetryUnavailable rather than putting a reconnect's backlog on
//     ctrl; after telemetryLaneWait they go on ctrl, until the lane attaches.
//
// Once on the data lane a connection never goes back to ctrl.
type telemetry struct {
	// serverCaps are the capabilities the server's hello advertised.
	serverCaps atomic.Pointer[[]string]

	mu         sync.Mutex
	expectData bool
	since      time.Time
	laneWait   time.Duration // zero means telemetryLaneWait; tests shorten it
	route      telemetryRoute
	// writeTimeout zero means telemetryWriteTimeout; tests shorten it.
	writeTimeout time.Duration

	metrics, logs telemetryStream
}

// telemetryStream is one batch type's stream on the current data lane,
// opened on first use and replaced along with the lane.
type telemetryStream struct {
	mu     sync.Mutex
	sess   *yamux.Session
	stream *yamux.Stream
}

// begin starts routing for a connection that has completed its handshake.
func (t *telemetry) begin(expectData bool) {
	t.mu.Lock()
	t.expectData, t.since, t.route = expectData, time.Now(), telemetryUndecided
	t.mu.Unlock()
}

func (t *telemetry) setServerCaps(caps []string) {
	caps = slices.Clone(caps)
	t.serverCaps.Store(&caps)
}

func (t *telemetry) serverAccepts() bool {
	caps := t.serverCaps.Load()
	return caps != nil && slices.Contains(*caps, proto.CapTelemetryStream)
}

// isTelemetry reports whether t is a batch the telemetry streams carry.
func isTelemetry(t string) bool { return t == proto.TMetrics || t == proto.TLogs }

func (c *Conn) dataLane() (*yamux.Session, bool) {
	c.dataMu.Lock()
	data := c.dataSess
	c.dataMu.Unlock()
	return data, data != nil && !data.IsClosed()
}

// telemetryRoute decides where the next batch goes (see telemetry).
func (c *Conn) telemetryRoute() (telemetryRoute, error) {
	_, attached := c.dataLane()
	c.tel.mu.Lock()
	defer c.tel.mu.Unlock()
	if c.tel.route == telemetryOnData {
		return telemetryOnData, nil
	}
	if !c.tel.expectData {
		c.tel.route = telemetryOnCtrl
		return telemetryOnCtrl, nil
	}
	if attached {
		if c.tel.route == telemetryOnCtrl {
			c.log.Info("data lane attached; telemetry moves to it")
		}
		c.tel.route = telemetryOnData
		return telemetryOnData, nil
	}
	if c.tel.route == telemetryOnCtrl {
		return telemetryOnCtrl, nil
	}
	wait := c.tel.laneWait
	if wait <= 0 {
		wait = telemetryLaneWait
	}
	if time.Since(c.tel.since) < wait {
		return telemetryUndecided, ErrTelemetryUnavailable
	}
	c.tel.route = telemetryOnCtrl
	// Warn reaches the log center as an agent log: the operator's way to
	// see a node whose telemetry is back on ctrl.
	c.log.Warn("data lane did not attach; telemetry uses ctrl until it does", "waited", wait)
	return telemetryOnCtrl, nil
}

// sendTelemetry sends a metric or log batch on the connection's telemetry
// route (see telemetry).
func (c *Conn) sendTelemetry(ctx context.Context, t string, d any) error {
	if !c.tel.serverAccepts() {
		return c.send(t, 0, d)
	}
	route, err := c.telemetryRoute()
	if err != nil {
		return err
	}
	if route == telemetryOnCtrl {
		return c.send(t, 0, d)
	}
	ts := &c.tel.metrics
	if t == proto.TLogs {
		ts = &c.tel.logs
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	// Read the lane under ts.mu: a sender that waited here behind a blocked
	// write must see the lane as it is now, not as it was when it arrived.
	data, attached := c.dataLane()
	if !attached {
		return ErrTelemetryUnavailable
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	line, err := json.Marshal(proto.Envelope{T: t, D: raw})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if ts.sess != data || ts.stream == nil {
		ts.closeLocked()
		stream, err := openTelemetry(ctx, data, t)
		if err != nil {
			return err
		}
		ts.sess, ts.stream = data, stream
	}
	timeout := c.tel.writeTimeout
	if timeout <= 0 {
		timeout = telemetryWriteTimeout
	}
	_ = ts.stream.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := ts.stream.Write(line); err != nil {
		// Close, not Reset: a reset stream discards the lines the server has
		// received but not yet read. The newline is the line's last byte, so
		// a failed write leaves an unterminated line, which the server drops;
		// the caller sends that batch again on a new stream.
		ts.closeLocked()
		return err
	}
	return nil
}

func openTelemetry(ctx context.Context, data *yamux.Session, batchType string) (*yamux.Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, telemetryOpenTimeout)
	defer cancel()
	stream, err := data.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	hdr, err := json.Marshal(proto.StreamHeader{Kind: proto.StreamTelemetry, Ref: batchType})
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	_ = stream.SetWriteDeadline(time.Now().Add(telemetryOpenTimeout))
	if _, err := stream.Write(append(hdr, '\n')); err != nil {
		_ = stream.Reset()
		return nil, err
	}
	return stream, nil
}

func (ts *telemetryStream) closeLocked() {
	if ts.stream != nil {
		_ = ts.stream.Close()
	}
	ts.sess, ts.stream = nil, nil
}

// closeTelemetry ends the telemetry streams with the connection.
func (c *Conn) closeTelemetry() {
	for _, ts := range []*telemetryStream{&c.tel.metrics, &c.tel.logs} {
		ts.mu.Lock()
		ts.closeLocked()
		ts.mu.Unlock()
	}
}
