// Package sshutil provides the SSH operations used by L4 cloud-GPU tests.
package sshutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/commandlog"
)

const (
	sshReadyTimeout      = 8 * time.Minute
	sshRetryDelay        = 5 * time.Second
	sshKeepaliveInterval = 20 * time.Second
)

type sshRequestSender interface {
	SendRequest(name string, wantReply bool, payload []byte) (bool, []byte, error)
}

func WaitSSHReady(ctx context.Context, host string, port int, privateKey []byte) error {
	return WaitSSHReadyAs(ctx, host, port, "root", privateKey)
}

func WaitSSHReadyAs(ctx context.Context, host string, port int, username string, privateKey []byte) error {
	return waitSSHReady(ctx, host, port, username, []ssh.AuthMethod{publicKeyAuth(privateKey)})
}

func WaitSSHReadyPasswordAs(ctx context.Context, host string, port int, username, password string) error {
	return waitSSHReady(ctx, host, port, username, []ssh.AuthMethod{ssh.Password(password)})
}

func waitSSHReady(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod) error {
	ctx, cancel := withMaxTimeout(ctx, sshReadyTimeout)
	defer cancel()
	var lastErr error
	for {
		client, err := dialAuth(ctx, host, port, username, auth)
		if err == nil {
			_, err = runContext(ctx, client, "uname -a")
			_ = client.Close()
			if err == nil {
				return nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for SSH readiness: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(sshRetryDelay):
		}
	}
}

func RunCommand(ctx context.Context, host string, port int, username string, privateKey []byte, command string) (string, error) {
	return runCommand(ctx, host, port, username, []ssh.AuthMethod{publicKeyAuth(privateKey)}, command)
}

func RunCommandPassword(ctx context.Context, host string, port int, username, password, command string) (string, error) {
	return runCommand(ctx, host, port, username, []ssh.AuthMethod{ssh.Password(password)}, command)
}

func runCommand(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod, command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("remote command is required")
	}
	client, err := dialAuth(ctx, host, port, username, auth)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return runContext(ctx, client, command)
}

// InstallAgent runs the same install.sh command shown to users. Token is
// variadic so callers that only want command validation get a useful error; a
// real installation must pass exactly one pending-node token.
func InstallAgent(ctx context.Context, host string, port int, privateKey []byte, serverAddr string, token ...string) error {
	if len(token) != 1 || strings.TrimSpace(token[0]) == "" {
		return errors.New("install agent: node token is required")
	}
	return InstallAgentAs(ctx, host, port, "root", privateKey, serverAddr, token[0])
}

func InstallAgentAs(ctx context.Context, host string, port int, username string, privateKey []byte, serverAddr, token string) error {
	return installAgent(ctx, host, port, username, []ssh.AuthMethod{publicKeyAuth(privateKey)}, serverAddr, token)
}

func InstallAgentPasswordAs(ctx context.Context, host string, port int, username, password, serverAddr, token string) error {
	return installAgent(ctx, host, port, username, []ssh.AuthMethod{ssh.Password(password)}, serverAddr, token)
}

func installAgent(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod, serverAddr, token string) error {
	serverAddr = strings.TrimRight(strings.TrimSpace(serverAddr), "/")
	if !strings.HasPrefix(serverAddr, "http://") && !strings.HasPrefix(serverAddr, "https://") {
		return fmt.Errorf("install agent: invalid server address %q", serverAddr)
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("install agent: node token is required")
	}
	client, err := dialAuth(ctx, host, port, username, auth)
	if err != nil {
		return err
	}
	defer client.Close()
	cmd := fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --token %s --server %s", shellQuote(serverAddr), shellQuote(token), shellQuote(serverAddr))
	out, err := runContext(ctx, client, cmd)
	if err != nil {
		return fmt.Errorf("install agent: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// maintainSSHConnection emits protocol-level traffic even when an agent has
// no control messages to send. Some cloud SSH gateways reap otherwise healthy
// forwarding sessions after roughly 15 minutes of perceived inactivity. A
// negative reply is harmless (the request is intentionally vendor-specific);
// a transport error closes the client so blocked Accept/Dial calls fail rather
// than leaving a dead tunnel looking usable.
func maintainSSHConnection(done <-chan struct{}, client *ssh.Client, interval time.Duration) {
	if err := sendSSHKeepalives(done, client, interval); err != nil {
		_ = client.Close()
	}
}

func sendSSHKeepalives(done <-chan struct{}, sender sshRequestSender, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("SSH keepalive interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-ticker.C:
			if _, _, err := sender.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return err
			}
		}
	}
}

func forwardTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("forward target is required")
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return "", fmt.Errorf("invalid forward target URL %q", raw)
		}
		raw = parsed.Host
	}
	if _, _, err := net.SplitHostPort(raw); err != nil {
		return "", fmt.Errorf("invalid forward target %q: %w", raw, err)
	}
	return raw, nil
}

func proxyPair(left, right net.Conn) {
	defer left.Close()
	defer right.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(left, right); done <- struct{}{} }()
	go func() { _, _ = io.Copy(right, left); done <- struct{}{} }()
	<-done
	// A request body commonly reaches EOF before the response is written. Do
	// not close both connections on that first completion or short HTTP
	// requests will be truncated as "empty reply" by the caller.
	<-done
}

func publicKeyAuth(privateKey []byte) ssh.AuthMethod {
	return ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		signer, err := ssh.ParsePrivateKey(privateKey)
		if err != nil {
			return nil, fmt.Errorf("parse SSH private key: %w", err)
		}
		return []ssh.Signer{signer}, nil
	})
}

func dialAuth(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod) (*ssh.Client, error) {
	if port <= 0 {
		port = 22
	}
	config := &ssh.ClientConfig{User: username, Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 15 * time.Second}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := dialTCP(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("dial SSH %s: %w", addr, err)
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake SSH %s: %w", addr, err)
	}
	return ssh.NewClient(sshConn, channels, requests), nil
}

type sessionOutput struct {
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func captureSessionOutput(session *ssh.Session) *sessionOutput {
	output := &sessionOutput{}
	// x/crypto/ssh copies the two streams in separate goroutines. A shared
	// bytes.Buffer is not safe for concurrent writes and has produced NUL-filled
	// L4 diagnostics on busy cloud nodes. Keep one writer per buffer and join
	// the streams only after Session.Run has completed.
	session.Stdout = &output.stdout
	session.Stderr = &output.stderr
	return output
}

func (o *sessionOutput) String() string {
	return o.stdout.String() + o.stderr.String()
}

// runContext closes the SSH transport when the caller's deadline is reached.
// ssh.Session.Run has no context parameter; without closing the client, a
// stalled apt/docker command can outlive the test timeout indefinitely.
func runContext(ctx context.Context, client *ssh.Client, command string) (string, error) {
	execution := commandlog.Start(command)
	session, err := client.NewSession()
	if err != nil {
		execution.Finish("", err)
		return "", err
	}
	defer session.Close()
	output := captureSessionOutput(session)
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		result := output.String()
		execution.Finish(result, err)
		return result, err
	case <-ctx.Done():
		_ = client.Close()
		err := <-done
		result := output.String()
		if err != nil {
			wrapped := fmt.Errorf("remote command canceled: %w: %v", ctx.Err(), err)
			execution.Finish(result, wrapped)
			return result, wrapped
		}
		execution.Finish(result, ctx.Err())
		return result, ctx.Err()
	}
}

func withMaxTimeout(parent context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= maximum {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, maximum)
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
