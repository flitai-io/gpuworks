package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

var (
	errBadToken       = errors.New("invalid token")
	errSmokeHandshake = errors.New("smoke handshake complete")
)

type handshakeRejectError struct {
	reason string
	token  string
	err    error
}

func (e *handshakeRejectError) Error() string { return e.err.Error() }
func (e *handshakeRejectError) Unwrap() error { return e.err }

// Session is one live agent connection: the yamux session plus the ctrl stream.
type Session struct {
	hub                 *Hub
	ysess               *yamux.Session
	ctrl                io.ReadWriteCloser
	dec                 *json.Decoder
	writeMu             sync.Mutex
	imageStatusMu       sync.Mutex // serializes image inventory and operation persistence
	imageStatusRevision atomic.Uint64
	// Network saves coalesce independently from resource reconciliation.
	networkSyncOnce   sync.Once
	networkSyncNotify chan struct{}

	// fullPushMu serializes full desired-state replays and network updates, and
	// initialPushDone gates incremental updates behind the first replay
	// (see push.go); pendingUpdates buffers those updates meanwhile.
	fullPushMu      sync.Mutex
	initialPushDone chan struct{}
	stateMu         sync.Mutex
	pendingUpdates  []proto.StateUpdate
	NodeUUID        string
	nodeID          uint // authenticated identity, immutable after registration
	remote          string
	proto           int
	lastSeen        atomic.Int64 // unix ms
	// runtimeProxy is the container runtime daemon's proxy as the agent last
	// reported it (proto.NodeStatus.RuntimeProxy). It is not stored: the next
	// report after a server restart brings it back.
	runtimeProxy atomic.Pointer[proto.RuntimeProxyStatus]
	rpcSeq       atomic.Uint64

	// caps is what the agent advertised in its hello. Written during handshake,
	// which completes before hub.register publishes the session; every reader
	// reaches it through hub.session() under the hub mutex, so that pairing is
	// the happens-before edge and no extra lock is needed here.
	caps []string

	assetMu      sync.RWMutex
	assetDigests map[string]struct{}

	// sendFn, when set, replaces the wire send for tests.
	sendFn func(t string, id uint64, d any) error

	pendMu    sync.Mutex
	pending   map[uint64]chan proto.Envelope // in-flight RPC responses
	closed    chan struct{}
	closeOnce sync.Once

	// sessionID is a per-ctrl-handshake random token returned in Registered for
	// dual-lane connections. A data lane must present the current ctrl session's
	// id to attach; it is empty for legacy/single-lane sessions. Set once during
	// handshake before register.
	sessionID string
	// Upgrade identity is also present for legacy and server_dial sessions;
	// upgrade identity remains available independently of data-lane negotiation.
	upgradeSessionID         string
	upgradeVersion           string
	upgradeHandshakeComplete atomic.Bool
	upgradeReady             atomic.Bool

	// data is the optional dedicated high-throughput lane (a second yamux session
	// over its own WebSocket or reverse TCP connection). It carries the data-plane stream kinds
	// (tunnel/logtail/model/image) while ctrl/metrics/terminal stay on s.ysess.
	// Guarded by dataMu; nil when detached. dataEver records whether a data lane
	// was ever attached so DataLaneState can distinguish detached from none.
	// dataChangeCh is closed and replaced (see notifyDataChangeLocked) on every
	// attach/detach transition, so awaitDataLane can select on it instead of
	// polling for a session that is expected to come back within seconds.
	dataMu       sync.Mutex
	data         *yamux.Session
	dataEver     bool
	dataChangeCh chan struct{}

	// ctrlDataPlaneStreams is a diagnostics-only counter: it counts every
	// data-plane-kind stream opened on s.ysess (the ctrl session), which only
	// ever happens in the "none" state (no data lane was ever attached — a
	// legacy agent or --data-lane=false). For a session that has
	// attached a data lane at least once, this must stay 0 for its whole
	// lifetime, detached periods included: that is the property the no-
	// ctrl-fallback design exists to guarantee, and tests assert it directly
	// through CtrlDataPlaneStreamCount rather than inferring it from timing.
	ctrlDataPlaneStreams atomic.Int64
}

func newSession(h *Hub, ys *yamux.Session, ctrl io.ReadWriteCloser, remoteOpt ...string) *Session {
	remote := ""
	if len(remoteOpt) > 0 {
		remote = strings.TrimSpace(remoteOpt[0])
	}
	return &Session{
		hub:             h,
		ysess:           ys,
		ctrl:            ctrl,
		dec:             json.NewDecoder(bufio.NewReader(ctrl)),
		pending:         map[uint64]chan proto.Envelope{},
		closed:          make(chan struct{}),
		remote:          remote,
		assetDigests:    map[string]struct{}{},
		dataChangeCh:    make(chan struct{}),
		initialPushDone: make(chan struct{}),
	}
}

// handshake reads the agent hello, authenticates its per-node token, and
// replies with a registered identity followed by the server hello.
func (s *Session) handshake() error {
	var env proto.Envelope
	if err := s.dec.Decode(&env); err != nil {
		return err
	}
	if env.T != proto.THello {
		return errors.New("expected hello, got " + env.T)
	}
	var hello proto.Hello
	if err := json.Unmarshal(env.D, &hello); err != nil {
		return err
	}
	negotiated := negotiate(hello)
	if negotiated == 0 {
		if token := strings.TrimSpace(hello.NodeToken); token != "" {
			if n, err := s.hub.store.NodeByTokenHash(store.HashToken(token)); err == nil {
				_ = s.hub.store.SetNodeReject(n.UUID, "proto_mismatch")
			}
		}
		s.hub.recordReject(hello.NodeToken, "proto_mismatch", s.remote)
		_ = s.send(proto.TReject, 0, proto.Reject{Reason: "proto_mismatch", Detail: fmt.Sprintf("server supports %d-%d", proto.ProtoMin, proto.ProtoMax)})
		return &handshakeRejectError{reason: "proto_mismatch", err: errors.New("no compatible protocol version")}
	}
	s.proto = negotiated
	s.caps = hello.Caps
	s.rememberAssetDigests(hello.RuntimeAssetDigests)

	if strings.TrimSpace(hello.NodeToken) == "" {
		s.hub.recordReject(hello.NodeToken, "bad_token", s.remote)
		_ = s.send(proto.TReject, 0, proto.Reject{Reason: "bad_token", Detail: "node token is required"})
		return &handshakeRejectError{reason: "bad_token", err: errBadToken}
	}
	n, err := s.hub.authNodeToken(hello.NodeToken)
	if err != nil {
		s.hub.recordReject(hello.NodeToken, "bad_token", s.remote)
		_ = s.send(proto.TReject, 0, proto.Reject{Reason: "bad_token", Detail: "node token was not recognized"})
		prefix := strings.TrimSpace(hello.NodeToken)
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		return &handshakeRejectError{reason: "bad_token", token: prefix, err: err}
	}
	s.NodeUUID = n.UUID
	s.nodeID = n.ID
	registeredToken, ok := n.PlaintextToken()
	if !ok {
		registeredToken = strings.TrimSpace(hello.NodeToken)
	}
	if hello.Smoke {
		// A smoke handshake validates auth/transport without registering a
		// session, so it carries no SessionID and never opens a data lane.
		if err := s.send(proto.TRegistered, 0, proto.Registered{NodeUUID: n.UUID, NodeToken: registeredToken, Proto: negotiated}); err != nil {
			return err
		}
		if err := s.send(proto.THello, 0, s.hub.serverHello()); err != nil {
			return err
		}
		return errSmokeHandshake
	}
	// A diagnostic handshake must preserve an earlier upgrade failure. Only a
	// formal connection running this version can clear that observation.
	if strings.TrimSpace(hello.Version) == version.Version {
		s.hub.setUpgradeError(n.UUID, "")
	}
	s.upgradeSessionID = s.sessionID
	if s.upgradeSessionID == "" {
		var err error
		s.upgradeSessionID, err = newSessionID()
		if err != nil {
			return fmt.Errorf("create upgrade session identity: %w", err)
		}
	}
	s.upgradeVersion = strings.TrimSpace(hello.Version)
	// Register before announcing the identity: the agent may open a data lane the
	// instant it reads Registered, and that lane matches its DataHello.SessionID
	// against this session's id via the hub session table. Registering here (with
	// s.sessionID already set by acceptYamuxServer) closes the race. register also
	// marks the node online and replaces any prior session for this UUID.
	// Persist the handshake facts first so that the instant the node reads as
	// online (register below) they are already on record.
	s.recordHandshakeFacts(n, hello)
	s.hub.register(s)
	if err := s.send(proto.TRegistered, 0, proto.Registered{NodeUUID: n.UUID, NodeToken: registeredToken, Proto: negotiated, SessionID: s.sessionID}); err != nil {
		return err
	}

	// Server hello reply completes the handshake.
	if err := s.send(proto.THello, 0, s.hub.serverHello()); err != nil {
		return err
	}
	s.lastSeen.Store(time.Now().UnixMilli())
	s.upgradeHandshakeComplete.Store(true)
	return nil
}

// ctrlLoop processes inbound ctrl envelopes until the stream closes.
func (s *Session) ctrlLoop() {
	defer s.markClosed()
	for {
		var env proto.Envelope
		if err := s.dec.Decode(&env); err != nil {
			if !errors.Is(err, io.EOF) {
				s.hub.log.Debug("ctrl decode ended", "node", s.NodeUUID, "err", err)
			}
			return
		}
		s.handle(env)
	}
}

func (s *Session) handle(env proto.Envelope) {
	s.observeUpgradeControl(env)
	switch env.T {
	case proto.THeartbeat:
		s.handleHeartbeat()
	case proto.TStatusNode:
		s.handleStatusNode(env.D)
	case proto.TStatusImageNode:
		s.handleImageStatus(env.D)
	case proto.TStatusInstance:
		var st proto.InstanceStatus
		if err := json.Unmarshal(env.D, &st); err != nil {
			s.hub.log.Warn("bad status.instance", "node", s.NodeUUID, "err", err)
			return
		}
		accepted, err := s.hub.applyInstanceStatus(s, st)
		if err != nil {
			s.hub.log.Debug("apply status.instance", "uuid", st.UUID, "err", err)
		}
		if accepted {
			s.rememberAssetDigests(st.RuntimeAssetDigests)
		}
	case proto.TPeerTransferPhase:
		var phase proto.PeerTransferPhase
		if json.Unmarshal(env.D, &phase) == nil && s.hub.recordPeerTransferPhase(s, phase) {
			s.hub.peerTransferEvent(phase.TransferID, "source_phase", "")
		}
	case proto.TStatusModelFile:
		var st proto.ModelFileStatus
		if err := json.Unmarshal(env.D, &st); err != nil {
			return
		}
		_ = s.hub.applyModelFileStatus(s, st)
	case proto.TStatusStorage:
		var report proto.StorageStatusReport
		if err := json.Unmarshal(env.D, &report); err != nil {
			s.hub.log.Warn("bad status.storage", "node", s.NodeUUID, "err", err)
			return
		}
		node, err := s.hub.store.NodeByUUID(s.NodeUUID)
		if err == nil {
			if err := s.hub.store.ApplyStorageStatus(node.ID, report); err != nil {
				s.hub.log.Warn("apply status.storage failed", "node", s.NodeUUID, "err", err)
			}
		}
	case proto.TStatusBenchmark:
		var st proto.BenchmarkStatus
		if err := json.Unmarshal(env.D, &st); err != nil {
			s.hub.log.Warn("bad status.benchmark", "node", s.NodeUUID, "err", err)
			return
		}
		node, err := s.hub.store.NodeByUUID(s.NodeUUID)
		if err != nil {
			return
		}
		if err := s.hub.store.ApplyBenchmarkStatus(node.ID, st); err != nil {
			s.hub.log.Debug("apply status.benchmark", "id", st.ID, "err", err)
			return
		}
		if len(st.Sample) > 0 && st.PointIndex > 0 {
			var sample metrics.ProcessSample
			if json.Unmarshal(st.Sample, &sample) == nil {
				_ = s.hub.store.AppendBenchmarkCurveSample(st.ID, st.PointIndex, time.Now().UTC(), sample)
			}
		}
		if (len(st.DetailChunk) > 0 || st.DetailFinal) && s.hub.onBenchmarkDetail != nil {
			s.hub.onBenchmarkDetail(s.NodeUUID, st)
		}
		if len(st.Sample) > 0 && s.hub.onMetrics != nil {
			var sample metrics.ProcessSample
			if json.Unmarshal(st.Sample, &sample) == nil {
				now := time.Now().UnixMilli()
				labels := map[string]string{"benchmark": fmt.Sprint(st.ID), "point": fmt.Sprint(st.PointIndex), "node": s.NodeUUID}
				s.hub.onMetrics(s.NodeUUID, []proto.MetricSample{{Metric: "gpuworks_bench_output_tput", Labels: labels, Value: sample.OutputTput, TimeMS: now}, {Metric: "gpuworks_bench_total_tput", Labels: labels, Value: sample.TotalTput, TimeMS: now}, {Metric: "gpuworks_bench_request_tput", Labels: labels, Value: sample.RequestTput, TimeMS: now}, {Metric: "gpuworks_bench_ttft_p50_ms", Labels: labels, Value: sample.TTFTP50, TimeMS: now}, {Metric: "gpuworks_bench_tpot_p50_ms", Labels: labels, Value: sample.TPOTP50, TimeMS: now}, {Metric: "gpuworks_bench_latency_p99_ms", Labels: labels, Value: sample.LatencyP99, TimeMS: now}})
			}
		}
		if st.State == "done" || st.State == "failed" || st.State == "canceled" {
			_ = s.PushUpdate(proto.StateUpdate{RemovedBenchmarks: []uint{st.ID}})
		}
	case proto.TMetrics:
		if s.hub.onMetrics != nil {
			var b proto.MetricBatch
			if json.Unmarshal(env.D, &b) == nil && len(b.Samples) > 0 {
				s.hub.onMetrics(s.NodeUUID, b.Samples)
			}
		}
	case proto.TLogs:
		if s.hub.onLogs != nil {
			var b proto.LogBatch
			if json.Unmarshal(env.D, &b) == nil && len(b.Lines) > 0 {
				s.hub.onLogs(s.NodeUUID, b.Lines)
			}
		}
	case proto.TAgentUpgradeStatus:
		var status proto.AgentUpgradeStatus
		if err := json.Unmarshal(env.D, &status); err != nil {
			s.hub.log.Warn("bad agent upgrade status", "node", s.NodeUUID, "err", err)
			return
		}
		if status.State == "failed" {
			s.hub.setUpgradeError(s.NodeUUID, status.Error)
			s.observeUpgradeFailure(status.Error)
			s.hub.log.Error("agent self-upgrade failed", "node", s.NodeUUID, "err", status.Error)
		} else {
			s.hub.log.Warn("unknown agent upgrade status", "node", s.NodeUUID, "state", status.State, "err", status.Error)
		}
	case proto.TRPCResp:
		s.dispatchRPCResp(env)
	default:
		s.hub.log.Debug("unhandled ctrl msg", "t", env.T, "node", s.NodeUUID)
	}
}

// send writes one envelope as a JSON line on the ctrl stream (mutex-guarded).
func (s *Session) send(t string, id uint64, d any) error {
	if s.sendFn != nil { // test seam: observe envelopes without a transport
		return s.sendFn(t, id, d)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	env := proto.Envelope{T: t, ID: id, D: raw}
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return s.writeCtrl(line)
}

// openStream opens a new yamux stream and writes its one-line JSON header. ctx
// bounds both an in-flight wait for a detached data lane to reattach (see
// dataLaneRoute) and the OpenStream call itself.
//
// Ctrl-plane kinds always use s.ysess. Data-plane kinds use the attached data
// lane, or s.ysess when no data lane was ever attached (legacy/single-lane and
// older server_dial peers keep one physical connection). A detached data lane is never
// silently replaced by ctrl: see dataLaneRoute and docs/adr for why the two
// planes must not share a connection.
func (s *Session) openStream(ctx context.Context, hdr proto.StreamHeader) (net.Conn, error) {
	ysess, usedData, err := s.dataLaneRoute(ctx, hdr.Kind)
	if err != nil {
		return nil, err
	}
	conn, dead, err := writeStreamHeaderFn(ctx, ysess, hdr)
	if err == nil {
		if usedData {
			return &laneStream{Conn: conn, sess: s, ysess: ysess}, nil
		}
		return conn, nil
	}
	if !usedData || !dead {
		// Either this rode ctrl (dead is meaningless there: detaching the
		// data lane over a ctrl-stream failure would be a bug in itself), or
		// the failure does not confirm the data session died — a canceled/
		// expired ctx, a resource-scope limit, stream-id exhaustion, and so
		// on all leave the lane perfectly healthy. Detaching (and closing;
		// see detachDataIfCurrent) a healthy lane over one of these would
		// turn a single canceled request into node-wide data-plane
		// unavailability until the lane happens to drop for a real reason —
		// exactly the failure mode this function exists to avoid. Return the
		// error as-is; the caller decides whether to retry.
		return nil, err
	}
	// Confirmed dead: either OpenStream failed with the session already
	// shut down, or a stream opened fine but writing its header failed —
	// yamux's send path is fire-and-forget (a write can "succeed" locally
	// and still be doomed), so a failure surfacing there is itself the only
	// signal available and is treated as death unconditionally. Detach (and
	// close) it now instead of waiting for CloseChan, which may lag well
	// behind reality, then retry once against whatever reattaches within the
	// same wait budget — never by falling back onto ctrl.
	s.detachDataIfCurrent(ysess)
	retry, waitErr := s.awaitDataLane(ctx)
	if waitErr != nil {
		return nil, waitErr
	}
	conn, dead, err = writeStreamHeaderFn(ctx, retry, hdr)
	if err == nil {
		return &laneStream{Conn: conn, sess: s, ysess: retry}, nil
	}
	if !dead {
		// Same reasoning as above: a failure that leaves the lane healthy
		// (canceled ctx, resource scope, stream-id exhaustion) is the
		// caller's own condition and must not be reported as the lane being
		// unavailable.
		return nil, err
	}
	// The lane that had just reattached died on this write too. The condition
	// is still "this node's data lane was not usable", which is the typed
	// retryable answer the dual-lane contract promises (ADR 0024): without
	// this, the raw transport error reaches the proxy, which can only read it
	// as a dead upstream and answers 502 instead of a retryable 503 — the
	// only exit from this function that left the condition unclassified.
	s.detachDataIfCurrent(retry)
	return nil, fmt.Errorf("%w: node %s data lane died again on the write that followed its reattach: %v", ErrDataLaneUnavailable, s.NodeUUID, err)
}

// writeStreamHeaderFn is writeStreamHeader behind a seam, so a test can drive
// openStream's reattach-then-die path deterministically; forcing a yamux
// session to look healthy at the route check and dead at the write is a race
// no test can stage reliably.
var writeStreamHeaderFn = writeStreamHeader

// writeStreamHeader opens one yamux stream on ysess and writes hdr as its
// one-line JSON header.
//
// dead reports whether a failure confirms the session itself is gone, as
// opposed to a failure that leaves it perfectly healthy:
//
//   - OpenStream failing while ysess.IsClosed() is now true. yamux sets its
//     internal "shutdown" flag and closes shutdownCh before an error caused
//     by that shutdown can ever be returned to a caller (see go-yamux v5's
//     Session.close/OpenStream), so checking IsClosed() immediately after is
//     race-free and — because shutdown is monotonic — cannot be stale in the
//     "actually dead but reports healthy" direction. This covers an already-
//     closed session, a shutdown observed mid-open, and ErrSessionShutdown.
//     It deliberately does NOT cover a canceled/expired ctx (ctx.Err(), or
//     yamux's own ErrTimeout when the same cancellation is observed a frame
//     later inside the send path), a resource-scope error from the p2p
//     resource manager, or stream-ID exhaustion — none of those mean the
//     session is unhealthy, so IsClosed() correctly reports false for them.
//   - The header write failing after OpenStream returned a live stream. The
//     write path is fire-and-forget (it can return success once the frame is
//     merely queued, before any byte reaches the wire), so a write error is
//     the only signal this path ever gets and is always treated as death,
//     independent of IsClosed() at that instant.
func writeStreamHeader(ctx context.Context, ysess *yamux.Session, hdr proto.StreamHeader) (conn net.Conn, dead bool, err error) {
	stream, err := ysess.OpenStream(ctx)
	if err != nil {
		return nil, ysess.IsClosed(), err
	}
	line, err := json.Marshal(hdr)
	if err != nil {
		_ = stream.Close()
		return nil, false, err
	}
	line = append(line, '\n')
	if _, err := stream.Write(line); err != nil {
		_ = stream.Close()
		return nil, true, err
	}
	return stream, false, nil
}

// flushCtrlBeforeClose keeps the yamux send loop running long enough to flush a
// handshake reply already written to the ctrl stream (a reject envelope, or the
// registered+hello of a smoke handshake) before the caller tears the session
// down. Unlike hashicorp/yamux, go-yamux's Session.Close closes the socket
// immediately without draining queued frames, so without this the peer observes
// a stream reset instead of the reply. Half-closing our write side sends a FIN
// in order after the queued bytes; the bounded read then blocks (keeping the
// send loop alive to flush) until the peer reads the reply and hangs up.
func (s *Session) flushCtrlBeforeClose() {
	if cw, ok := s.ctrl.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	if nc, ok := s.ctrl.(net.Conn); ok {
		_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	}
	buf := make([]byte, 256)
	for {
		if _, err := s.ctrl.Read(buf); err != nil {
			return
		}
	}
}

// Close tears down the session's yamux connection. The data lane is a child of
// the ctrl session, so it is closed here too.
func (s *Session) Close() {
	s.markClosed()
	s.dataMu.Lock()
	data := s.data
	s.data = nil
	s.dataMu.Unlock()
	if data != nil {
		_ = data.Close()
	}
	_ = s.ctrl.Close()
	if s.ysess != nil {
		_ = s.ysess.Close()
	}
}

func (s *Session) markClosed() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// LastSeen returns the last activity time (unix ms).
func (s *Session) LastSeen() int64 { return s.lastSeen.Load() }

// CtrlDataPlaneStreamCount returns how many data-plane-kind streams this
// session has ever opened on ctrl (see the ctrlDataPlaneStreams field
// comment). Tests use it to assert the no-ctrl-fallback property directly
// instead of inferring it from timing.
func (s *Session) CtrlDataPlaneStreamCount() int64 { return s.ctrlDataPlaneStreams.Load() }

// negotiate picks the highest protocol version both ends support (0 = none).
func negotiate(h proto.Hello) int {
	hi := min(h.ProtoMax, proto.ProtoMax)
	lo := max(h.ProtoMin, proto.ProtoMin)
	if lo > hi {
		return 0
	}
	return hi
}

// readStreamHeader reads the one-line JSON stream header prefixing every stream.
func readStreamHeader(r io.Reader) (proto.StreamHeader, error) {
	// Read a single line without consuming past it: the ctrl stream then carries
	// JSON Lines, so we use a byte-at-a-time read until '\n'. Headers are tiny.
	var hdr proto.StreamHeader
	line, err := readLine(r)
	if err != nil {
		return hdr, err
	}
	err = json.Unmarshal(line, &hdr)
	return hdr, err
}

func readLine(r io.Reader) ([]byte, error) {
	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, one[0])
			if len(buf) > 64*1024 {
				return nil, errors.New("stream header too long")
			}
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
	}
}
