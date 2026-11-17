package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// readReset is the error chain net/http hands back when the registry resets
// the connection mid-body.
func readReset() error {
	return &url.Error{Op: "Get", URL: "https://registry.example/v2/a/blobs/sha256:1", Err: &net.OpError{
		Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET},
	}}
}

// realClientTimeout makes net/http time out against a listener that accepts
// and never answers, so the timeout error is the one the client really builds.
func realClientTimeout(t *testing.T) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	client := &http.Client{Timeout: 50 * time.Millisecond}
	_, err = client.Get("http://" + ln.Addr().String() + "/v2/")
	if err == nil {
		t.Fatal("a silent server answered")
	}
	return err
}

func TestRetryableClassifiesByTypeNotText(t *testing.T) {
	timeout := realClientTimeout(t)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"idle", errIdle, true},
		{"blob size mismatch", fmt.Errorf("%w: size mismatch: got 1 want 2", errBlobMismatch), true},
		{"wrapped digest mismatch", fmt.Errorf("layer sha256:1: %w", fmt.Errorf("%w: digest mismatch", errBlobMismatch)), true},
		{"connection reset", fmt.Errorf("layer: %w", readReset()), true},
		{"unexpected eof", fmt.Errorf("copy: %w", io.ErrUnexpectedEOF), true},
		{"client timeout", timeout, true},
		{"dns not found", &url.Error{Op: "Get", URL: "https://r.example/v2/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "r.example", IsNotFound: true}}}, true},
		{"blob 502", &statusError{msg: "blob: 502 Bad Gateway", status: http.StatusBadGateway}, true},
		{"blob 429 throttles", &statusError{msg: "blob: 429 Too Many Requests", status: http.StatusTooManyRequests}, true},
		{"blob 404", &statusError{msg: "blob: 404 Not Found", status: http.StatusNotFound}, false},
		{"blob 501", &statusError{msg: "blob: 501 Not Implemented", status: http.StatusNotImplemented}, false},
		// The old text matcher retried anything that merely mentioned these
		// words; an error the repository wrote itself is not retryable by
		// its wording.
		{"own text mentioning timeout", errors.New("config timeout must be positive: 500"), false},
		{"canceled", context.Canceled, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Fatalf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsResetSeparatesTornConnectionsFromTimeouts(t *testing.T) {
	if !isReset(fmt.Errorf("blob: %w", readReset())) {
		t.Fatal("a reset read is a reset")
	}
	pipe := &net.OpError{Op: "write", Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}}
	if !isReset(pipe) || !isReset(io.EOF) {
		t.Fatal("broken pipe and early EOF are resets")
	}
	if isReset(realClientTimeout(t)) {
		t.Fatal("a timeout is not a reset")
	}
	if isReset(errors.New("connection reset by peer")) {
		t.Fatal("text that only looks like a reset must not classify")
	}
}

func TestPullFailureMarksRegistryRefusalsAndFullDisks(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"manifest 404", fmt.Errorf("resolve: %w", &statusError{msg: "manifest 1: 404 Not Found", status: http.StatusNotFound}), runtime.ErrRegistryRefused},
		{"token 401", &statusError{msg: "token endpoint: 401 Unauthorized", status: http.StatusUnauthorized}, runtime.ErrRegistryRefused},
		{"blob 403", &statusError{msg: "blob: 403 Forbidden", status: http.StatusForbidden}, runtime.ErrRegistryRefused},
		{"enospc", fmt.Errorf("layer: %w", &os.PathError{Op: "write", Path: "/l", Err: syscall.ENOSPC}), runtime.ErrNoSpace},
		{"unpack space", fmt.Errorf("%w under /x: 1 B free", errUnpackSpace), runtime.ErrNoSpace},
		{"blob 502", &statusError{msg: "blob: 502 Bad Gateway", status: http.StatusBadGateway}, nil},
		{"text only", errors.New("manifest 1: 404 Not Found"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pullFailure(tc.err)
			if got.Error() != tc.err.Error() {
				t.Fatalf("text changed: %q -> %q", tc.err, got)
			}
			for _, kind := range []error{runtime.ErrRegistryRefused, runtime.ErrNoSpace} {
				if errors.Is(got, kind) != (kind == tc.want) {
					t.Fatalf("errors.Is(%v, %v) = %v", got, kind, !(kind == tc.want))
				}
			}
		})
	}
}
