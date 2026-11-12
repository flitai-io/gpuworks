package modelsync

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// DialLocal connects only to the receiver's private listening socket. The caller
// owns the returned raw connection; TLS and token validation remain end-to-end.
func (r *Receiver) DialLocal(ctx context.Context) (net.Conn, error) {
	endpoints := r.localEndpoints()
	if len(endpoints) == 0 {
		return nil, errors.New("model sync local receiver is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var failures []error
	for _, endpoint := range endpoints {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
		if err == nil {
			return conn, nil
		}
		failures = append(failures, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("dial model sync local receiver: %w", errors.Join(failures...))
}

func (r *Receiver) localEndpoints() []string {
	if r == nil || r.listener == nil {
		return nil
	}
	address, ok := r.listener.Addr().(*net.TCPAddr)
	if !ok || address == nil || address.Port <= 0 || address.Port > 65535 || address.IP == nil {
		return nil
	}
	local := *address
	if !local.IP.IsUnspecified() {
		return []string{local.String()}
	}
	local.Zone = ""
	if local.IP.To4() != nil {
		local.IP = net.IPv4(127, 0, 0, 1)
		return []string{local.String()}
	}
	// [::] can describe either IPv6-only or dual-stack listeners. Prefer the
	// actual address family, then allow IPv4 when IPv6 loopback is disabled.
	// Trying IPv4 first could reach an unrelated service on the same port of
	// an IPv6-only receiver. Advertised endpoints never participate here.
	local.IP = net.IPv6loopback
	ipv6 := local.String()
	local.IP = net.IPv4(127, 0, 0, 1)
	return []string{ipv6, local.String()}
}

// ProbeOnConnWithMagic authenticates a raw transport without sending an archive.
func ProbeOnConnWithMagic(ctx context.Context, raw net.Conn, token, fingerprint, magic string) error {
	if raw == nil {
		return errors.New("model sync connection is required")
	}
	defer raw.Close()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(fingerprint) == "" {
		return errors.New("model sync credentials are incomplete")
	}
	conn, err := pinnedConn(ctx, raw, fingerprint)
	if err != nil {
		return err
	}
	return probeArchive(ctx, conn, token, magic)
}

func probeArchive(ctx context.Context, conn *tls.Conn, token, magic string) (resultErr error) {
	if strings.TrimSpace(magic) == "" {
		magic = protocolMagic
	}
	defer func() {
		if resultErr != nil && ctx.Err() != nil {
			resultErr = ctx.Err()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.NetConn().Close()
		case <-done:
		}
	}()
	if err := writeJSONLine(conn, header{Magic: magic, Token: token, Probe: true}); err != nil {
		return fmt.Errorf("write archive probe header: %w", err)
	}
	var ack acknowledgement
	if err := json.NewDecoder(conn).Decode(&ack); err != nil {
		return fmt.Errorf("read archive probe acknowledgement: %w", err)
	}
	if !ack.OK {
		return errors.New(ack.Error)
	}
	return nil
}
