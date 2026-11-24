package conn

import (
	"context"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// handleTunnel bridges a data-plane stream to the engine's local socket.
func (c *Conn) handleTunnel(hdr proto.StreamHeader, stream *yamux.Stream) {
	defer stream.Close()
	log := c.log
	captured := hdr.Trace != nil && log.Enabled(context.Background(), slog.LevelDebug)
	started := time.Now()
	if captured {
		log = log.With("request_id", hdr.Trace.RequestID, "attempt", hdr.Trace.Attempt, "stream_id", stream.StreamID())
		log.Debug("tunnel dialing", "dst", hdr.Dst)
	}
	local, err := net.DialTimeout("tcp", hdr.Dst, 5*time.Second)
	if err != nil {
		log.Debug("tunnel dial failed", "dst", hdr.Dst, "duration", time.Since(started), "err", err)
		return
	}
	defer local.Close()
	if captured {
		log = log.With("dst", hdr.Dst, "local_addr", local.LocalAddr().String())
		log.Debug("tunnel connected", "duration", time.Since(started))
	}
	forward := func(dst io.Writer, src io.Reader, readSide, writeSide, onEnd string) {
		if !captured {
			_, _ = io.Copy(dst, src)
			return
		}
		copyTunnelDebug(log, dst, src, readSide, writeSide, onEnd)
	}
	// A completed upload only half-closes the engine socket; the reply can
	// arrive much later. Completion of the reply direction closes the tunnel.
	go func() {
		forward(local, stream, "tunnel", "engine", "half_close_engine_write")
		if half, ok := local.(interface{ CloseWrite() error }); ok {
			if err := half.CloseWrite(); err != nil && captured {
				log.Debug("tunnel half-close failed", "side", "engine", "err", err)
			}
		}
	}()
	forward(stream, local, "engine", "tunnel", "close_tunnel")
}

// io.Copy normalizes a clean read EOF to nil. Observe destination failures
// separately so the terminal operation and the original error are both kept.
func copyTunnelDebug(log *slog.Logger, dst io.Writer, src io.Reader, readSide, writeSide, onEnd string) {
	writer := tunnelWriter{Writer: dst}
	n, err := io.Copy(&writer, struct{ io.Reader }{src})
	side, op := readSide, "read"
	if writer.failed {
		side, op = writeSide, "write"
	}
	if err == nil {
		err = io.EOF
	}
	log.Debug("tunnel copy ended", "side", side, "operation", op, "bytes", n, "err", err, "on_end", onEnd)
}

type tunnelWriter struct {
	io.Writer
	failed bool
}

func (w *tunnelWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.failed = err != nil || n != len(p)
	return n, err
}
