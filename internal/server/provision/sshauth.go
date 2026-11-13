package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH authentication modes. A request names the one the operator chose; an
// empty mode (cloud installs, stored recovery credentials) tries everything
// it has, platform key first.
const (
	AuthPlatformKey         = "platform_key"
	AuthPassword            = "password"
	AuthKeyboardInteractive = "keyboard_interactive"
	AuthPrivateKey          = "private_key"
	AuthInstancePassword    = "instance_password" // a password the cloud provider's API returned
)

// SSHAuth is how a request logs in. Every SSH path in this package (precheck,
// install, probe, restart, and cloud drivers through DialSSH) builds its
// attempts from it in one place, so the report of what was tried reads the
// same everywhere.
type SSHAuth struct {
	Mode          string `json:"auth_mode,omitempty"`
	Password      string `json:"password,omitempty"`
	PrivateKeyPEM string `json:"private_key_pem,omitempty"`
	// PlatformKey is the server's own key, loaded by the caller from the
	// store. Never part of a request body.
	PlatformKey *PlatformKey `json:"-"`
	// InstancePassword is the root password a cloud provider's API returned
	// for the instance.
	InstancePassword string `json:"-"`
}

type authAttempt struct {
	kind     string
	label    string
	signer   ssh.Signer // key attempts
	password string     // password attempts
}

// attempts lists what to try, in order. An explicit mode tries only that
// credential; the empty mode tries the platform key, then whatever else the
// request carries.
func (a SSHAuth) attempts() ([]authAttempt, error) {
	var out []authAttempt
	platform := func() error {
		if a.PlatformKey == nil || strings.TrimSpace(a.PlatformKey.PrivatePEM) == "" {
			return errors.New("the platform SSH key is not available")
		}
		signer, err := ssh.ParsePrivateKey([]byte(a.PlatformKey.PrivatePEM))
		if err != nil {
			return fmt.Errorf("parse platform key: %w", err)
		}
		out = append(out, authAttempt{kind: AuthPlatformKey, label: "platform key " + a.PlatformKey.Fingerprint, signer: signer})
		return nil
	}
	privateKey := func() error {
		signer, err := ssh.ParsePrivateKey([]byte(a.PrivateKeyPEM))
		if err != nil {
			return fmt.Errorf("parse private key: %w", err)
		}
		out = append(out, authAttempt{kind: AuthPrivateKey, label: "private key " + ssh.FingerprintSHA256(signer.PublicKey()), signer: signer})
		return nil
	}
	password := func(kind, label, value string) {
		out = append(out, authAttempt{kind: kind, label: label, password: value})
	}
	switch a.Mode {
	case AuthPlatformKey:
		if err := platform(); err != nil {
			return nil, err
		}
	case AuthPassword, AuthKeyboardInteractive:
		if a.Password == "" {
			return nil, errors.New("ssh password is required")
		}
		label := "password"
		if a.Mode == AuthKeyboardInteractive {
			label = "keyboard-interactive password"
		}
		password(a.Mode, label, a.Password)
	case AuthPrivateKey:
		if strings.TrimSpace(a.PrivateKeyPEM) == "" {
			return nil, errors.New("ssh private key is required")
		}
		if err := privateKey(); err != nil {
			return nil, err
		}
	case "":
		if a.PlatformKey != nil {
			if err := platform(); err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(a.PrivateKeyPEM) != "" {
			if err := privateKey(); err != nil {
				return nil, err
			}
		}
		if a.Password != "" {
			password(AuthPassword, "password", a.Password)
		}
		if a.InstancePassword != "" && a.InstancePassword != a.Password {
			password(AuthInstancePassword, "instance password", a.InstancePassword)
		}
		// Stored recovery credentials keep the password, not its SSH protocol.
		// Preserve the normal credential order, then try that password against
		// hosts whose sshd exposes password prompts only through PAM.
		if a.Password != "" {
			password(AuthKeyboardInteractive, "keyboard-interactive password", a.Password)
		}
		if len(out) == 0 {
			return nil, errors.New("no ssh credential: the platform key is not available and no password or private key was given")
		}
	default:
		return nil, fmt.Errorf("unknown ssh auth mode %q", a.Mode)
	}
	return out, nil
}

// Validate reports a request whose credentials cannot be used at all.
func (a SSHAuth) Validate() error {
	_, err := a.attempts()
	return err
}

// AuthOutcome records which attempts the server offered and which one the
// target accepted.
type AuthOutcome struct {
	Used  string   // an Auth* kind; "" when the handshake failed
	Tried []string // labels, in order
	// platformOnly: the platform key was the only thing tried, so a failure
	// means the target has not authorized it.
	platformOnly bool
}

// authRecorder turns the attempts into the methods the SSH client runs and
// records what happened. The client tries each method type once and skips any
// later method of a type it already tried, so all keys go into one publickey
// method and all passwords into one retryable password method, each in the
// attempts' order. A key counts as tried when the client asks for its public
// half and as used when it signs, which the client does only for a key the
// server accepted.
type authRecorder struct {
	mu       sync.Mutex
	attempts []authAttempt
	tried    []int
	last     int // the attempt that signed or answered an authentication challenge most recently
}

func newAuthRecorder(attempts []authAttempt) (*authRecorder, []ssh.AuthMethod) {
	r := &authRecorder{attempts: attempts, last: -1}
	var signers []ssh.Signer
	var passwords []int
	interactive := -1
	for i, attempt := range attempts {
		if attempt.signer != nil {
			signers = append(signers, newRecordingSigner(attempt.signer, func() { r.mark(i, false) }, func() { r.mark(i, true) }))
		} else if attempt.kind == AuthKeyboardInteractive {
			interactive = i
		} else {
			passwords = append(passwords, i)
		}
	}
	var methods []ssh.AuthMethod
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if len(passwords) > 0 {
		next := 0
		methods = append(methods, ssh.RetryableAuthMethod(ssh.PasswordCallback(func() (string, error) {
			if next >= len(passwords) {
				return "", errors.New("no more passwords")
			}
			i := passwords[next]
			next++
			r.mark(i, true)
			return attempts[i].password, nil
		}), len(passwords)))
	}
	if interactive >= 0 {
		answered := false
		rounds := 0
		methods = append(methods, ssh.KeyboardInteractive(func(_ string, _ string, questions []string, echos []bool) ([]string, error) {
			r.mark(interactive, false)
			rounds++
			if rounds > 4 {
				return nil, errors.New("keyboard-interactive exceeded the supported challenge limit; use an SSH key for multi-step authentication")
			}
			// PAM may send an informational challenge before its password prompt.
			if len(questions) == 0 && len(echos) == 0 {
				r.mark(interactive, true)
				return []string{}, nil
			}
			if answered || len(questions) != 1 || len(echos) != 1 || echos[0] {
				return nil, errors.New("keyboard-interactive requires a single non-echoing password prompt; use an SSH key for multi-step authentication")
			}
			answered = true
			r.mark(interactive, true)
			return []string{attempts[interactive].password}, nil
		}))
	}
	return r, methods
}

func (r *authRecorder) mark(i int, sent bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.tried, i) {
		r.tried = append(r.tried, i)
	}
	if sent {
		r.last = i
	}
}

func (r *authRecorder) outcome(ok bool) AuthOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out AuthOutcome
	for _, i := range r.tried {
		out.Tried = append(out.Tried, r.attempts[i].label)
	}
	if ok && r.last >= 0 {
		out.Used = r.attempts[r.last].kind
		// Report the accepted attempt last, after everything tried before it.
		out.Tried = append(slices.DeleteFunc(out.Tried, func(label string) bool { return label == r.attempts[r.last].label }), r.attempts[r.last].label)
	}
	out.platformOnly = len(r.attempts) == 1 && r.attempts[0].kind == AuthPlatformKey
	return out
}

// recordingSigner reports when the SSH client offers and when it signs with
// a key, keeping the wrapped signer's algorithm choices (RSA keys must still
// sign with rsa-sha2-*).
type recordingSigner struct {
	ssh.MultiAlgorithmSigner
	offered, signed func()
}

func newRecordingSigner(signer ssh.Signer, offered, signed func()) ssh.Signer {
	multi, ok := signer.(ssh.MultiAlgorithmSigner)
	if !ok {
		// ssh.ParsePrivateKey always returns one; anything else keeps its
		// default algorithm.
		if algo, isAlgo := signer.(ssh.AlgorithmSigner); isAlgo {
			if wrapped, err := ssh.NewSignerWithAlgorithms(algo, []string{signer.PublicKey().Type()}); err == nil {
				multi = wrapped
			}
		}
		if multi == nil {
			return signer
		}
	}
	return &recordingSigner{MultiAlgorithmSigner: multi, offered: offered, signed: signed}
}

func (s *recordingSigner) PublicKey() ssh.PublicKey {
	s.offered()
	return s.MultiAlgorithmSigner.PublicKey()
}

func (s *recordingSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	s.signed()
	return s.MultiAlgorithmSigner.Sign(rand, data)
}

func (s *recordingSigner) SignWithAlgorithm(rand io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	s.signed()
	return s.MultiAlgorithmSigner.SignWithAlgorithm(rand, data, algorithm)
}

// Check summarizes the attempts for a report.
func (o AuthOutcome) Check() CheckResult {
	if len(o.Tried) == 0 {
		return CheckResult{Name: "SSH authentication", Status: "error", Detail: "no credential was offered"}
	}
	steps := make([]string, len(o.Tried))
	for i, label := range o.Tried {
		verdict := "rejected"
		if i == len(o.Tried)-1 && o.Used != "" {
			verdict = "accepted"
		}
		steps[i] = label + " " + verdict
	}
	status := "ok"
	if o.Used == "" {
		status = "error"
	}
	return CheckResult{Name: "SSH authentication", Status: status, Detail: strings.Join(steps, " → ")}
}

// SSHAuthError is authentication that could not complete, with what was tried.
type SSHAuthError struct {
	Outcome AuthOutcome
	Err     error
	hint    string
}

func (e *SSHAuthError) Error() string {
	msg := e.Err.Error()
	if len(e.Outcome.Tried) > 0 {
		msg += " (tried: " + strings.Join(e.Outcome.Tried, ", ") + ")"
	}
	if e.hint != "" {
		msg += "; " + e.hint
	}
	return msg
}

func (e *SSHAuthError) Unwrap() error { return e.Err }

// DialSSH opens an SSH client with auth's attempts. hostKeys may be nil for a
// one-off connection to an endpoint whose host key is not tracked.
func DialSSH(ctx context.Context, user, host string, port int, auth SSHAuth, timeout time.Duration, hostKeys HostKeyStore) (*ssh.Client, AuthOutcome, error) {
	attempts, err := auth.attempts()
	if err != nil {
		return nil, AuthOutcome{}, err
	}
	recorder, methods := newAuthRecorder(attempts)
	client, err := connect(ctx, user, host, port, methods, timeout, hostKeys)
	outcome := recorder.outcome(err == nil)
	if err != nil {
		// x/crypto/ssh reports exhausted authentication as an untyped error.
		// No callback also means the peer offered none of our method types,
		// not just that the TCP/host-key handshake failed before authentication.
		if len(outcome.Tried) == 0 && !strings.Contains(err.Error(), "ssh: unable to authenticate, attempted methods ") {
			return nil, outcome, err // never got as far as authenticating
		}
		authErr := &SSHAuthError{Outcome: outcome, Err: err}
		if len(outcome.Tried) == 0 {
			labels := make([]string, len(attempts))
			for i, attempt := range attempts {
				labels[i] = attempt.label
			}
			authErr.hint = "no credential was offered with the configured SSH methods (" + strings.Join(labels, ", ") + "); check the target's SSH authentication settings, or select keyboard-interactive if it requires a password prompt"
		} else if outcome.platformOnly && auth.PlatformKey != nil {
			authErr.hint = "the target has not authorized the platform key; run on it: " + AuthorizeKeyCommand(auth.PlatformKey.PublicKey)
		}
		return nil, outcome, authErr
	}
	return client, outcome, nil
}

// AuthorizeKeyCommand is the one-liner an operator runs on a target to
// authorize publicKey for the user it runs as.
func AuthorizeKeyCommand(publicKey string) string {
	return "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo " + shellQuote(strings.TrimSpace(publicKey)) +
		" >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
}

// authorizeKeyScript appends publicKey to the login user's authorized_keys
// unless the key (compared without its comment) is already there, and prints
// "present <file>" or "added <file>".
func authorizeKeyScript(publicKey string) string {
	publicKey = strings.TrimSpace(publicKey)
	fields := strings.Fields(publicKey)
	match := publicKey
	if len(fields) >= 2 {
		match = fields[0] + " " + fields[1]
	}
	return fmt.Sprintf(`set -e
umask 077
dir="$HOME/.ssh"
file="$dir/authorized_keys"
mkdir -p "$dir"
touch "$file"
if grep -qF -- %s "$file"; then state=present; else printf '%%s\n' %s >> "$file"; state=added; fi
chmod 700 "$dir"
chmod 600 "$file"
echo "$state $file"`, shellQuote(match), shellQuote(publicKey))
}

// AuthorizePlatformKey makes sure the target accepts the platform key from
// now on. After logging in with the platform key there is nothing to do;
// after a password or another key it appends the platform public key. A
// failure is a warning: the install itself succeeded, and the caller keeps
// the credential that worked.
func AuthorizePlatformKey(ctx context.Context, client *ssh.Client, auth SSHAuth, outcome AuthOutcome) (CheckResult, bool) {
	check := CheckResult{Name: "Platform key"}
	if auth.PlatformKey == nil {
		check.Status, check.Detail = "warn", "the platform SSH key is not available; the credential that worked is kept"
		return check, false
	}
	if outcome.Used == AuthPlatformKey {
		check.Status, check.Detail = "ok", "authorized (used to log in)"
		return check, true
	}
	out, err := runContext(ctx, client, authorizeKeyScript(auth.PlatformKey.PublicKey))
	if err != nil {
		detail := strings.TrimSpace(out)
		if detail != "" {
			detail += ": "
		}
		check.Status, check.Detail = "warn", "could not add the platform key to authorized_keys: "+detail+err.Error()
		return check, false
	}
	state, file, _ := strings.Cut(strings.TrimSpace(lastLine(out)), " ")
	switch state {
	case "present":
		check.Status, check.Detail = "ok", "already in "+file
	case "added":
		check.Status, check.Detail = "ok", "added to "+file
	default:
		check.Status, check.Detail = "warn", "unexpected output while adding the platform key: "+strings.TrimSpace(out)
		return check, false
	}
	return check, true
}

// platformKeyPresentScript reports, without changing anything, whether the
// login user's authorized_keys already lists publicKey.
func platformKeyPresentScript(publicKey string) string {
	fields := strings.Fields(strings.TrimSpace(publicKey))
	match := strings.TrimSpace(publicKey)
	if len(fields) >= 2 {
		match = fields[0] + " " + fields[1]
	}
	return fmt.Sprintf(`file="$HOME/.ssh/authorized_keys"
if [ -f "$file" ] && grep -qF -- %s "$file"; then echo "present $file"; else echo "absent $file"; fi`, shellQuote(match))
}

// PlatformKeyPresence is the precheck's read-only view of the platform key:
// already authorized, or to be added by the install.
func PlatformKeyPresence(ctx context.Context, client *ssh.Client, publicKey string) CheckResult {
	check := CheckResult{Name: "Platform key", Status: "ok"}
	out, err := runContext(ctx, client, platformKeyPresentScript(publicKey))
	state, file, _ := strings.Cut(strings.TrimSpace(lastLine(out)), " ")
	switch {
	case err != nil:
		check.Status, check.Detail = "warn", "could not read authorized_keys: "+err.Error()
	case state == "present":
		check.Detail = "already in " + file
	default:
		check.Detail = "not in " + file + " yet; the install adds it"
	}
	return check
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
