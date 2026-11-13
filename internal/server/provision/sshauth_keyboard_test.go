package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// This server advertises keyboard-interactive only, as an sshd configured
// with PasswordAuthentication=no and KbdInteractiveAuthentication=yes does.
func startKeyboardAuthServer(t *testing.T, challenge func(ssh.ConnMetadata, ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error)) authServer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{KeyboardInteractiveCallback: challenge}
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
			go serveFakeSSHConn(conn, cfg, func(string, io.Reader) (string, string, uint32) {
				return "", "unexpected command in an authentication-only fixture", 1
			})
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return authServer{host: addr.IP.String(), port: addr.Port}
}

func passwordChallenge(meta ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	answers, err := challenge("SSH login", "", []string{"Password: "}, []bool{false})
	if err != nil {
		return nil, err
	}
	if meta.User() != "root" || len(answers) != 1 || answers[0] != "keyboard-secret" {
		return nil, errors.New("password rejected")
	}
	return nil, nil
}

func TestDialSSHKeyboardInteractivePassword(t *testing.T) {
	srv := startKeyboardAuthServer(t, passwordChallenge)
	_, outcome, err := dial(t, srv, SSHAuth{Mode: "keyboard_interactive", Password: "keyboard-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Used != "keyboard_interactive" || outcome.Check().Status != "ok" {
		t.Fatalf("outcome = %+v, check = %+v", outcome, outcome.Check())
	}
}

func TestDialSSHNoCompatibleMethodIsAnAuthenticationError(t *testing.T) {
	srv := startKeyboardAuthServer(t, passwordChallenge)
	_, outcome, err := dial(t, srv, SSHAuth{Mode: AuthPassword, Password: "keyboard-secret"})
	var authErr *SSHAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %T %v, want an actionable SSHAuthError", err, err)
	}
	if outcome.Used != "" || len(outcome.Tried) != 0 {
		t.Fatalf("an unsupported password method must not count as sent: %+v", outcome)
	}
	if !strings.Contains(err.Error(), "password") || !strings.Contains(err.Error(), "keyboard-interactive") {
		t.Fatalf("error lacks selected method or corrective guidance: %v", err)
	}
	if strings.Contains(err.Error(), "keyboard-secret") {
		t.Fatal("authentication error exposed the credential")
	}
}

func TestDialSSHStoredPasswordUsesKeyboardInteractive(t *testing.T) {
	srv := startKeyboardAuthServer(t, passwordChallenge)
	_, outcome, err := dial(t, srv, SSHAuth{Password: "keyboard-secret"})
	if err != nil || outcome.Used != AuthKeyboardInteractive {
		t.Fatalf("saved password recovery: outcome=%+v err=%v", outcome, err)
	}
	if got := strings.Join(outcome.Tried, ","); got != "keyboard-interactive password" {
		t.Fatalf("unsupported password protocol must not count as tried: %q", got)
	}
}

func TestDialSSHKeyboardInteractiveDoesNotFallBackToPassword(t *testing.T) {
	srv := startAuthServer(t, "root", "keyboard-secret")
	_, outcome, err := dial(t, srv, SSHAuth{Mode: AuthKeyboardInteractive, Password: "keyboard-secret"})
	var authErr *SSHAuthError
	if !errors.As(err, &authErr) || outcome.Used != "" || len(outcome.Tried) != 0 {
		t.Fatalf("explicit keyboard-interactive must not use password: outcome=%+v err=%v", outcome, err)
	}
}

func TestDialSSHKeyboardInteractiveRejection(t *testing.T) {
	srv := startKeyboardAuthServer(t, passwordChallenge)
	_, outcome, err := dial(t, srv, SSHAuth{Mode: AuthKeyboardInteractive, Password: "wrong-secret"})
	var authErr *SSHAuthError
	if !errors.As(err, &authErr) || outcome.Used != "" || len(outcome.Tried) != 1 {
		t.Fatalf("rejected password: outcome=%+v err=%v", outcome, err)
	}
	if strings.Contains(err.Error(), "wrong-secret") {
		t.Fatal("rejection exposed the password")
	}
}

func TestDialSSHKeyboardInteractiveInformationalChallenge(t *testing.T) {
	srv := startKeyboardAuthServer(t, func(meta ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
		if _, err := challenge("SSH login", "Password authentication", nil, nil); err != nil {
			return nil, err
		}
		return passwordChallenge(meta, challenge)
	})
	if _, _, err := dial(t, srv, SSHAuth{Mode: AuthKeyboardInteractive, Password: "keyboard-secret"}); err != nil {
		t.Fatal(err)
	}
}

func TestDialSSHKeyboardInteractiveAcceptedInformationalChallenge(t *testing.T) {
	srv := startKeyboardAuthServer(t, func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
		_, err := challenge("SSH login", "Authentication already satisfied", nil, nil)
		return nil, err
	})
	_, outcome, err := dial(t, srv, SSHAuth{Mode: AuthKeyboardInteractive, Password: "keyboard-secret"})
	if err != nil || outcome.Used != AuthKeyboardInteractive || outcome.Check().Status != "ok" {
		t.Fatalf("accepted keyboard-interactive must be reported as accepted: outcome=%+v err=%v", outcome, err)
	}
}

func TestDialSSHKeyboardInteractiveRefusesUnsupportedPrompts(t *testing.T) {
	for _, name := range []string{"multiple", "echo", "second-password", "too-many-informational"} {
		t.Run(name, func(t *testing.T) {
			answered := make(chan bool, 1)
			srv := startKeyboardAuthServer(t, func(meta ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
				questions, echos := []string{"Password: ", "OTP: "}, []bool{false, false}
				switch name {
				case "echo":
					questions, echos = []string{"Password: "}, []bool{true}
				case "second-password":
					if _, err := passwordChallenge(meta, challenge); err != nil {
						return nil, err
					}
					questions, echos = []string{"OTP: "}, []bool{false}
				case "too-many-informational":
					for range 4 {
						if _, err := challenge("SSH login", "Wait", nil, nil); err != nil {
							return nil, err
						}
					}
					questions, echos = nil, nil
				}
				answers, err := challenge("SSH login", "", questions, echos)
				answered <- err == nil || len(answers) != 0
				return nil, err
			})
			_, _, err := dial(t, srv, SSHAuth{Mode: AuthKeyboardInteractive, Password: "keyboard-secret"})
			var authErr *SSHAuthError
			if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "use an SSH key") {
				t.Fatalf("expected actionable unsupported-challenge error, got %v", err)
			}
			select {
			case replied := <-answered:
				if replied {
					t.Fatal("client answered an unsupported challenge")
				}
			case <-time.After(time.Second):
				t.Fatal("server was left waiting after the client rejected a challenge")
			}
		})
	}
}

func TestDialSSHTransportFailureIsNotAnAuthenticationError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := DialSSH(ctx, "root", "127.0.0.1", 22, SSHAuth{Mode: AuthPassword, Password: "pw"}, time.Second, nil)
	var authErr *SSHAuthError
	if err == nil || errors.As(err, &authErr) {
		t.Fatalf("transport failure mislabeled as authentication: %v", err)
	}
}
