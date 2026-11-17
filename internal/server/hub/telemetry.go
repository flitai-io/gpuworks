package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// telemetryHeaderWait bounds how long an agent-opened data-lane stream may
// take to name its kind. The header is one short line written right after
// the open, so anything slower is a broken peer. telemetryCloseGrace is how
// long a batch already read keeps waiting for ingest room after the node's
// session ends. telemetryMaxLine bounds one envelope line, far above a real
// batch (instance and agent log batches are at most 64 lines of at most 1 MiB
// each).
const telemetryHeaderWait = 10 * time.Second

// Atomic so tests can shorten them while streams are being served.
var (
	telemetryCloseGrace atomic.Int64 // time.Duration
	telemetryMaxLine    atomic.Int64
)

func init() {
	telemetryCloseGrace.Store(int64(10 * time.Second))
	telemetryMaxLine.Store(128 << 20)
}

var errTelemetryLineTooLong = errors.New("telemetry line exceeds the size bound")

// SetLogIngestWait installs the log sink the telemetry stream uses: it waits
// for ingest room instead of dropping. Without one, telemetry logs go through
// the dropping onLogs sink like ctrl's.
func (h *Hub) SetLogIngestWait(wait func(ctx context.Context, nodeUUID string, lines []proto.LogLine) error) {
	h.onLogsWait = wait
}

// acceptDataStreams serves the streams the agent opens on a data lane until
// the lane closes. The only agent-opened kind is StreamTelemetry; the server
// advertises it with CapTelemetryStream, so an agent never opens one on a
// server that would leave it unread.
//
// Streams of one batch type are read one after another in the order they
// were accepted: the agent opens a new one only after the last one failed,
// and the old one's buffered lines come first. The order is fixed here, in
// the accept loop, not by which reader goroutine gets going first.
func (s *Session) acceptDataStreams(data *yamux.Session) {
	for {
		stream, err := data.AcceptStream()
		if err != nil {
			return
		}
		hdr, ok := s.telemetryHeader(stream)
		if !ok {
			_ = stream.Reset()
			continue
		}
		s.telMu.Lock()
		if s.telTail == nil {
			s.telTail = map[string]chan struct{}{}
		}
		prev, done := s.telTail[hdr.Ref], make(chan struct{})
		s.telTail[hdr.Ref] = done
		s.telMu.Unlock()
		go func() {
			defer close(done)
			defer stream.Close()
			if prev != nil {
				<-prev
			}
			s.serveTelemetry(stream)
		}()
	}
}

// telemetryHeader reads an agent-opened stream's header. The agent writes it
// right after the open, so a stream that has not within telemetryHeaderWait
// is reset rather than left holding up the accept loop.
func (s *Session) telemetryHeader(stream *yamux.Stream) (proto.StreamHeader, bool) {
	stall := time.AfterFunc(telemetryHeaderWait, func() { _ = stream.Reset() })
	hdr, err := readStreamHeader(stream)
	if !stall.Stop() || err != nil || hdr.Kind != proto.StreamTelemetry || (hdr.Ref != proto.TMetrics && hdr.Ref != proto.TLogs) {
		s.hub.log.Debug("refuse agent-opened data stream", "node", s.NodeUUID, "kind", hdr.Kind, "ref", hdr.Ref, "err", err)
		return hdr, false
	}
	return hdr, true
}

// serveTelemetry reads metric and log envelopes until the stream ends. Only
// newline-terminated lines count: after a failed write the agent closes the
// stream and sends the batch again, so the unterminated line that write left
// must not be taken as delivered. Any other envelope type is ignored:
// control messages belong on ctrl, where their ordering and the session's
// upgrade observation live. When it stops reading before the agent's end of
// the stream (a bad line, an ingest wait given up), it resets the stream, so
// the agent's next write fails and that batch is sent again instead of
// landing unread in the receive window.
func (s *Session) serveTelemetry(stream *yamux.Stream) {
	r := io.Reader(stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.closed:
		case <-ctx.Done():
			return
		}
		select {
		case <-time.After(time.Duration(telemetryCloseGrace.Load())):
			cancel()
		case <-ctx.Done():
		}
	}()
	br := bufio.NewReader(r)
	warned := false
	for {
		line, err := readTelemetryLine(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.hub.log.Debug("telemetry stream ended", "node", s.NodeUUID, "err", err)
				_ = stream.Reset()
			}
			return
		}
		var env proto.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			s.hub.log.Debug("telemetry stream sent a malformed line", "node", s.NodeUUID, "err", err)
			_ = stream.Reset()
			return
		}
		switch env.T {
		case proto.TMetrics:
			s.handleMetrics(env.D)
		case proto.TLogs:
			if s.hub.onLogsWait == nil {
				s.handleLogs(env.D)
				continue
			}
			var b proto.LogBatch
			if json.Unmarshal(env.D, &b) != nil || len(b.Lines) == 0 {
				continue
			}
			// This stream is the agent's alone, so waiting for ingest room
			// costs nobody else: the wait reaches the agent as flow control
			// and its log buffer holds the lines, where ctrl would drop them.
			if err := s.hub.onLogsWait(ctx, s.NodeUUID, b.Lines); err != nil {
				_ = stream.Reset()
				return
			}
		default:
			if !warned {
				warned = true
				s.hub.log.Warn("ignore non-telemetry envelope on the telemetry stream", "node", s.NodeUUID, "t", env.T)
			}
		}
	}
}

func (s *Session) handleMetrics(raw json.RawMessage) {
	if s.hub.onMetrics == nil {
		return
	}
	var b proto.MetricBatch
	if json.Unmarshal(raw, &b) == nil && len(b.Samples) > 0 {
		s.hub.onMetrics(s.NodeUUID, b.Samples)
	}
}

func (s *Session) handleLogs(raw json.RawMessage) {
	if s.hub.onLogs == nil {
		return
	}
	var b proto.LogBatch
	if json.Unmarshal(raw, &b) == nil && len(b.Lines) > 0 {
		s.hub.onLogs(s.NodeUUID, b.Lines)
	}
}

// readTelemetryLine returns the next newline-terminated line, or an error for
// a line that ends the stream unterminated or exceeds telemetryMaxLine.
func readTelemetryLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if int64(len(line)+len(chunk)) > telemetryMaxLine.Load() {
			return nil, errTelemetryLineTooLong
		}
		line = append(line, chunk...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}
