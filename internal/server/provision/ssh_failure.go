package provision

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// This file is the one place the package names what went wrong on the far
// side of an SSH step. Errors from the network, from x/crypto/ssh and from
// commands run on the target are recognised here, by type wherever the
// library gives one and by text only where it does not, and turned into a
// Failure carrying a messages code. Callers read the code with FailureCode
// and never parse an error's text.

// Failure is a provisioning step that failed for a reason a code names. Its
// text is the underlying error's, kept as the English evidence.
type Failure struct {
	Code string
	Err  error
}

func (e *Failure) Error() string       { return e.Err.Error() }
func (e *Failure) Unwrap() error       { return e.Err }
func (e *Failure) FailureCode() string { return e.Code }

// coded is what FailureCode looks for: the outermost error in a chain that
// names its own code.
type coded interface{ FailureCode() string }

func failure(code string, err error) error {
	if err == nil {
		return nil
	}
	return &Failure{Code: code, Err: err}
}

// FailureCode is the code of err's failure: the one the step that failed
// named, or, for an error that reached the caller without one, the code its
// network or SSH cause carries. ok is false when nothing names it.
func FailureCode(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var named coded
	if errors.As(err, &named) && named.FailureCode() != "" {
		return named.FailureCode(), true
	}
	if code := causeCode(err); code != "" {
		return code, true
	}
	return "", false
}

// causeCode names a failure by the type of its network or SSH cause.
func causeCode(err error) string {
	var exit *ssh.ExitError
	var exitMissing *ssh.ExitMissingError
	var channel *ssh.OpenChannelError
	var dns *net.DNSError
	var op *net.OpError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.As(err, &exit), errors.As(err, &exitMissing):
		return messages.ErrorSSHCommandFailed
	case errors.As(err, &channel):
		return messages.ErrorSSHSessionFailed
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &timeout) && timeout.Timeout():
		return messages.ErrorSSHTimeout
	case errors.As(err, &dns), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH),
		errors.As(err, &op) && op.Op == "dial":
		return messages.ErrorSSHUnreachable
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed):
		return messages.ErrorSSHConnectionLost
	}
	return ""
}

// dialFailure names a failed TCP dial to the SSH endpoint.
func dialFailure(err error) error {
	if causeCode(err) == messages.ErrorSSHTimeout {
		return failure(messages.ErrorSSHTimeout, err)
	}
	return failure(messages.ErrorSSHUnreachable, err)
}

// handshakeFailure names a failed SSH handshake. Exhausted authentication is
// left unnamed here: DialSSH reports it as an SSHAuthError, which names it.
func handshakeFailure(err error) error {
	var closed *closedBeforeIdentError
	var mismatch *hostKeyMismatchError
	switch {
	case errors.As(err, &closed):
		return failure(messages.ErrorSSHClosedBeforeIdent, err)
	case errors.As(err, &mismatch):
		return failure(messages.ErrorSSHHostKeyMismatch, err)
	case authExhausted(err):
		return err
	case causeCode(err) == messages.ErrorSSHTimeout:
		return failure(messages.ErrorSSHTimeout, err)
	}
	return failure(messages.ErrorSSHHandshakeFailed, err)
}

// authExhausted reports x/crypto/ssh's "no method left" error, which the
// library returns untyped (ssh/client_auth.go), so its text is all there is.
func authExhausted(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ssh: unable to authenticate, attempted methods ")
}

// startFailure names a failed agent start from the install script's output:
// systemd reports a unit whose binary it could not execute as 203/EXEC, and
// the script's own check says "not executable"; anything else is a start
// failure without a narrower name.
func startFailure(out string, err error) error {
	lower := strings.ToLower(out)
	if strings.Contains(lower, "203/exec") || strings.Contains(lower, "not executable") {
		return failure(messages.ErrorAgentInstallBinaryMissing, err)
	}
	return failure(messages.ErrorAgentInstallStartFailed, err)
}

// hostKeyMismatchError is a target whose host key differs from the recorded one.
type hostKeyMismatchError struct{ endpoint string }

func (e *hostKeyMismatchError) Error() string { return "ssh host key mismatch for " + e.endpoint }
