package app

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	netBenchMagic       = "GPUWORKS-NET/1"
	netBenchChunkBytes  = 1 << 20
	maxBandwidthBytes   = int64(128 << 30)
	maxNetBenchSessions = 16
)

// netProbeParams is the net.probe RPC input used after the target agent has
// opened a short-lived authenticated sink.
type netProbeParams struct {
	Target    string `json:"target"`
	Token     string `json:"token"`
	Bandwidth bool   `json:"bandwidth"`
	Seconds   int    `json:"seconds"`
	// SourceIP binds the probe to a local address so the traffic leaves on
	// the network under test; empty lets the kernel route it.
	SourceIP string `json:"source_ip,omitempty"`
}

type netProbeResult struct {
	Reachable       bool    `json:"reachable"`
	RTTMillisP50    float64 `json:"rtt_ms_p50"`
	RTTMillisP99    float64 `json:"rtt_ms_p99"`
	Gbps            float64 `json:"gbps,omitempty"`
	BandwidthTested bool    `json:"bandwidth_tested"`
	BytesSent       int64   `json:"bytes_sent,omitempty"`
	BytesReceived   int64   `json:"bytes_received,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Error           string  `json:"error,omitempty"`
	BandwidthError  string  `json:"bandwidth_error,omitempty"`
}

type netBenchStartParams struct {
	Port       int    `json:"port"`
	Token      string `json:"token"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type netBenchStopParams struct {
	Token string `json:"token"`
}

type netBenchStartResult struct {
	Port int `json:"port"`
}

func (n *NodeRuntime) rpcNetBenchStart(params json.RawMessage) (any, error) {
	var p netBenchStartParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if p.Port < 0 || p.Port > 65535 {
		return nil, fmt.Errorf("invalid benchmark port")
	}
	if !validNetBenchToken(p.Token) {
		return nil, fmt.Errorf("invalid benchmark token")
	}
	if p.TTLSeconds == 0 {
		p.TTLSeconds = 60
	}
	if p.TTLSeconds < 5 || p.TTLSeconds > 3600 {
		return nil, fmt.Errorf("benchmark listener TTL must be between 5 and 3600 seconds")
	}
	port, err := n.netBench.start(p.Port, p.Token, time.Duration(p.TTLSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	return netBenchStartResult{Port: port}, nil
}

func (n *NodeRuntime) rpcNetBenchStop(params json.RawMessage) (any, error) {
	var p netBenchStopParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if !validNetBenchToken(p.Token) {
		return nil, fmt.Errorf("invalid benchmark token")
	}
	n.netBench.stop(p.Token)
	return map[string]bool{"stopped": true}, nil
}

// rpcNetProbe measures authenticated TCP handshake RTT and, when requested,
// streams data to the peer sink and uses the peer's received-byte count.
func (n *NodeRuntime) rpcNetProbe(ctx context.Context, params json.RawMessage) (any, error) {
	var p netProbeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	if err := validateNetProbeParams(p); err != nil {
		return nil, err
	}
	if p.Seconds == 0 {
		p.Seconds = 3
	}
	res := netProbeResult{}
	rtts := make([]float64, 0, 20)
	for i := 0; i < 20; i++ {
		start := time.Now()
		conn, _, err := dialNetBench(ctx, p.SourceIP, p.Target, p.Token, "ping")
		if err != nil {
			res.Error = err.Error()
			return res, nil //nolint:nilerr // A failed probe is a successful RPC carrying its diagnostic result.
		}
		rtts = append(rtts, float64(time.Since(start).Microseconds())/1000.0)
		_ = conn.Close()
	}
	res.Reachable = true
	res.RTTMillisP50 = percentile(rtts, 0.5)
	res.RTTMillisP99 = percentile(rtts, 0.99)
	if !p.Bandwidth {
		return res, nil
	}

	gbps, sent, received, duration, err := measureBandwidth(ctx, p.SourceIP, p.Target, p.Token, p.Seconds)
	if err != nil {
		res.BandwidthError = err.Error()
		return res, nil //nolint:nilerr // Preserve reachability and expose bandwidth failure in the result.
	}
	res.BandwidthTested = true
	res.Gbps = gbps
	res.BytesSent = sent
	res.BytesReceived = received
	res.DurationSeconds = duration
	return res, nil
}

func validateNetProbeParams(p netProbeParams) error {
	host, port, err := net.SplitHostPort(p.Target)
	if err != nil || strings.TrimSpace(host) == "" {
		return fmt.Errorf("target must be host:port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber <= 0 || portNumber > 65535 {
		return fmt.Errorf("target port is invalid")
	}
	if !validNetBenchToken(p.Token) {
		return fmt.Errorf("invalid benchmark token")
	}
	if p.Seconds < 0 || p.Seconds > 10 {
		return fmt.Errorf("bandwidth duration must not exceed 10 seconds")
	}
	if p.SourceIP != "" && net.ParseIP(p.SourceIP) == nil {
		return fmt.Errorf("source_ip is not an IP address")
	}
	return nil
}

func validNetBenchToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func dialNetBench(ctx context.Context, sourceIP, target, token, mode string) (net.Conn, *bufio.Reader, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := &net.Dialer{}
	if ip := net.ParseIP(sourceIP); ip != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: ip}
	}
	conn, err := dialer.DialContext(dialCtx, "tcp", target)
	if err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(3 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	if _, err := fmt.Fprintf(conn, "%s %s %s\n", netBenchMagic, token, mode); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	reader := bufio.NewReaderSize(conn, 256)
	line, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if strings.TrimSpace(line) != "OK" {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("benchmark peer rejected authentication")
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, nil
}

func measureBandwidth(ctx context.Context, sourceIP, target, token string, seconds int) (float64, int64, int64, float64, error) {
	conn, reader, err := dialNetBench(ctx, sourceIP, target, token, "sink")
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer conn.Close()
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("benchmark connection is not TCP")
	}
	buf := make([]byte, netBenchChunkBytes)
	started := time.Now()
	end := started.Add(time.Duration(seconds) * time.Second)
	_ = conn.SetWriteDeadline(end)
	var sent int64
	for time.Now().Before(end) && sent < maxBandwidthBytes {
		n, writeErr := conn.Write(buf)
		sent += int64(n)
		if writeErr != nil {
			var netErr net.Error
			if errors.As(writeErr, &netErr) && netErr.Timeout() {
				break
			}
			return 0, sent, 0, time.Since(started).Seconds(), writeErr
		}
		select {
		case <-ctx.Done():
			return 0, sent, 0, time.Since(started).Seconds(), ctx.Err()
		default:
		}
	}
	duration := time.Since(started).Seconds()
	_ = conn.SetWriteDeadline(time.Time{})
	if err := tcpConn.CloseWrite(); err != nil {
		return 0, sent, 0, duration, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, sent, 0, duration, fmt.Errorf("read peer byte count: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "BYTES" {
		return 0, sent, 0, duration, fmt.Errorf("invalid peer byte count")
	}
	received, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || received <= 0 || received > sent {
		return 0, sent, received, duration, fmt.Errorf("invalid peer byte count")
	}
	if duration <= 0 {
		return 0, sent, received, duration, fmt.Errorf("invalid benchmark duration")
	}
	return float64(received*8) / duration / 1e9, sent, received, duration, nil
}

func percentile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
	idx := int(q * float64(len(s)-1))
	return s[idx]
}

type netBenchManager struct {
	mu        sync.Mutex
	listeners map[string]*netBenchListener
}

type netBenchListener struct {
	ln      net.Listener
	token   string
	expires time.Time
	done    chan struct{}
	once    sync.Once

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func newNetBenchManager() *netBenchManager {
	return &netBenchManager{listeners: map[string]*netBenchListener{}}
}

func (m *netBenchManager) start(port int, token string, ttl time.Duration) (int, error) {
	m.mu.Lock()
	if len(m.listeners) >= maxNetBenchSessions {
		m.mu.Unlock()
		return 0, fmt.Errorf("too many benchmark listeners")
	}
	if _, exists := m.listeners[token]; exists {
		m.mu.Unlock()
		return 0, fmt.Errorf("benchmark listener already exists")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		m.mu.Unlock()
		return 0, err
	}
	listener := &netBenchListener{
		ln: ln, token: token, expires: time.Now().Add(ttl), done: make(chan struct{}), conns: map[net.Conn]struct{}{},
	}
	m.listeners[token] = listener
	m.mu.Unlock()

	go listener.serve()
	go func() {
		timer := time.NewTimer(ttl)
		defer timer.Stop()
		select {
		case <-timer.C:
			m.stop(token)
		case <-listener.done:
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (m *netBenchManager) stop(token string) {
	m.mu.Lock()
	listener := m.listeners[token]
	delete(m.listeners, token)
	m.mu.Unlock()
	if listener != nil {
		listener.close()
	}
}

func (l *netBenchListener) serve() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}
		if !l.track(conn) {
			_ = conn.Close()
			return
		}
		go func() {
			defer l.untrack(conn)
			defer conn.Close()
			l.handle(conn)
		}()
	}
}

func (l *netBenchListener) track(conn net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.conns[conn] = struct{}{}
	return true
}

func (l *netBenchListener) untrack(conn net.Conn) {
	l.mu.Lock()
	delete(l.conns, conn)
	l.mu.Unlock()
}

func (l *netBenchListener) close() {
	l.once.Do(func() {
		_ = l.ln.Close()
		l.mu.Lock()
		l.closed = true
		for conn := range l.conns {
			_ = conn.Close()
		}
		l.mu.Unlock()
		close(l.done)
	})
}

func (l *netBenchListener) handle(conn net.Conn) {
	deadline := l.expires
	if maxDeadline := time.Now().Add(20 * time.Second); maxDeadline.Before(deadline) {
		deadline = maxDeadline
	}
	_ = conn.SetDeadline(deadline)
	reader := bufio.NewReaderSize(conn, 512)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 256 {
		return
	}
	fields := strings.Fields(string(line))
	if len(fields) != 3 || fields[0] != netBenchMagic || !constantTimeTokenEqual(fields[1], l.token) {
		_, _ = io.WriteString(conn, "DENIED\n")
		return
	}
	if fields[2] != "ping" && fields[2] != "sink" {
		_, _ = io.WriteString(conn, "DENIED\n")
		return
	}
	if _, err := io.WriteString(conn, "OK\n"); err != nil || fields[2] == "ping" {
		return
	}
	received, err := io.Copy(io.Discard, io.LimitReader(reader, maxBandwidthBytes+1))
	if err != nil || received <= 0 || received > maxBandwidthBytes {
		return
	}
	_, _ = fmt.Fprintf(conn, "BYTES %d\n", received)
}

func constantTimeTokenEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
