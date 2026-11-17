package vast

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// keyLaggingSSHServer refuses the platform key on the first refuse logins,
// as a Vast container does until the key attached through the API reaches
// its authorized_keys, then accepts it and answers every command as an
// install that started the agent.
func keyLaggingSSHServer(t *testing.T, refuse int32) (string, int, *atomic.Int32) {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(hostKey)
	var logins atomic.Int32
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		if logins.Add(1) <= refuse {
			return nil, errors.New("not yet")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
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
			go serveCommands(conn, cfg)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port, &logins
}

func serveCommands(conn net.Conn, cfg *ssh.ServerConfig) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range requests {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				_, _ = ch.Write([]byte("gpuworks: agent started via systemd.\n"))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}()
	}
}

func platformKey(t *testing.T) *provider.PlatformKey {
	t.Helper()
	pub, private, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	return &provider.PlatformKey{PrivatePEM: string(pem.EncodeToMemory(block)), PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}
}

// A new instance refuses the platform key for a while after it was
// attached: the install with the agent connecting to the server logs in on
// a later try instead of failing the first time (todo T74).
func TestAgentDialInstallWaitsForTheAttachedKey(t *testing.T) {
	host, port, logins := keyLaggingSSHServer(t, 2)
	d := &Driver{keyAttachWait: 3 * time.Second}
	report, err := d.InstallAgentWithReport(context.Background(), provider.Credentials{APIKey: "k"}, provider.InstallRequest{
		Instance:  provider.Instance{ExternalID: "1", PublicIP: host, SSHPort: port, SSHUser: "root"},
		ServerURL: "https://server.example", Token: "t", ConnectMode: "agent_dial", PlatformKey: platformKey(t),
	})
	if err != nil {
		t.Fatalf("install failed: %v (logins %d)", err, logins.Load())
	}
	if logins.Load() < 3 {
		t.Fatalf("logins = %d, want the refused ones retried", logins.Load())
	}
	if first := report.Checks[0]; first.ID != "ssh_auth" || first.Status != "pass" || !strings.Contains(checkText(first), "platform key") {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

// A key that never takes is reported after the wait, as a connection error
// the service may try another way for.
func TestAgentDialInstallGivesUpWhenTheKeyNeverTakes(t *testing.T) {
	host, port, _ := keyLaggingSSHServer(t, 1000)
	d := &Driver{keyAttachWait: 300 * time.Millisecond}
	start := time.Now()
	_, err := d.InstallAgentWithReport(context.Background(), provider.Credentials{APIKey: "k"}, provider.InstallRequest{
		Instance:  provider.Instance{ExternalID: "1", PublicIP: host, SSHPort: port, SSHUser: "root"},
		ServerURL: "https://server.example", Token: "t", ConnectMode: "agent_dial", PlatformKey: platformKey(t),
	})
	if err == nil || !provider.IsRetryableSSHConnectionError(err) || !strings.Contains(err.Error(), "unable to authenticate") || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}
