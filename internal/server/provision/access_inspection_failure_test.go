package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"

	"golang.org/x/crypto/ssh"
)

// Exercise channel-open and incremental command output over the real SSH
// protocol. The peer stays connected until the inspection closes its transport.
func accessInspectionSSH(t *testing.T, handle func(ssh.NewChannel, <-chan struct{})) (SSHProbeRequest, <-chan struct{}) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() != "tester" || string(password) != "pw" {
			return nil, fmt.Errorf("authentication rejected")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done, closed := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("SSH fixture did not stop")
		}
	})
	go func() {
		defer close(done)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = raw.Close() })
		defer raw.Close()
		conn, channels, requests, err := ssh.NewServerConn(raw, cfg)
		if err != nil {
			t.Errorf("SSH handshake: %v", err)
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		go func() { _ = conn.Wait(); close(closed) }()
		for channel := range channels {
			handle(channel, closed)
			return
		}
	}()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	return SSHProbeRequest{Host: host, Port: port, User: "tester", Token: "owned", ServerURL: "http://server", ConnectMode: "agent_dial", SSHAuth: SSHAuth{Password: "pw"}}, closed
}

func TestAccessInspectionSessionOpenIsBounded(t *testing.T) {
	for _, reason := range []string{messages.DoctorCancelled, messages.DoctorTimeout} {
		t.Run(reason, func(t *testing.T) {
			opened := make(chan struct{})
			req, closed := accessInspectionSSH(t, func(_ ssh.NewChannel, closed <-chan struct{}) {
				close(opened)
				<-closed // No channel-open confirmation or rejection.
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan AccessInspection, 1)
			go func() { done <- NewSSHInstaller(nil).InspectAccess(ctx, req) }()
			select {
			case <-opened:
			case <-time.After(3 * time.Second):
				t.Fatal("inspection did not reach channel-open")
			}
			if reason == messages.DoctorCancelled {
				cancel()
			}
			select {
			case got := <-done:
				if got.SSH != proto.CheckPass || got.Installation != proto.CheckSkip || got.Code != reason {
					t.Fatalf("inspection = %+v", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("inspection remained blocked in channel-open")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("inspection left the SSH connection open")
			}
		})
	}
}

func TestAccessInspectionPreservesCompletedChecksAfterTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, output, installation, process, connection string
	}{
		{"before installation", "", proto.CheckSkip, proto.CheckSkip, ""},
		{"after installation", "GW_INSTALL passed\n", proto.CheckPass, proto.CheckSkip, ""},
		{"within process result", "GW_INSTALL passed\nGW_PROCESS passed", proto.CheckPass, proto.CheckSkip, ""},
		{"after process", "GW_INSTALL passed\nGW_PROCESS failed\n", proto.CheckPass, proto.CheckFail, ""},
		{"during HTTP", "GW_INSTALL passed\nGW_PROCESS passed\nGW_CONFIG state_file\nGW_ADDRESS http://server\n", proto.CheckPass, proto.CheckPass, ""},
		{"waiting for exit", "GW_INSTALL passed\nGW_PROCESS passed\nGW_CONFIG state_file\nGW_ADDRESS http://server\nGW_HEALTH 0 200\n", proto.CheckPass, proto.CheckPass, "http_ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			req, _ := accessInspectionSSH(t, func(newChannel ssh.NewChannel, closed <-chan struct{}) {
				channel, requests, err := newChannel.Accept()
				if err != nil {
					t.Errorf("accept session: %v", err)
					return
				}
				defer channel.Close()
				for request := range requests {
					if request.Type != "exec" {
						_ = request.Reply(false, nil)
						continue
					}
					_ = request.Reply(true, nil)
					token, err := io.ReadAll(channel)
					if err != nil || string(token) != "owned\n" {
						t.Errorf("unexpected inspection input: %v", err)
						return
					}
					if _, err := io.WriteString(channel, tc.output); err != nil {
						t.Errorf("write partial output: %v", err)
						return
					}
					close(started)
					<-closed // No exit status: the next remote step is stuck.
					return
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan AccessInspection, 1)
			go func() { done <- NewSSHInstaller(nil).InspectAccess(ctx, req) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("inspection did not receive partial output")
			}
			select {
			case got := <-done:
				if got.Code != messages.DoctorTimeout || got.Installation != tc.installation || got.Process != tc.process || got.Connection.Outcome != tc.connection {
					t.Fatalf("inspection = %+v", got)
				}
				if (got.Connection.Target != "") != (tc.connection != "") || got.Connection.ObservedAt.IsZero() != (tc.connection == "") {
					t.Fatalf("unfinished HTTP probe acquired evidence: %+v", got.Connection)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("inspection did not return after command timeout")
			}
		})
	}
}

func TestAccessInspectionPreservesCompletedChecksAfterCommandFailure(t *testing.T) {
	host, port := startFakeSSHServer(t, "tester", "pw", func(string, io.Reader) (string, string, uint32) {
		return "GW_INSTALL passed\nGW_PROCESS failed\n", "No space left on device", 1
	})
	got := NewSSHInstaller(nil).InspectAccess(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "tester", Token: "owned", ConnectMode: "server_dial", SSHAuth: SSHAuth{Password: "pw"}})
	if got.Installation != proto.CheckPass || got.Process != proto.CheckFail || got.Code != messages.DoctorInspectionUnavailable {
		t.Fatalf("completed results lost after remote command failure: %+v", got)
	}
}

func TestAccessInspectionRejectsMalformedPartialOutput(t *testing.T) {
	for _, raw := range []string{
		"GW_INSTALL passed", // An incomplete line is not a completed observation.
		"GW_PROCESS passed\n",
		"GW_INSTALL passed\nGW_INSTALL failed\n",
		"GW_INSTALL passed\nGW_PROCESS passed\nprivate-token\n",
		"GW_INSTALL passed\nGW_PROCESS passed\n\x00",
		"GW_INSTALL passed\n" + strings.Repeat("x", connectionDiagnosticLimit),
	} {
		out := parseAccessInspection(raw, AccessInspection{Installation: proto.CheckSkip, Process: proto.CheckSkip})
		if out.Installation != proto.CheckSkip || out.Process != proto.CheckSkip || out.Connection.Outcome != "" {
			t.Fatalf("malformed output accepted: %+v", out)
		}
	}
}

// A real SSH server refusing the password and a closed port: the inspection
// carries the SSH adapter's own code for each, not one catch-all.
func TestAccessInspectionNamesHowTheSSHConnectionFailed(t *testing.T) {
	host, port := startFakeSSHServer(t, "tester", "pw", func(string, io.Reader) (string, string, uint32) { return "", "", 0 })
	got := NewSSHInstaller(nil).InspectAccess(context.Background(), SSHProbeRequest{Host: host, Port: port, User: "tester", Token: "owned", ConnectMode: "server_dial", SSHAuth: SSHAuth{Password: "wrong"}})
	if got.SSH != proto.CheckFail || got.Code != messages.ErrorSSHAuthFailed {
		t.Fatalf("wrong password: %+v, want fail %s", got, messages.ErrorSSHAuthFailed)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	got = NewSSHInstaller(nil).InspectAccess(context.Background(), SSHProbeRequest{Host: "127.0.0.1", Port: closed, User: "tester", Token: "owned", ConnectMode: "server_dial", SSHAuth: SSHAuth{Password: "pw"}})
	if got.SSH != proto.CheckFail || got.Code != messages.ErrorSSHUnreachable {
		t.Fatalf("closed port: %+v, want fail %s", got, messages.ErrorSSHUnreachable)
	}
}
