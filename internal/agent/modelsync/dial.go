package modelsync

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const dialParallelism = 8

// ErrDirectUnavailable marks exhaustion of TCP connection candidates before
// any TLS handshake or archive write. Only this failure permits relay fallback.
var ErrDirectUnavailable = errors.New("direct peer connection unavailable")

type dialResult struct {
	endpoint string
	conn     *tls.Conn
	err      error
}

func dialPinned(ctx context.Context, endpoints []string, localIP net.IP, fingerprint string) (*tls.Conn, error) {
	want, err := hex.DecodeString(strings.TrimSpace(fingerprint))
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("invalid model sync certificate fingerprint")
	}
	endpoints = uniqueEndpoints(endpoints)
	if len(endpoints) == 0 {
		return nil, errors.New("model sync endpoints are required")
	}

	// A node can advertise a hostname plus every address on a container-heavy
	// host. Probe a bounded number concurrently so a black-holed address cannot
	// delay a usable address that appears later in the list by 10s per attempt.
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan dialResult, len(endpoints))
	sem := make(chan struct{}, dialParallelism)
	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		endpoint := endpoint
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-dialCtx.Done():
				results <- dialResult{endpoint: endpoint, err: dialCtx.Err()}
				return
			}
			defer func() { <-sem }()

			conn, dialErr := dialEndpoint(dialCtx, endpoint, localIP, want)
			select {
			case results <- dialResult{endpoint: endpoint, conn: conn, err: dialErr}:
			case <-dialCtx.Done():
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var errs []string
	allConnectFailures := true
	var winner *tls.Conn
	for result := range results {
		if result.conn != nil {
			if winner == nil {
				winner = result.conn
				cancel()
			} else {
				_ = result.conn.Close()
			}
			continue
		}
		if result.err != nil {
			errs = append(errs, result.endpoint+": "+result.err.Error())
			if !errors.Is(result.err, ErrDirectUnavailable) {
				allConnectFailures = false
			}
		}
	}
	if winner != nil {
		return winner, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if allConnectFailures && len(errs) > 0 {
		return nil, fmt.Errorf("dial model sync target: %w: %s", ErrDirectUnavailable, strings.Join(errs, "; "))
	}
	return nil, errors.New("dial model sync target: " + strings.Join(errs, "; "))
}

func dialEndpoint(ctx context.Context, endpoint string, localIP net.IP, want []byte) (*tls.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if localIP != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: localIP}
	}
	raw, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrDirectUnavailable, err)
	}
	return handshakePinned(ctx, raw, want)
}

func pinnedConn(ctx context.Context, raw net.Conn, fingerprint string) (*tls.Conn, error) {
	want, err := hex.DecodeString(strings.TrimSpace(fingerprint))
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("invalid model sync certificate fingerprint")
	}
	return handshakePinned(ctx, raw, want)
}

func handshakePinned(ctx context.Context, raw net.Conn, want []byte) (*tls.Conn, error) {
	cfg := &tls.Config{
		InsecureSkipVerify: true, // verified by the one-time pinned fingerprint below
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return errors.New("model sync peer certificate count mismatch")
			}
			got := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return errors.New("model sync peer certificate fingerprint mismatch")
			}
			return nil
		},
	}
	conn := tls.Client(raw, cfg)
	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

func uniqueEndpoints(endpoints []string) []string {
	seen := make(map[string]struct{}, len(endpoints))
	out := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		if _, ok := seen[endpoint]; ok {
			continue
		}
		seen[endpoint] = struct{}{}
		out = append(out, endpoint)
	}
	return out
}
