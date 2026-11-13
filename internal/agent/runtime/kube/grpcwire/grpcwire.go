// Package grpcwire speaks just enough gRPC for the kubelet's local APIs (the
// PodResources lister, device plugin registration and serving): HTTP/2
// without TLS over a Unix socket, length-prefixed messages, the status in
// the trailers, and the few protobuf wire forms those messages use, encoded
// and decoded by hand. The agent therefore carries neither a gRPC nor a
// protobuf library.
package grpcwire

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// maxMessage bounds a message read from the kubelet.
const maxMessage = 32 << 20

// Fields walks a protobuf message, handing every length-delimited field to
// fn and every varint field to fn as its value's bytes (see Varint), and
// skipping fixed-width ones.
func Fields(b []byte, fn func(num int, wire int, payload []byte) error) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return errors.New("protobuf: bad field key")
		}
		b = b[n:]
		num, wire := int(key>>3), int(key&7)
		switch wire {
		case 0:
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return errors.New("protobuf: bad varint")
			}
			if err := fn(num, wire, b[:n]); err != nil {
				return err
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return errors.New("protobuf: short fixed64")
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return errors.New("protobuf: short fixed32")
			}
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return errors.New("protobuf: bad length")
			}
			payload := b[n : n+int(l)]
			b = b[n+int(l):]
			if err := fn(num, wire, payload); err != nil {
				return err
			}
		default:
			return fmt.Errorf("protobuf: unsupported wire type %d", wire)
		}
	}
	return nil
}

// Varint reads a varint field's payload.
func Varint(payload []byte) uint64 {
	v, _ := binary.Uvarint(payload)
	return v
}

// Message builds a protobuf message field by field.
type Message struct{ b []byte }

func (m *Message) key(num, wire int) { m.b = binary.AppendUvarint(m.b, uint64(num)<<3|uint64(wire)) }

// Bytes appends a length-delimited field (a string, bytes or a message).
func (m *Message) Bytes(num int, payload []byte) *Message {
	m.key(num, 2)
	m.b = binary.AppendUvarint(m.b, uint64(len(payload)))
	m.b = append(m.b, payload...)
	return m
}

// String appends a string field; the empty string is the default and left out.
func (m *Message) String(num int, s string) *Message {
	if s == "" {
		return m
	}
	return m.Bytes(num, []byte(s))
}

// Bool appends a bool field; false is the default and left out.
func (m *Message) Bool(num int, v bool) *Message {
	if !v {
		return m
	}
	m.key(num, 0)
	m.b = append(m.b, 1)
	return m
}

// Map appends a map<string,string> field: one entry message per key.
func (m *Message) Map(num int, kv map[string]string) *Message {
	for k, v := range kv {
		var e Message
		e.String(1, k).String(2, v)
		m.Bytes(num, e.Encode())
	}
	return m
}

// Encode returns the message's bytes.
func (m *Message) Encode() []byte {
	if m.b == nil {
		return []byte{}
	}
	return m.b
}

// Frame wraps a message in gRPC's length prefix (uncompressed).
func Frame(msg []byte) []byte {
	out := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:], uint32(len(msg)))
	return append(out, msg...)
}

// ReadFrame reads one length-prefixed message.
func ReadFrame(r io.Reader) ([]byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if head[0] != 0 {
		return nil, errors.New("grpc: compressed messages are not supported")
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > maxMessage {
		return nil, fmt.Errorf("grpc: message of %d bytes is too large", n)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// Client calls a gRPC server on a Unix socket.
type Client struct {
	socket string
	http   *http.Client
}

// Dial returns a client for the socket at path.
func Dial(path string, timeout time.Duration) *Client {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Client{socket: path, http: &http.Client{Transport: tr, Timeout: timeout}}
}

// Unary makes a unary call (method is "/package.Service/Method") and
// returns the response message.
func (c *Client) Unary(ctx context.Context, method string, request []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+method, bytes.NewReader(Frame(request)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s on %s: %w", method, c.socket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+5))
	if err != nil {
		return nil, err
	}
	if st := status(resp); st != "" && st != "0" {
		return nil, fmt.Errorf("%s: grpc-status %s: %s", method, st, firstNonEmpty(resp.Trailer.Get("Grpc-Message"), resp.Header.Get("Grpc-Message")))
	}
	if len(body) == 0 {
		return []byte{}, nil
	}
	msg, err := ReadFrame(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if len(body) != len(msg)+5 {
		return nil, fmt.Errorf("%s: frame says %d bytes, got %d", method, len(msg), len(body)-5)
	}
	return msg, nil
}

func status(resp *http.Response) string {
	if st := resp.Trailer.Get("Grpc-Status"); st != "" {
		return st
	}
	return resp.Header.Get("Grpc-Status") // a trailers-only answer
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Server-side helpers.

// StartResponse writes a gRPC response's headers; the status follows as
// trailers (Finish).
func StartResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
}

// Send writes one message and flushes it (a streaming response's next item).
func Send(w http.ResponseWriter, msg []byte) error {
	if _, err := w.Write(Frame(msg)); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// Finish writes the call's status: code 0 is OK.
func Finish(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Grpc-Status", fmt.Sprint(code))
	if message != "" {
		w.Header().Set("Grpc-Message", message)
	}
}

// Fail answers a call with an error status and no message.
func Fail(w http.ResponseWriter, code int, message string) {
	StartResponse(w)
	Finish(w, code, message)
}

// Status codes the agent answers with.
const (
	CodeInvalidArgument = 3
	CodeUnimplemented   = 12
	CodeInternal        = 13
)

// Listen serves h over unencrypted HTTP/2 on a Unix socket until ctx ends.
func Listen(ctx context.Context, path string, h http.Handler) (stop func(), err error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: &protocols, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	stop = func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(cctx)
		_ = srv.Close()
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, nil
}
