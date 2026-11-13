package terminal

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
)

// streamWriter serializes frame writes. The output pump, the initial info frame
// and the final exit frame all write to the same stream from different
// goroutines, and interleaved frames would corrupt the protocol.
type streamWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *streamWriter) frame(typ byte, payload []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return proto.WriteTermFrame(sw.w, typ, payload)
}

func (sw *streamWriter) data(p []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return proto.WriteTermData(sw.w, p)
}

// Serve opens a session per spec and bridges it to rw, which carries terminal
// frames in both directions. It returns when the shell exits or the peer hangs
// up, having reaped the session's process group either way.
//
// The stream is not closed here; the caller owns it.
func Serve(rw io.ReadWriter, spec Spec) error {
	slog.Info("terminal session starting", "rows", spec.Rows, "cols", spec.Cols)
	sw := &streamWriter{w: rw}
	var sess *Session
	var cleanupErr error
	var cleanupOnce sync.Once
	cleanup := func() error {
		cleanupOnce.Do(func() {
			// Keep the attached runtime client alive while its owner snapshots and
			// reaps remote descendants. Closing it first can orphan setsid jobs
			// before their original parent proves they belong to this terminal.
			if spec.Cleanup != nil {
				cleanupErr = spec.Cleanup()
			}
			if sess != nil {
				cleanupErr = errors.Join(cleanupErr, sess.Close())
			}
		})
		return cleanupErr
	}
	// Owned-session paths report cleanup errors explicitly; this defer is a fallback.
	defer func() { _ = cleanup() }()
	sess, err := Open(spec)
	if err != nil {
		slog.Warn("terminal session open failed", "err", err)
		if spec.Cleanup != nil {
			err = errors.Join(err, cleanup())
			return errors.Join(err, sw.frame(proto.TermExit, proto.EncodeExit(-1, err.Error())))
		}
		return err
	}

	if info, err := json.Marshal(sess.Info()); err == nil {
		// Best-effort: a failed info frame is not worth refusing the session
		// over, the operator just does not see the uid banner.
		_ = sw.frame(proto.TermInfo, info)
	}

	// Output pump. Ends when the pty reports EOF, which happens once the shell
	// has exited and the last slave fd is gone.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, readErr := sess.Read(buf)
			if n > 0 {
				if err := sw.data(buf[:n]); err != nil {
					return // peer is gone; the deferred Close tears down the pty
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// Input pump. It must not run on this goroutine: a session usually ends
	// because the shell exited, and pumpInput would still be blocked reading a
	// stream the peer has not closed, so Serve would never report the exit.
	inDone := make(chan error, 1)
	go func() { inDone <- pumpInput(rw, sess) }()

	select {
	case <-outDone:
		// An owned runtime session can outlive the local CLI. Its close result
		// must be known before the server consumes the single terminal outcome.
		if spec.Cleanup != nil {
			if err := cleanup(); err != nil {
				return errors.Join(err, sw.frame(proto.TermExit, proto.EncodeExit(-1, err.Error())))
			}
			return sw.frame(proto.TermExit, proto.EncodeExit(sess.ExitCode(), ""))
		}
		// The pty reported EOF, so the shell is gone. Report its status; the
		// input goroutine stays blocked until the caller closes the stream,
		// which it does as soon as Serve returns.
		_ = sw.frame(proto.TermExit, proto.EncodeExit(sess.ExitCode(), ""))
	case <-inDone:
		// The peer hung up (closed browser tab) or the stream broke. Tear the
		// pty down, which unblocks the output pump. No exit frame: there is no
		// longer anyone to read it.
		err := cleanup()
		<-outDone
		if spec.Cleanup != nil && err != nil {
			return err
		}
	}
	slog.Info("terminal session ended", "exit_code", sess.ExitCode())
	return nil
}

// pumpInput copies peer frames into the pty until the stream ends.
func pumpInput(r io.Reader, sess *Session) error {
	fr := proto.NewTermFrameReader(r)
	for {
		f, err := fr.Next()
		if err != nil {
			return err
		}
		switch f.Type {
		case proto.TermData:
			if _, err := sess.Write(f.Payload); err != nil {
				return err
			}
		case proto.TermResize:
			rows, cols, err := proto.DecodeResize(f.Payload)
			if err != nil {
				continue // malformed resize is not worth ending the session over
			}
			_ = sess.Resize(rows, cols)
		default:
			// Ignore unknown frame types so a newer server can add them without
			// breaking older agents.
		}
	}
}
