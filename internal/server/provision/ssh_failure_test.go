package provision

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// listenerPort serves each accepted connection with serve.
func listenerPort(t *testing.T, serve func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// Every way an SSH endpoint can refuse a probe reaches the caller with its
// code, named where the network or the SSH library reported it.
func TestProbeFailuresCarryTheirCode(t *testing.T) {
	fastConnectBackoff(t)
	srv := startAuthServer(t, "root", "right-password")
	for _, tc := range []struct {
		name string
		port func(t *testing.T) int
		auth SSHAuth
		hook func(t *testing.T, installer *SSHInstaller, port int)
		code string
	}{
		{name: "refused", port: unusedPort, auth: SSHAuth{Mode: AuthPassword, Password: "x"}, code: messages.ErrorSSHUnreachable},
		{name: "silent", port: func(t *testing.T) int {
			return listenerPort(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
		}, auth: SSHAuth{Mode: AuthPassword, Password: "x"}, code: messages.ErrorSSHTimeout},
		{name: "max startups", port: func(t *testing.T) int {
			return listenerPort(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "Exceeded MaxStartups\r\n"); _ = conn.Close() })
		}, auth: SSHAuth{Mode: AuthPassword, Password: "x"}, code: messages.ErrorSSHClosedBeforeIdent},
		{name: "broken handshake", port: func(t *testing.T) int {
			return listenerPort(t, func(conn net.Conn) {
				_, _ = io.WriteString(conn, "SSH-2.0-OpenSSH_9.6\r\n")
				_, _ = conn.Write([]byte{0, 0, 0, 12, 4, 99, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
				_ = conn.Close()
			})
		}, auth: SSHAuth{Mode: AuthPassword, Password: "x"}, code: messages.ErrorSSHHandshakeFailed},
		{name: "wrong password", port: func(*testing.T) int { return srv.port }, auth: SSHAuth{Mode: AuthPassword, Password: "wrong"}, code: messages.ErrorSSHAuthFailed},
		{name: "unparsable key", port: func(*testing.T) int { return srv.port }, auth: SSHAuth{Mode: AuthPrivateKey, PrivateKeyPEM: "not a key"}, code: messages.ErrorSSHCredentialInvalid},
		{name: "host key changed", port: func(*testing.T) int { return srv.port }, auth: SSHAuth{Mode: AuthPassword, Password: "right-password"},
			hook: func(t *testing.T, installer *SSHInstaller, port int) {
				old := verifyHostKeys
				verifyHostKeys = true
				t.Cleanup(func() { verifyHostKeys = old })
				if err := installer.HostKeys.Put(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "SHA256:another-host"); err != nil {
					t.Fatal(err)
				}
			}, code: messages.ErrorSSHHostKeyMismatch},
		// Logged in, the probe script refuses to run without a node token.
		{name: "command failed", port: func(*testing.T) int { return srv.port }, auth: SSHAuth{Mode: AuthPassword, Password: "right-password"}, code: messages.ErrorSSHCommandFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installer := NewSSHInstaller(nil)
			installer.Timeout = 300 * time.Millisecond
			port := tc.port(t)
			if tc.hook != nil {
				tc.hook(t, installer, port)
			}
			result := installer.Probe(context.Background(), SSHProbeRequest{Host: "127.0.0.1", Port: port, User: "root", SSHAuth: tc.auth})
			code, ok := FailureCode(result.Err)
			if !ok || code != tc.code {
				t.Fatalf("code = %q (%v) for %v, want %s", code, ok, result.Err, tc.code)
			}
		})
	}
}

// Install and restart failures name themselves where they happen.
func TestInstallFailuresCarryTheirCode(t *testing.T) {
	srv := startAuthServer(t, "root", "pw")
	client, _, err := dial(t, srv, SSHAuth{Mode: AuthPassword, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	err = uploadStream(context.Background(), client, srv.home+"/agent", []byte("binary"), true, time.Nanosecond, false)
	if code, _ := FailureCode(err); code != messages.ErrorAgentInstallUploadStalled {
		t.Fatalf("stalled upload = %v (%s)", err, code)
	}
	for _, tc := range []struct {
		out  string
		code string
	}{
		{"gpuworks-agent.service: Main process exited, code=exited, status=203/EXEC", messages.ErrorAgentInstallBinaryMissing},
		{"/opt/gpuworks/gpuworks-agent: not executable", messages.ErrorAgentInstallBinaryMissing},
		{"Job for gpuworks-agent.service failed", messages.ErrorAgentInstallStartFailed},
	} {
		if code, _ := FailureCode(startFailure(tc.out, errors.New("exit status 1"))); code != tc.code {
			t.Errorf("start output %q = %s, want %s", tc.out, code, tc.code)
		}
	}
}

// A code named by the failing step wins over what its cause would say, and
// an error nothing names has no code.
func TestFailureCodePrefersTheNamingStep(t *testing.T) {
	stalled := failure(messages.ErrorAgentInstallUploadStalled, context.DeadlineExceeded)
	if code, _ := FailureCode(&SSHConnectionError{Err: stalled}); code != messages.ErrorAgentInstallUploadStalled {
		t.Fatalf("code = %s", code)
	}
	if code, ok := FailureCode(errors.New("something else")); ok || code != "" {
		t.Fatalf("unnamed = %q %v", code, ok)
	}
	if code, ok := FailureCode(nil); ok || code != "" {
		t.Fatalf("nil = %q %v", code, ok)
	}
	if !strings.Contains(stalled.Error(), "deadline") {
		t.Fatalf("failure text = %q, want the cause's", stalled.Error())
	}
}
