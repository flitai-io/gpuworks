// Package modelsync implements authenticated archive transfer between nodes.
// Direct and caller-provided transports share the same end-to-end TLS and
// receiver-confirmed archive protocol.
package modelsync

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/traffic"
)

const (
	protocolMagic = "GPUWORKS-MODEL/1"
	archiveMagic  = "GPUWORKS-ARCHIVE/1"
	maxChunkSize  = 1 << 20
	defaultTTL    = 10 * time.Minute
	ioTimeout     = 30 * time.Second
)

type header struct {
	Magic string `json:"magic"`
	Token string `json:"token"`
	Probe bool   `json:"probe,omitempty"`
}

var errProbeOnly = errors.New("archive probe completed")

type acknowledgement struct {
	OK     bool   `json:"ok"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Result is the peer-confirmed archive transfer result.
type Result struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// ReceiveOptions configures one single-use receiver.
type ReceiveOptions struct {
	BindHost       string
	AdvertiseHosts []string
	Token          string
	MaxBytes       int64
	TTL            time.Duration
	Receive        func(context.Context, io.Reader) error
	Progress       func(int64)
	// Magic lets the same authenticated transport carry another archive kind.
	// Empty preserves the model-sync wire contract.
	Magic string
}

// Receiver owns a single-use TLS listener. Done always yields exactly once.
type Receiver struct {
	Endpoints   []string
	Fingerprint string

	listener net.Listener
	cancel   context.CancelFunc
	done     chan receiveResult
	once     sync.Once
}

type receiveResult struct {
	Result Result
	Err    error
}

// StartReceiver starts a one-shot TLS receiver and returns dialable endpoints.
func StartReceiver(parent context.Context, opts ReceiveOptions) (*Receiver, error) {
	slog.Info("model sync receiver starting", "bind_host", opts.BindHost, "advertise_hosts", opts.AdvertiseHosts)
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("model sync token is required")
	}
	if opts.Receive == nil {
		return nil, errors.New("model sync receive callback is required")
	}
	if strings.TrimSpace(opts.Magic) == "" {
		opts.Magic = protocolMagic
	}
	bindHost := strings.TrimSpace(opts.BindHost)
	if bindHost == "" {
		bindHost = "0.0.0.0"
	}
	tcpListener, err := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		slog.Warn("model sync receiver listen failed", "err", err)
		return nil, fmt.Errorf("listen for model sync: %w", err)
	}
	cert, fingerprint, err := ephemeralCertificate()
	if err != nil {
		_ = tcpListener.Close()
		return nil, err
	}
	tlsListener := tls.NewListener(tcpListener, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	port := tcpListener.Addr().(*net.TCPAddr).Port
	hosts := append([]string(nil), opts.AdvertiseHosts...)
	if ip := net.ParseIP(bindHost); ip != nil && !ip.IsUnspecified() {
		hosts = append([]string{ip.String()}, hosts...)
	}
	hosts = uniqueHosts(hosts)
	if len(hosts) == 0 {
		_ = tlsListener.Close()
		return nil, errors.New("model sync receiver has no advertise address")
	}
	endpoints := make([]string, 0, len(hosts))
	for _, host := range hosts {
		endpoints = append(endpoints, net.JoinHostPort(host, fmt.Sprint(port)))
	}

	ctx, cancel := context.WithCancel(parent)
	r := &Receiver{
		Endpoints: endpoints, Fingerprint: fingerprint,
		listener: tlsListener, cancel: cancel, done: make(chan receiveResult, 1),
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	go r.serve(ctx, ttl, opts)
	return r, nil
}

func (r *Receiver) serve(ctx context.Context, ttl time.Duration, opts ReceiveOptions) {
	defer r.cancel()
	defer r.listener.Close()
	expired := make(chan struct{})
	timer := time.AfterFunc(ttl, func() {
		close(expired)
		_ = r.listener.Close()
	})
	defer timer.Stop()
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				err = ctx.Err()
			case channelClosed(expired):
				err = errors.New("model sync receiver expired")
			default:
				err = fmt.Errorf("accept model sync: %w", err)
			}
			r.finish(receiveResult{Err: err})
			slog.Warn("model sync receiver ended", "err", err)
			return
		}
		connectionDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-connectionDone:
			}
		}()
		result, authenticated, receiveErr := receiveConnection(ctx, conn, opts)
		close(connectionDone)
		_ = conn.Close()
		// Cancellation closes the socket to interrupt blocked reads. Preserve
		// that cause instead of exposing the resulting closed-connection error.
		if receiveErr != nil && ctx.Err() != nil {
			receiveErr = ctx.Err()
		}
		if errors.Is(receiveErr, errProbeOnly) && ctx.Err() == nil && !channelClosed(expired) {
			continue
		}
		if !authenticated && ctx.Err() == nil && !channelClosed(expired) {
			continue
		}
		r.finish(receiveResult{Result: result, Err: receiveErr})
		if receiveErr != nil {
			slog.Warn("model sync receive failed", "err", receiveErr)
		} else {
			slog.Info("model sync receive completed", "bytes", result.Bytes)
		}
		return
	}
}

func (r *Receiver) finish(result receiveResult) {
	r.once.Do(func() {
		r.done <- result
		close(r.done)
	})
}

// Stop cancels an unused or active receiver.
func (r *Receiver) Stop() {
	r.cancel()
	_ = r.listener.Close()
}

// Wait blocks for completion or caller cancellation.
func (r *Receiver) Wait(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case result := <-r.done:
		return result.Result, result.Err
	}
}

func receiveConnection(ctx context.Context, conn net.Conn, opts ReceiveOptions) (Result, bool, error) {
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))
	reader := bufio.NewReader(conn)
	line, err := readLine(reader, 64<<10)
	if err != nil {
		return Result{}, false, fmt.Errorf("read model sync header: %w", err)
	}
	var hdr header
	if err := json.Unmarshal(line, &hdr); err != nil {
		return Result{}, false, writeAckError(conn, fmt.Errorf("decode model sync header: %w", err))
	}
	if hdr.Magic != opts.Magic || !secureEqual(hdr.Token, opts.Token) {
		return Result{}, false, writeAckError(conn, errors.New("model sync authentication failed"))
	}
	if hdr.Probe {
		if err := writeJSONLine(conn, acknowledgement{OK: true}); err != nil {
			return Result{}, true, err
		}
		return Result{}, true, errProbeOnly
	}
	_ = conn.SetDeadline(time.Time{})
	chunks := &chunkReader{r: reader}
	measured := newMeasuredReader(chunks, opts.MaxBytes, opts.Progress)
	receiveErr := opts.Receive(ctx, measured)
	if receiveErr == nil {
		_, receiveErr = io.Copy(io.Discard, measured)
	}
	measured.finishProgress()
	result := Result{Bytes: measured.bytes, SHA256: hex.EncodeToString(measured.hash.Sum(nil))}
	ack := acknowledgement{OK: receiveErr == nil, Bytes: result.Bytes, SHA256: result.SHA256}
	if receiveErr != nil {
		ack.Error = receiveErr.Error()
	}
	if err := writeJSONLine(conn, ack); err != nil && receiveErr == nil {
		receiveErr = fmt.Errorf("write model sync acknowledgement: %w", err)
	}
	return result, true, receiveErr
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func writeAckError(w io.Writer, cause error) error {
	_ = writeJSONLine(w, acknowledgement{OK: false, Error: cause.Error()})
	return cause
}

// Send dials the first reachable endpoint, pins the target certificate, and
// streams a framed archive produced by writeArchive.
func Send(ctx context.Context, endpoints []string, localIP net.IP, token, fingerprint string, writeArchive func(io.Writer) error, observe ...func(string)) (Result, error) {
	return SendWithMagic(ctx, endpoints, localIP, token, fingerprint, protocolMagic, writeArchive, observe...)
}

// SendWithMagic is Send with an explicit archive protocol name. It is used by
// image peer sync while keeping the model API source compatible.
func SendWithMagic(ctx context.Context, endpoints []string, localIP net.IP, token, fingerprint, magic string, writeArchive func(io.Writer) error, observe ...func(string)) (Result, error) {
	slog.Info("model sync send started", "endpoints", endpoints)
	if len(endpoints) == 0 {
		return Result{}, errors.New("model sync endpoints are required")
	}
	if strings.TrimSpace(token) == "" || strings.TrimSpace(fingerprint) == "" {
		return Result{}, errors.New("model sync credentials are incomplete")
	}
	if strings.TrimSpace(magic) == "" {
		magic = protocolMagic
	}
	if writeArchive == nil {
		return Result{}, errors.New("model sync archive callback is required")
	}
	conn, err := dialPinned(ctx, endpoints, localIP, fingerprint)
	if err != nil {
		slog.Warn("model sync send dial failed", "err", err)
		return Result{}, err
	}
	defer conn.Close()
	return sendArchive(ctx, conn, token, magic, writeArchive, false, observe...)
}

// Probe performs the authenticated TLS and token handshake without sending an
// archive. The receiver remains available for the subsequent real transfer.
func Probe(ctx context.Context, endpoints []string, localIP net.IP, token, fingerprint, magic string) error {
	conn, err := dialPinned(ctx, endpoints, localIP, fingerprint)
	if err != nil {
		return err
	}
	defer conn.Close()
	return probeArchive(ctx, conn, token, magic)
}

// RateLimitedWriter applies a per-transfer KiB/s cap. A non-positive cap is
// unlimited. It deliberately lives beside the wire implementation so model
// and image sends account for actual socket bytes in the same way. Waiting
// for the next allowance is canceled with the transfer context.
func RateLimitedWriter(ctx context.Context, w io.Writer, kibPerSecond int) io.Writer {
	if kibPerSecond <= 0 {
		return w
	}
	return &rateLimitedWriter{ctx: ctx, w: w, bytesPerSecond: int64(kibPerSecond) * 1024}
}

type rateLimitedWriter struct {
	ctx            context.Context
	w              io.Writer
	bytesPerSecond int64
	started        time.Time
	written        int64
}

func (w *rateLimitedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.started.IsZero() {
		w.started = time.Now()
	}
	if w.written > 0 {
		want := time.Duration(float64(w.written+int64(len(p))) / float64(w.bytesPerSecond) * float64(time.Second))
		if wait := want - time.Since(w.started); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-w.ctx.Done():
				timer.Stop()
				return 0, w.ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.w.Write(p)
	w.written += int64(n)
	return n, err
}

func ephemeralCertificate() (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "gpuworks-model-sync"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	fingerprint := sha256.Sum256(der)
	return cert, hex.EncodeToString(fingerprint[:]), nil
}

type chunkWriter struct {
	w     io.Writer
	hash  hash.Hash
	bytes int64
}

func newChunkWriter(w io.Writer) *chunkWriter {
	return &chunkWriter{w: w, hash: sha256.New()}
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxChunkSize {
			n = maxChunkSize
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(n))
		if err := writeAll(w.w, size[:]); err != nil {
			return total, err
		}
		if err := writeAll(w.w, p[:n]); err != nil {
			return total, err
		}
		_, _ = w.hash.Write(p[:n])
		traffic.AddStorageSyncTx(n)
		w.bytes += int64(n)
		total += n
		p = p[n:]
	}
	return total, nil
}

func (w *chunkWriter) Finish() error {
	var zero [4]byte
	return writeAll(w.w, zero[:])
}

type chunkReader struct {
	r         io.Reader
	remaining uint32
	done      bool
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	if r.remaining == 0 {
		var size [4]byte
		if _, err := io.ReadFull(r.r, size[:]); err != nil {
			// EOF before the explicit zero-length terminator is a truncated
			// transfer, not a successful end-of-stream. Treating plain EOF as
			// clean lets a sender canceled exactly between two frames be
			// acknowledged as complete by io.Copy.
			if err == io.EOF {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		r.remaining = binary.BigEndian.Uint32(size[:])
		if r.remaining == 0 {
			r.done = true
			return 0, io.EOF
		}
		if r.remaining > maxChunkSize {
			return 0, fmt.Errorf("model sync chunk exceeds %d bytes", maxChunkSize)
		}
	}
	if uint32(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	want := len(p)
	n, err := io.ReadFull(r.r, p[:want])
	r.remaining -= uint32(n)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if len(line) > max {
		return nil, io.ErrShortBuffer
	}
	if err != nil {
		return nil, err
	}
	return line[:len(line)-1], nil
}

func writeJSONLine(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeAll(w, b)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func secureEqual(a, b string) bool {
	aSum, bSum := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aSum[:], bSum[:]) == 1
}

func uniqueHosts(hosts []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(strings.Trim(host, "[]"))
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out
}
