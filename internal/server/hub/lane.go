package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

// dataLaneReattachWait bounds how long a data-plane stream open waits for a
// detached data lane to reattach before giving up. The agent's first
// reconnect after a clean attach-then-drop is immediate (see
// internal/agent/conn/datalane.go's backoff reset), so this window exists
// only to absorb that one reconnect round trip plus the ctrl-lane RPC latency
// the agent needs to notice the drop — not to ride out the agent's full
// backoff ceiling (30s). A caller with its own deadline (an HTTP request)
// still bounds the wait tighter through ctx.
const dataLaneReattachWait = 5 * time.Second

// ErrDataLaneUnavailable is returned by a data-plane stream open when the
// data lane is detached and did not reattach within dataLaneReattachWait (or
// the ctrl session closed first). It is never satisfied by falling back onto
// the ctrl session: mixing data-plane traffic onto ctrl is exactly the
// coupling the dual-lane design exists to prevent. Callers that can retry
// against a different node/instance (the proxy's retry transport) should
// treat it as a connection-level failure; callers that cannot should surface
// it as a retryable/unavailable condition rather than hanging or silently
// degrading.
var ErrDataLaneUnavailable = errors.New("data lane unavailable")

// newSessionID returns a random 16-byte hex token identifying one ctrl session.
// A data lane must present it to attach, so it must be unpredictable.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// dataLaneRoute resolves which yamux session a stream of kind should open on.
//
//   - Ctrl-plane kinds always use s.ysess; usedData is false.
//   - Data-plane kinds use the attached data lane when one is healthy.
//   - Data-plane kinds use s.ysess when no data lane was ever attached: a
//     legacy agent, --data-lane=false, and server_dial keep a single physical
//     connection, and that is their normal, permanent shape rather than a
//     fault to wait out. usedData is false in this case too.
//   - Data-plane kinds on a detached lane (attached before, currently down)
//     block in awaitDataLane instead of falling back to ctrl.
//
// usedData tells the caller whether the returned session is the data lane, so
// it knows to treat a subsequent OpenStream/write failure on it as a detach
// (see Session.openStream) rather than a plain error.
func (s *Session) dataLaneRoute(ctx context.Context, kind string) (ysess *yamux.Session, usedData bool, err error) {
	if proto.LaneFor(kind) != proto.LaneData {
		return s.ysess, false, nil
	}
	s.dataMu.Lock()
	if s.data != nil && !s.data.IsClosed() {
		data := s.data
		s.dataMu.Unlock()
		return data, true, nil
	}
	ever := s.dataEver
	s.dataMu.Unlock()
	if !ever {
		s.ctrlDataPlaneStreams.Add(1)
		return s.ysess, false, nil
	}
	data, err := s.awaitDataLane(ctx)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// awaitDataLane blocks until a data lane is attached, the wait budget
// (dataLaneReattachWait, further bounded by ctx) elapses, or the ctrl session
// closes — at which point waiting is pointless: a closed ctrl session means
// the node is offline and nothing will attach a new data lane to it. It never
// polls: notifyDataChangeLocked wakes it on every attach/detach transition.
func (s *Session) awaitDataLane(ctx context.Context) (*yamux.Session, error) {
	waitCtx, cancel := context.WithTimeout(ctx, dataLaneReattachWait)
	defer cancel()
	for {
		s.dataMu.Lock()
		if s.data != nil && !s.data.IsClosed() {
			data := s.data
			s.dataMu.Unlock()
			return data, nil
		}
		changed := s.dataChangeCh
		s.dataMu.Unlock()
		select {
		case <-changed:
			continue
		case <-s.closed:
			return nil, fmt.Errorf("%w: node %s ctrl session closed while waiting for the data lane", ErrDataLaneUnavailable, s.NodeUUID)
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if s.hub != nil {
				s.hub.usage.Inc(usagereport.KindProtocol, "data_lane_unavailable")
			}
			return nil, fmt.Errorf("%w: node %s data lane did not reattach within %s", ErrDataLaneUnavailable, s.NodeUUID, dataLaneReattachWait)
		}
	}
}

// DataLaneState reports the data lane's state for diagnostics and tests:
//   - "attached": a healthy data lane is currently bound to this ctrl session;
//   - "detached": a data lane was attached at least once but is currently gone
//     (dropped and not yet reconnected);
//   - "none": no data lane was ever attached (a single-lane connection, or a
//     dual-lane one whose data lane has not attached yet).
func (s *Session) DataLaneState() string {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	if s.data != nil && !s.data.IsClosed() {
		return "attached"
	}
	if s.dataEver {
		return "detached"
	}
	return "none"
}

// attachData binds a freshly negotiated data-lane yamux session to this ctrl
// session, replacing (and closing) any prior one. A watcher goroutine detaches
// it when it closes so DataLaneState falls back to "detached" and data-plane
// stream opens wait (awaitDataLane) for the agent to reconnect the lane,
// rather than sliding onto ctrl.
func (s *Session) attachData(data *yamux.Session) {
	s.dataMu.Lock()
	old := s.data
	s.data = data
	s.dataEver = true
	s.notifyDataChangeLocked()
	s.dataMu.Unlock()
	if old != nil && old != data {
		_ = old.Close()
	}
	s.hub.log.Info("data lane attached", "node", s.NodeUUID)
	go s.watchData(data)
	go s.acceptDataStreams(data)
}

// watchData clears the data pointer once the lane closes, but only if it is
// still the current one (a replacement must not be detached by the old watcher).
func (s *Session) watchData(data *yamux.Session) {
	select {
	case <-data.CloseChan():
	case <-s.closed:
		return
	}
	s.dataMu.Lock()
	detached := false
	if s.data == data {
		s.data = nil
		detached = true
		s.notifyDataChangeLocked()
	}
	s.dataMu.Unlock()
	if detached {
		s.hub.log.Info("data lane detached", "node", s.NodeUUID)
	}
}

// detachDataIfCurrent marks the data lane detached immediately, without
// waiting for CloseChan to fire, and closes dead — openStream calls this only
// once it has confirmed (see writeStreamHeader's dead return) that the
// session really is gone, so there is no half-open connection left dangling
// behind the detach: the peer sees it close right away instead of whenever
// its own keepalive eventually notices, which is also what lets the agent
// start reconnecting the lane sooner rather than later. A no-op if dead is no
// longer the current data lane (already replaced or detached by the ordinary
// watchData path) — Close is called only by whichever caller actually wins
// the detach, not unconditionally.
func (s *Session) detachDataIfCurrent(dead *yamux.Session) {
	s.dataMu.Lock()
	replaced := s.data == dead
	if replaced {
		s.data = nil
		s.notifyDataChangeLocked()
	}
	s.dataMu.Unlock()
	if replaced {
		_ = dead.Close()
	}
}

// notifyDataChangeLocked wakes every waiter parked in awaitDataLane on an
// attach or detach transition. Callers must hold dataMu.
func (s *Session) notifyDataChangeLocked() {
	close(s.dataChangeCh)
	s.dataChangeCh = make(chan struct{})
}

// DataLaneState reports the per-node data-lane state (attached/detached/none),
// or "none" when the node has no live session.
func (h *Hub) DataLaneState(uuid string) string {
	sess := h.session(uuid)
	if sess == nil {
		return "none"
	}
	return sess.DataLaneState()
}

// CtrlDataPlaneStreamCount reports how many data-plane-kind streams the
// node's session has ever opened on ctrl (0 if the node has no live
// session). See Session.ctrlDataPlaneStreams.
func (h *Hub) CtrlDataPlaneStreamCount(uuid string) int64 {
	sess := h.session(uuid)
	if sess == nil {
		return 0
	}
	return sess.CtrlDataPlaneStreamCount()
}

// serveDataLane handles a GET /api/v1/agent/connect?lane=data WebSocket. The
// agent writes one DataHello JSON line before yamux; the server authenticates
// the node token and requires the current ctrl SessionID, then acks and layers
// the data-profile yamux session onto the live ctrl session. It writes exactly
// one line before yamux in every case: a TDataAck envelope on success or a
// TReject envelope on failure (a garbage/missing hello just closes).
func (h *Hub) serveDataLane(netConn net.Conn, c *websocket.Conn, remote string) {
	line, err := readLine(netConn)
	if err != nil {
		// No usable hello line: close without attaching and without a reply.
		_ = netConn.Close()
		_ = c.Close(websocket.StatusUnsupportedData, "data hello")
		return
	}
	var hello proto.DataHello
	if err := json.Unmarshal(line, &hello); err != nil {
		_ = netConn.Close()
		_ = c.Close(websocket.StatusUnsupportedData, "data hello")
		return
	}

	n, err := h.authNodeToken(hello.NodeToken)
	if err != nil {
		h.recordReject(hello.NodeToken, proto.RejectBadToken, remote)
		h.writeDataReject(netConn, c, proto.RejectBadToken)
		return
	}
	sess := h.session(n.UUID)
	if sess == nil || sess.sessionID == "" || sess.sessionID != hello.SessionID {
		// The ctrl session is gone or the presented id is stale: a data lane must
		// never attach to a session other than the one that issued its id.
		h.recordReject(hello.NodeToken, proto.RejectStaleSession, remote)
		h.writeDataReject(netConn, c, proto.RejectStaleSession)
		return
	}

	// Ack before yamux so the agent knows to start its client session.
	if err := writeEnvelopeLine(netConn, proto.TDataAck, nil); err != nil {
		_ = netConn.Close()
		_ = c.Close(websocket.StatusInternalError, "ack")
		return
	}
	ycfg := laneYamuxConfig(proto.LaneData, &slogWriter{log: h.log})
	data, err := yamux.Server(netConn, ycfg, nil)
	if err != nil {
		_ = netConn.Close()
		_ = c.Close(websocket.StatusInternalError, "yamux")
		return
	}
	sess.attachData(data)

	// The ctrl session owns node liveness, and attachData serves the streams
	// the agent opens on the lane, so waiting on close is enough to keep the
	// WebSocket net.Conn alive.
	<-data.CloseChan()
	_ = netConn.Close()
	_ = c.Close(websocket.StatusNormalClosure, "data closed")
}

// writeDataReject sends one TReject envelope line, flushes it, and closes.
func (h *Hub) writeDataReject(netConn net.Conn, c *websocket.Conn, reason string) {
	_ = writeEnvelopeLine(netConn, proto.TReject, proto.Reject{Reason: reason})
	flushBeforeClose(netConn)
	_ = netConn.Close()
	_ = c.Close(websocket.StatusPolicyViolation, reason)
}

// writeEnvelopeLine marshals d into an Envelope of type t and writes it as one
// JSON line. d may be nil for a bodyless envelope (e.g. the data ack).
func writeEnvelopeLine(w net.Conn, t string, d any) error {
	var raw json.RawMessage
	if d != nil {
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		raw = b
	}
	line, err := json.Marshal(proto.Envelope{T: t, D: raw})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = w.Write(line)
	return err
}

// flushBeforeClose gives a just-written reject line a chance to reach the agent
// before the caller closes the socket. go-yamux is not involved here (the reject
// is written on the raw WebSocket net.Conn before any yamux layering), but the
// coder/websocket NetConn still buffers, so we half-close our write side to send
// a FIN in order after the queued bytes, then drain until the peer hangs up.
func flushBeforeClose(nc net.Conn) {
	if cw, ok := nc.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	for {
		if _, err := nc.Read(buf); err != nil {
			return
		}
	}
}
