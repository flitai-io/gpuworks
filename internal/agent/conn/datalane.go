package conn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/version"
	"gpuworks.ai/gpuworks/internal/wsconn"
)

// Data-lane reconnect backoff, mirroring the ctrl reconnect policy in
// internal/agent/app (newBackoff(1s, 30s), jittered exponential). Package conn
// cannot import app, so the policy is duplicated here; keep the two in step.
const (
	dataLaneBackoffBase = 1 * time.Second
	dataLaneBackoffMax  = 30 * time.Second
)

// runDataLane opens and maintains the dedicated data lane for the lifetime of
// ctx (the ctrl connection). It dials, authenticates with a DataHello, and on
// success layers a data-profile yamux client, then serves inbound data-plane
// streams until the lane drops; it then reconnects with backoff. It never
// touches the ctrl session, so a flapping data lane cannot disturb control
// traffic. A server that rejects the hello (e.g. the ctrl session rotated) ends
// one attempt; the next backoff round re-reads the current SessionID via the
// caller and tries again.
func (c *Conn) runDataLane(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.dialDataLane(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			var rej *RejectError
			if errors.As(err, &rej) {
				// A reject is expected transient state (the ctrl session id the
				// server holds differs from ours mid-reconnect); log once at debug
				// and retry, never escalating to the ctrl loop.
				c.log.Debug("data lane rejected", "reason", safeRejectReason(rej.Reason))
			} else {
				c.log.Debug("data lane ended", "err", safeConnectionError(err))
			}
			attempt++
		} else {
			// A clean return means the lane served for a while and then closed;
			// reset backoff so a transient drop recovers promptly.
			attempt = 0
		}
		delay := dataLaneBackoff(attempt)
		if err != nil {
			c.emitDataConnectionFailure(err, time.Now().Add(delay))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// dialDataLane performs one data-lane connection attempt: dial, DataHello, wait
// for ack (or reject), then run the data-profile yamux client until it closes.
func (c *Conn) dialDataLane(ctx context.Context) (err error) {
	client, observation := c.observeHTTPClient(http.DefaultClient, false)
	stage := "dial"
	attempt := fmt.Sprintf("%016x%016x", time.Now().UnixNano(), connectionAttemptSequence.Add(1))
	defer func() {
		if err != nil {
			observed := observation.current()
			if stage == "dial" && observed != "" {
				stage = observed
			}
			err = &connectionFailure{reason: connectionReason(err), cause: err, stage: stage, attempt: attempt}
		}
	}()
	wsURL := toWSURL(c.creds.ServerURL) + "/api/v1/agent/connect?lane=data"
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dialCtx = observation.context(dialCtx)
	//nolint:bodyclose // coder/websocket.Dial owns handshake bodies; NetConn below owns the upgraded connection.
	ws, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"User-Agent": []string{"gpuworks-agent/" + version.Version}},
	})
	if err != nil {
		// An old server has no data route: it answers non-101. Report it once so
		// the operator sees single-lane operation, then let backoff keep it quiet.
		if resp != nil {
			return &dataLaneDialError{status: resp.StatusCode, err: err}
		}
		return err
	}
	ws.SetReadLimit(-1)
	netConn := wsconn.Wrap(websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
	defer netConn.Close()

	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = netConn.Close()
		case <-stopClose:
		}
	}()

	stage = "hello"
	hello := proto.DataHello{NodeToken: c.creds.NodeToken, SessionID: c.sessionID}
	line, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := netConn.Write(line); err != nil {
		return err
	}

	// The server replies with exactly one line before yamux: an ack on success
	// or a reject envelope on failure.
	stage = "wait_registered"
	respLine, err := readLine(netConn)
	if err != nil {
		return err
	}
	var env proto.Envelope
	if err := json.Unmarshal(respLine, &env); err != nil {
		return err
	}
	switch env.T {
	case proto.TDataAck:
		// proceed to yamux
	case proto.TReject:
		var rej proto.Reject
		_ = json.Unmarshal(env.D, &rej)
		return &RejectError{Reason: rej.Reason, Detail: rej.Detail}
	default:
		return errors.New("data lane: unexpected reply " + env.T)
	}

	stage = "yamux"
	ycfg := laneYamuxConfig(proto.LaneData, io.Discard)
	data, err := yamux.Client(netConn, ycfg, nil)
	if err != nil {
		return err
	}
	c.setDataSess(data)
	defer c.clearDataSess(data)
	c.log.Info("data lane connected")
	stage = "session"

	// Serve server-initiated data-plane streams on the data lane until it closes.
	for {
		stream, err := data.AcceptStream()
		if err != nil {
			if ctx.Err() == nil {
				c.emitDataConnectionFailure(&connectionFailure{reason: "transport", cause: err, stage: stage, attempt: attempt}, time.Time{})
			}
			return nil // lane closed; caller decides whether to reconnect
		}
		go c.handleStream(ctx, stream)
	}
}

func (c *Conn) setDataSess(s *yamux.Session) {
	c.dataMu.Lock()
	old := c.dataSess
	c.dataSess = s
	if s != nil {
		c.dataEver = true
	}
	c.dataMu.Unlock()
	if old != nil && old != s {
		_ = old.Close()
	}
}

func (c *Conn) clearDataSess(s *yamux.Session) {
	c.dataMu.Lock()
	if c.dataSess == s {
		c.dataSess = nil
	}
	c.dataMu.Unlock()
	_ = s.Close()
}

// closeDataLane tears down any live data lane. Called when the ctrl connection
// ends so the data lane never outlives its parent.
func (c *Conn) closeDataLane() {
	c.dataMu.Lock()
	s := c.dataSess
	c.dataSess = nil
	c.dataMu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// dataLaneBackoff is a jittered exponential backoff matching the ctrl policy.
func dataLaneBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	d := dataLaneBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= dataLaneBackoffMax {
			d = dataLaneBackoffMax
			break
		}
	}
	// Full jitter over [d/2, d].
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// dataLaneDialError distinguishes a non-101 answer (old server without the data
// route) from a transport failure, so the caller can log it distinctly.
type dataLaneDialError struct {
	status int
	err    error
}

func (e *dataLaneDialError) Error() string { return e.err.Error() }
func (e *dataLaneDialError) Unwrap() error { return e.err }
