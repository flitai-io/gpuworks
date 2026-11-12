package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

type peerRelaySender interface {
	StreamPeerSend(context.Context, string, json.RawMessage, net.Conn) (any, error)
}
type peerRelayReceiver interface {
	PeerRelayReceiver(context.Context, string, string) (net.Conn, error)
}

func (c *Conn) handlePeerRelaySend(ctx context.Context, stream *yamux.Stream) {
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(stream)
	if err != nil {
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	var env proto.Envelope
	if json.Unmarshal(line, &env) != nil || env.T != proto.TRPCReq || env.ID == 0 {
		return
	}
	var req rpcReqBody
	var result any
	err = json.Unmarshal(env.D, &req)
	if err == nil {
		switch req.Method {
		case proto.RPCModelSyncSend, proto.RPCImageSyncSend, proto.RPCModelSyncProbe, proto.RPCImageSyncProbe:
			err = c.creds.Simulated.RPC(req.Method, req.Params)
		default:
			err = errors.New("unsupported peer relay method")
		}
	}
	if err == nil {
		if handler, ok := c.handlers.(peerRelaySender); ok {
			sendCtx, cancel := context.WithCancel(ctx)
			raw := newPeerRelaySourceConn(stream, cancel)
			stop := context.AfterFunc(sendCtx, func() { stream.Close() })
			result, err = handler.StreamPeerSend(sendCtx, req.Method, req.Params, raw)
			raw.Close()
			stop()
			cancel()
		} else {
			err = errors.New("peer relay sender unavailable")
		}
	}
	body := rpcRespBody{Result: result}
	if err != nil {
		body.Error = err.Error()
	}
	_ = c.send(proto.TRPCResp, env.ID, body)
}

func (c *Conn) handlePeerRelayReceive(ctx context.Context, hdr proto.StreamHeader, stream *yamux.Stream) {
	defer stream.Close()
	setupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var local net.Conn
	var err error
	if handler, ok := c.handlers.(peerRelayReceiver); ok {
		local, err = handler.PeerRelayReceiver(setupCtx, hdr.Target, hdr.Ref)
	} else {
		err = errors.New("peer relay receiver unavailable")
	}
	ready := proto.PeerRelayReady{}
	if err != nil {
		ready.Error = err.Error()
		if len(ready.Error) > 1024 {
			ready.Error = ready.Error[:1024]
		}
	}
	_ = stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
	writeErr := json.NewEncoder(stream).Encode(ready)
	_ = stream.SetWriteDeadline(time.Time{})
	if local != nil {
		defer local.Close()
	}
	if err != nil || writeErr != nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { local.Close(); stream.Close() })
	defer stop()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(local, stream)
		if half, ok := local.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	}()
	go func() { _, _ = io.Copy(stream, local); close(done) }()
	<-done
}

// peerRelaySourceConn reads the raw stream independently of archive production.
// TLS remains the sole consumer. A bounded queue lets a FIN/reset cancel a
// producer blocked on work without concurrently reading from tls.Conn.
type peerRelaySourceConn struct {
	net.Conn
	finished  chan struct{}
	cancel    context.CancelFunc
	once      sync.Once
	closeErr  error
	readMu    sync.Mutex
	available *sync.Cond
	buffered  bytes.Buffer
	readErr   error
}

func newPeerRelaySourceConn(raw net.Conn, cancel context.CancelFunc) *peerRelaySourceConn {
	c := &peerRelaySourceConn{Conn: raw, finished: make(chan struct{}), cancel: cancel}
	c.available = sync.NewCond(&c.readMu)
	go func() {
		defer close(c.finished)
		defer cancel()
		data := make([]byte, 16*1024)
		for {
			n, err := raw.Read(data)
			c.readMu.Lock()
			if c.buffered.Len()+n > 128*1024 {
				c.readErr = errors.New("peer relay incoming data exceeds buffer limit")
				c.available.Broadcast()
				c.readMu.Unlock()
				raw.Close()
				return
			}
			if n > 0 {
				_, _ = c.buffered.Write(data[:n])
			}
			if err != nil {
				c.readErr = err
			}
			c.available.Broadcast()
			c.readMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return c
}

func (c *peerRelaySourceConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for c.buffered.Len() == 0 && c.readErr == nil {
		c.available.Wait()
	}
	// Drain a final TLS acknowledgement even when FIN arrives before its reader.
	if c.buffered.Len() > 0 {
		return c.buffered.Read(p)
	}
	return 0, c.readErr
}

// CloseWrite half-closes the stream after the archive, so the server sees the
// source's EOF and switches the relay from its no-progress deadline to its
// confirmation deadline while the receiver finishes. Without it the server
// saw EOF only after the ACK, and a receiver busy past the no-progress
// deadline (an image load, a large model fsync) was reclaimed as stalled.
func (c *peerRelaySourceConn) CloseWrite() error {
	half, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return nil
	}
	return half.CloseWrite()
}

func (c *peerRelaySourceConn) Close() error {
	c.once.Do(func() { c.cancel(); c.closeErr = c.Conn.Close() })
	<-c.finished
	return c.closeErr
}
