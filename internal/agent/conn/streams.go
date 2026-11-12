package conn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/terminal"
	"gpuworks.ai/gpuworks/internal/proto"
)

// acceptStreams handles server-initiated yamux streams. The agent opened the
// ctrl stream; every other stream here is opened by the server (tunnel/logtail).
func (c *Conn) acceptStreams(ctx context.Context) {
	for {
		stream, err := c.ysess.AcceptStream()
		if err != nil {
			return // session closed
		}
		go c.handleStream(ctx, stream)
	}
}

func (c *Conn) handleStream(ctx context.Context, stream *yamux.Stream) {
	hdr, err := readStreamHeader(stream)
	if err != nil {
		_ = stream.Close()
		return
	}
	if err := c.creds.Simulated.Stream(hdr); err != nil {
		c.log.Warn("stream refused on a simulated node", "kind", hdr.Kind, "err", err)
		if hdr.Kind == proto.StreamTerminal {
			termFail(stream, err.Error())
		}
		_ = stream.Close()
		return
	}
	switch hdr.Kind {
	case proto.StreamPeerRelaySend:
		c.handlePeerRelaySend(ctx, stream)
	case proto.StreamPeerRelayReceive:
		c.handlePeerRelayReceive(ctx, hdr, stream)
	case proto.StreamTunnel:
		c.handleTunnel(hdr.Dst, stream)
	case proto.StreamLogtail:
		c.handleLogtail(ctx, hdr.Ref, stream)
	case proto.StreamTerminal:
		c.handleTerminal(hdr, stream)
	case proto.StreamModelRead:
		c.handleModelRead(ctx, hdr.Dst, stream)
	case proto.StreamModelWrite:
		c.handleModelWrite(ctx, hdr.Dst, stream)
	case proto.StreamImageRead:
		c.handleImageRead(ctx, hdr.Dst, hdr.Ref, stream)
	case proto.StreamImageWrite:
		c.handleImageWrite(ctx, hdr.Dst, hdr.Ref, stream)
	case proto.StreamLargeFileRead:
		c.handleLargeFileRead(ctx, hdr.Dst, stream)
	case proto.StreamDiagnosticsRead:
		c.handleDiagnosticsRead(ctx, hdr.Ref, stream)
	case proto.StreamAgentUpgrade:
		c.handleUpgradePush(hdr, stream)
	case proto.StreamStorageUpload:
		c.handleStorageUpload(ctx, hdr, stream)
	case proto.StreamStorageDownload:
		c.handleStorageDownload(ctx, hdr, stream)
	default:
		_ = stream.Close()
	}
}

// handleTunnel bridges a data-plane stream to a local address (the inference
// engine's 127.0.0.1:port).
func (c *Conn) handleTunnel(dst string, stream *yamux.Stream) {
	defer stream.Close()
	local, err := net.DialTimeout("tcp", dst, 5*time.Second)
	if err != nil {
		c.log.Debug("tunnel dial failed", "dst", dst, "err", err)
		return
	}
	defer local.Close()
	done := make(chan struct{})
	// The request body commonly reaches EOF long before a non-streaming
	// inference response. Do not close the tunnel when that direction finishes;
	// only the local response EOF terminates the stream.
	go func() {
		_, _ = io.Copy(local, stream)
		if half, ok := local.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	}()
	go func() {
		_, _ = io.Copy(stream, local)
		close(done)
	}()
	<-done
}

// handleLogtail streams a container's logs into the stream.
func (c *Conn) handleLogtail(ctx context.Context, ref string, stream *yamux.Stream) {
	defer stream.Close()
	if c.handlers == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Close cancels the follow when the server hangs up.
	go func() {
		buf := make([]byte, 1)
		_, _ = stream.Read(buf) // server side is write-only; read unblocks on close
		cancel()
	}()
	_ = c.handlers.StreamLogs(ctx, ref, true, stream)
}

// handleTerminal runs an interactive pty session. Session policy (idle timeout,
// concurrency, audit) lives on the server; the agent only honours the grace
// period it is told to use when reaping the session.
func (c *Conn) handleTerminal(hdr proto.StreamHeader, stream *yamux.Stream) {
	defer stream.Close()

	if hdr.Target == proto.TermTargetContainer {
		h, ok := c.handlers.(containerTerminalHandler)
		if !ok {
			termFail(stream, "this agent does not support container terminals")
			return
		}
		prepareCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var argv []string
		var closeSession func(context.Context) error
		var err error
		if owned, ok := c.handlers.(containerTerminalSessionHandler); ok {
			argv, closeSession, err = owned.ContainerTerminalSession(prepareCtx, hdr.Ref, nil)
		} else {
			argv, err = h.ContainerTerminalArgv(prepareCtx, hdr.Ref, nil)
		}
		cancel()
		if err != nil {
			termFail(stream, err.Error())
			return
		}
		spec := terminal.Spec{
			Rows: hdr.Rows, Cols: hdr.Cols, Grace: time.Duration(hdr.GraceSec) * time.Second, Argv: argv,
			Dir: "/",
		}
		if closeSession != nil {
			spec.Cleanup = func() error {
				closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				return closeSession(closeCtx)
			}
		}
		err = terminal.Serve(stream, spec)

		if err != nil {
			c.log.Warn("container terminal session failed", "err", err)
			// Serve owns the unique outcome when cleanup is installed, including
			// Open failure. A second frame could contradict the first outcome.
			if closeSession == nil {
				termFail(stream, err.Error())
			}
		}
		return
	}
	err := terminal.Serve(stream, terminal.Spec{
		Rows:  hdr.Rows,
		Cols:  hdr.Cols,
		Grace: time.Duration(hdr.GraceSec) * time.Second,
	})
	if err != nil {
		c.log.Warn("terminal session failed", "err", err)
		termFail(stream, err.Error())
	}
}

// termFail reports a setup failure as an exit frame, so the operator sees the
// reason instead of a terminal that connects and immediately goes blank.
func termFail(w io.Writer, msg string) {
	_ = proto.WriteTermFrame(w, proto.TermExit, proto.EncodeExit(-1, msg))
}

func (c *Conn) handleModelRead(ctx context.Context, path string, stream *yamux.Stream) {
	defer stream.Close()
	if c.handlers == nil {
		return
	}
	_ = c.handlers.StreamModelArchive(ctx, path, stream)
}

func (c *Conn) handleModelWrite(ctx context.Context, path string, stream *yamux.Stream) {
	defer stream.Close()
	if c.handlers == nil {
		return
	}
	_ = c.handlers.ReceiveModelArchive(ctx, path, stream)
}

func (c *Conn) handleImageRead(ctx context.Context, imageRef, transferID string, stream *yamux.Stream) {
	defer stream.Close()
	if c.handlers == nil {
		return
	}
	if err := c.handlers.StreamImage(ctx, imageRef, transferID, stream, false); err != nil {
		c.log.Warn("image save stream failed", "image", imageRef, "transfer", transferID, "err", err)
	}
}

func (c *Conn) handleImageWrite(ctx context.Context, imageRef, transferID string, stream *yamux.Stream) {
	defer stream.Close()
	if c.handlers == nil {
		return
	}
	if err := c.handlers.StreamImage(ctx, imageRef, transferID, stream, true); err != nil {
		c.log.Warn("image load stream failed", "image", imageRef, "transfer", transferID, "err", err)
	}
}

// handleLargeFileRead streams one bounded regular file to the server. A failure
// (a path that is not absolute, a directory, a file over the limit or a plain
// read error) closes the stream without a byte, which is how the server reads
// it: a fetch that cannot be completed is a fetch that did not happen, and the
// caller falls back rather than failing the operation it was serving.
func (c *Conn) handleLargeFileRead(ctx context.Context, path string, stream *yamux.Stream) {
	defer stream.Close()
	if c.handlers == nil {
		return
	}
	if err := c.handlers.StreamLargeFile(ctx, path, stream); err != nil {
		c.log.Warn("large file read failed", "path", path, "err", err)
	}
}

// handleDiagnosticsRead streams the node's part of a diagnostics collection.
// A handler without it (or a failure) closes the stream, which the server
// records as not collected.
func (c *Conn) handleDiagnosticsRead(ctx context.Context, sessions string, stream *yamux.Stream) {
	defer stream.Close()
	h, ok := c.handlers.(interface {
		StreamDiagnostics(ctx context.Context, sessions []string, w io.Writer) error
	})
	if !ok {
		return
	}
	var ids []string
	for _, id := range strings.Split(sessions, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if err := h.StreamDiagnostics(ctx, ids, stream); err != nil {
		c.log.Warn("diagnostics read failed", "err", err)
	}
}

// handleStorageUpload receives one upload into a storage. The handler answers
// on the stream; closing it afterwards tells the server nothing more follows.
// If the server goes away mid-body the read fails and the handler drops the
// staging, so a broken upload never changes the target.
func (c *Conn) handleStorageUpload(ctx context.Context, hdr proto.StreamHeader, stream *yamux.Stream) {
	defer stream.Close()
	if c.handlers == nil {
		return
	}
	if err := c.handlers.ReceiveStorageUpload(ctx, hdr, stream); err != nil {
		c.log.Warn("storage upload failed", "storage", hdr.StorageID, "path", hdr.Rel, "err", err)
	}
}

// handleStorageDownload sends one file or packed directory from a storage. A
// refusal (a StorageFilesError: the head carries it and nothing followed) is
// closed normally so the server reads the head. A failure after the head
// resets the stream instead, so the server's read fails rather than ending
// cleanly: a cut-off directory archive has no length to be checked against.
func (c *Conn) handleStorageDownload(ctx context.Context, hdr proto.StreamHeader, stream *yamux.Stream) {
	if c.handlers == nil {
		_ = stream.Close()
		return
	}
	err := c.handlers.SendStorageDownload(ctx, hdr, stream)
	var refusal *proto.StorageFilesError
	if err != nil && !errors.As(err, &refusal) {
		c.log.Warn("storage download failed", "storage", hdr.StorageID, "path", hdr.Rel, "err", err)
		_ = stream.Reset()
		return
	}
	_ = stream.Close()
}

// readStreamHeader reads the one-line JSON header prefixing every stream.
func readStreamHeader(r io.Reader) (proto.StreamHeader, error) {
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
				return nil, io.ErrShortBuffer
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
