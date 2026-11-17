package recovery

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// realProbeProvisioner probes with the production SSH installer, so the
// error recovery records is the one a real endpoint produces.
type realProbeProvisioner struct {
	*fakeProvisioner
	installer *provision.SSHInstaller
	port      int
}

func (p realProbeProvisioner) Probe(ctx context.Context, req provision.SSHProbeRequest) provision.ProbeResult {
	req.Host, req.Port = "127.0.0.1", p.port
	return p.installer.Probe(ctx, req)
}

// closedPort is a local port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// endpoint accepts connections and hands each to serve.
func endpoint(t *testing.T, serve func(net.Conn)) int {
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

// Each failure class an SSH endpoint can produce is recorded with its code,
// decided where provision met the network, never by reading the text.
func TestRecoveryRecordsTheCodeOfARealSSHFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		port  func(t *testing.T) int
		phase string
		code  string
	}{
		{name: "connection refused", port: closedPort, phase: store.RecoveryPhaseUnreachable, code: messages.ErrorSSHUnreachable},
		{name: "silent endpoint", port: func(t *testing.T) int {
			return endpoint(t, func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) })
		}, phase: store.RecoveryPhaseUnreachable, code: messages.ErrorSSHTimeout},
		{name: "closed before identification", port: func(t *testing.T) int {
			return endpoint(t, func(conn net.Conn) { _, _ = io.WriteString(conn, "Exceeded MaxStartups\r\n"); _ = conn.Close() })
		}, phase: store.RecoveryPhaseUnreachable, code: messages.ErrorSSHClosedBeforeIdent},
		{name: "not an ssh server after its banner", port: func(t *testing.T) int {
			return endpoint(t, func(conn net.Conn) {
				_, _ = io.WriteString(conn, "SSH-2.0-OpenSSH_9.6\r\n")
				_, _ = conn.Write([]byte{0, 0, 0, 12, 4, 99, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
				_ = conn.Close()
			})
		}, phase: store.RecoveryPhaseUnreachable, code: messages.ErrorSSHHandshakeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st, _, fake := newRecoveryHarness(t)
			installer := provision.NewSSHInstaller(nil)
			installer.Timeout = 300 * time.Millisecond
			r.provisioner = realProbeProvisioner{fakeProvisioner: fake, installer: installer, port: tc.port(t)}
			node := newOfflineNode(t, st, true)
			outcome, err := r.recoverNode(context.Background(), node, false)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Phase != tc.phase || outcome.ErrorCode != tc.code || outcome.Error == "" {
				t.Fatalf("outcome = %+v, want %s %s", outcome, tc.phase, tc.code)
			}
			updated, err := st.NodeByUUID(node.UUID)
			if err != nil || updated.LastRecoveryCode != tc.code || updated.LastRecoveryError != outcome.Error {
				t.Fatalf("recorded %q / %q (%v), want %s / %q", updated.LastRecoveryCode, updated.LastRecoveryError, err, tc.code, outcome.Error)
			}
			if fake.restarts != 0 || fake.installs != 0 {
				t.Fatalf("an unreachable node was restarted %d / reinstalled %d", fake.restarts, fake.installs)
			}
		})
	}
}

// A node recorded before codes keeps its text and reads with no code; the
// next failure records both.
func TestRecoveryErrorWithoutACodeFromAnOlderServer(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	node := newOfflineNode(t, st, true)
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"recovery_phase": store.RecoveryPhaseUnreachable, "recovery_attempts": 1, "last_recovery_error": "network: ssh: connect refused",
	}).Error; err != nil {
		t.Fatal(err)
	}
	old, err := st.NodeByUUID(node.UUID)
	if err != nil || old.LastRecoveryCode != "" || old.LastRecoveryError != "network: ssh: connect refused" {
		t.Fatalf("old row = %q / %q (%v)", old.LastRecoveryCode, old.LastRecoveryError, err)
	}
	r.provisioner = realProbeProvisioner{fakeProvisioner: fake, installer: provision.NewSSHInstaller(nil), port: closedPort(t)}
	if _, err := r.recoverNode(context.Background(), old, true); err != nil {
		t.Fatal(err)
	}
	blocked, _ := st.NodeByUUID(node.UUID)
	if blocked.RecoveryPhase != store.RecoveryPhaseBlocked || blocked.LastRecoveryCode != messages.ErrorSSHUnreachable ||
		!strings.HasPrefix(blocked.LastRecoveryError, store.RecoveryPhaseUnreachable+": ") {
		t.Fatalf("repeated failure = %s %q / %q", blocked.RecoveryPhase, blocked.LastRecoveryCode, blocked.LastRecoveryError)
	}
	if err := st.ClearNodeRecovery(node.UUID); err != nil {
		t.Fatal(err)
	}
	cleared, _ := st.NodeByUUID(node.UUID)
	if cleared.LastRecoveryCode != "" || cleared.LastRecoveryError != "" {
		t.Fatalf("cleared = %q / %q", cleared.LastRecoveryCode, cleared.LastRecoveryError)
	}
}
