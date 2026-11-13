package conn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// An in-place upgrade replaces the running binary and execs the new one. The
// new process starts unconfirmed: a marker next to the binary records when it
// was installed, and only a completed registration with the server confirms
// it. If the server has not connected (agent_dial) or connected back
// (server_dial) before the deadline, or the new binary keeps restarting
// without getting that far, the agent puts the previous binary back and execs
// it. A server_dial agent cannot tell "server unreachable" from "new binary
// broken", so it rolls back on either; the previous binary is known to work.

var upgradeMu sync.Mutex

const (
	// A self-upgrade may traverse a slow public tunnel. The context bounds the
	// complete download/smoke/install operation, while the transport limits only
	// connection setup and response-header stalls so a slow body can continue.
	selfUpgradeTimeout           = 10 * time.Minute
	upgradeConnectTimeout        = 60 * time.Second
	upgradeResponseHeaderTimeout = 60 * time.Second
	upgradeSmokeTimeout          = 10 * time.Second

	// upgradeMaxBinaryBytes bounds a pushed binary before any byte is written.
	upgradeMaxBinaryBytes = 1 << 30
	// upgradeMaxUnconfirmedStarts: a binary that crashes before registering
	// is restarted by systemd every few seconds; give up on it after this many
	// starts instead of waiting out the whole deadline.
	upgradeMaxUnconfirmedStarts = 3

	upgradePendingSuffix    = ".upgrade-pending"
	upgradeRolledBackSuffix = ".upgrade-rolled-back"
)

// upgradeConfirmTimeout covers the server_dial reconnect backoff (at most
// five minutes) plus agent startup. Tests shorten it.
var upgradeConfirmTimeout = 6 * time.Minute

// execFn is syscall.Exec; tests replace it to observe the hand-over.
var execFn = syscall.Exec

// upgradePending is the JSON in <binary>.upgrade-pending.
type upgradePending struct {
	Deadline time.Time `json:"deadline"`
	Starts   int       `json:"starts"`
}

func (c *Conn) handleUpgradeRPC(id uint64) {
	// The control-plane call must complete before the long-running download
	// and exec begins, otherwise the server-side RPC times out while waiting
	// for a process that is intentionally replacing itself.
	if !upgradeMu.TryLock() {
		_ = c.send(proto.TRPCResp, id, rpcRespBody{Error: "agent upgrade already in progress"})
		return
	}
	_ = c.send(proto.TRPCResp, id, rpcRespBody{Result: map[string]string{"state": "upgrading"}})
	go func() {
		defer upgradeMu.Unlock()
		if err := c.selfUpgrade(context.Background()); err != nil {
			c.reportUpgradeFailure(err)
		}
	}()
}

// reportUpgradeFailure sends the failure as a separate event: the RPC that
// started the upgrade has already been answered, and a pushed upgrade's
// stream may be gone by the time install fails.
func (c *Conn) reportUpgradeFailure(err error) {
	c.log.Error("agent self-upgrade failed", "err", err)
	_ = c.send(proto.TAgentUpgradeStatus, 0, proto.AgentUpgradeStatus{State: "failed", Error: err.Error()})
}

// upgradeSupported: in-place replacement relies on exec over a renamed file,
// and on a supervisor (systemd, launchd's KeepAlive) restarting a binary that
// dies before it is confirmed. Renaming rather than overwriting also keeps a
// Mac's code-signature cache from killing the new binary.
func upgradeSupported() bool { return runtime.GOOS == "linux" || runtime.GOOS == "darwin" }

func (c *Conn) binaryPath() (string, error) {
	bin := c.creds.BinaryPath
	if bin == "" {
		var err error
		bin, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	for strings.HasSuffix(bin, ".old") {
		bin = strings.TrimSuffix(bin, ".old")
	}
	return bin, nil
}

func (c *Conn) selfUpgrade(ctx context.Context) error {
	if strings.TrimSpace(c.creds.ServerURL) == "" || strings.TrimSpace(c.creds.NodeToken) == "" {
		return errors.New("self-upgrade requires server URL and node token")
	}
	if !upgradeSupported() {
		return fmt.Errorf("self-upgrade is only supported on linux and macOS")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > selfUpgradeTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, selfUpgradeTimeout)
		defer cancel()
	}
	bin, err := c.binaryPath()
	if err != nil {
		return err
	}
	tmp := bin + ".new"
	defer os.Remove(tmp)
	if err := downloadAndSmoke(ctx, tmp, c.creds.ServerURL, c.creds.NodeToken); err != nil {
		return err
	}
	// A manual `upgrade` invocation must resume the long-lived agent loop after
	// replacement; the RPC path already runs under `run` and can preserve its
	// original argv verbatim.
	return installAndExec(bin, tmp, replacementExecArgs(bin, c.creds, os.Args))
}

// handleUpgradePush receives a binary pushed on a StreamAgentUpgrade stream,
// verifies it, answers on the stream, and installs it. The server closes its
// side after reading the answer; waiting for that close before exec keeps the
// answer from being lost in yamux buffers when the process image is replaced.
func (c *Conn) handleUpgradePush(hdr proto.StreamHeader, stream net.Conn) {
	defer stream.Close()
	reply := func(status proto.AgentUpgradeStatus) {
		raw, _ := json.Marshal(status)
		_, _ = stream.Write(append(raw, '\n'))
	}
	if !upgradeSupported() {
		reply(proto.AgentUpgradeStatus{State: "failed", Error: "self-upgrade is only supported on linux and macOS"})
		return
	}
	if !upgradeMu.TryLock() {
		reply(proto.AgentUpgradeStatus{State: "failed", Error: "agent upgrade already in progress"})
		return
	}
	defer upgradeMu.Unlock()
	bin, err := c.binaryPath()
	if err != nil {
		reply(proto.AgentUpgradeStatus{State: "failed", Error: err.Error()})
		return
	}
	tmp := bin + ".new"
	defer os.Remove(tmp)
	if err := receiveBinary(stream, tmp, hdr.Size, hdr.Ref); err != nil {
		c.log.Error("pushed agent upgrade rejected", "err", err)
		reply(proto.AgentUpgradeStatus{State: "failed", Error: err.Error()})
		return
	}
	reply(proto.AgentUpgradeStatus{State: "verified"})
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.Copy(io.Discard, stream)
	if err := installAndExec(bin, tmp, os.Args); err != nil {
		c.reportUpgradeFailure(err)
	}
}

// receiveBinary writes exactly size bytes to tmp and checks them against the
// sha256 the server announced and by running the binary once.
func receiveBinary(r io.Reader, tmp string, size int64, wantSHA string) error {
	if size <= 0 || size > upgradeMaxBinaryBytes {
		return fmt.Errorf("pushed agent binary size %d is out of range", size)
	}
	wantSHA = strings.ToLower(strings.TrimSpace(wantSHA))
	if len(wantSHA) != sha256.Size*2 {
		return fmt.Errorf("pushed agent binary has no valid sha256")
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	sum := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(f, sum), r, size)
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("receive agent binary: %d of %d bytes: %w", n, size, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != wantSHA {
		return fmt.Errorf("agent binary sha256 %s does not match announced %s", got, wantSHA)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	// The pull path smoke-tests by dialing the server, which a server_dial
	// node cannot do; running the binary proves it is executable here.
	ctx, cancel := context.WithTimeout(context.Background(), upgradeSmokeTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, tmp, "version").CombinedOutput(); err != nil {
		return fmt.Errorf("new agent binary does not run: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installAndExec swaps tmp in for bin, keeping the previous binary as
// bin.old, marks the upgrade unconfirmed, and execs the new binary.
func installAndExec(bin, tmp string, args []string) error {
	old := bin + ".old"
	_ = os.Remove(old)
	if err := os.Rename(bin, old); err != nil {
		return fmt.Errorf("backup old agent: %w", err)
	}
	if err := os.Rename(tmp, bin); err != nil {
		_ = os.Rename(old, bin)
		return fmt.Errorf("install new agent: %w", err)
	}
	if err := writeUpgradePending(bin, upgradePending{Deadline: time.Now().Add(upgradeConfirmTimeout)}); err != nil {
		_ = os.Rename(old, bin)
		return fmt.Errorf("mark upgrade pending: %w", err)
	}
	_ = os.Remove(bin + upgradeRolledBackSuffix)
	if err := execFn(bin, args, os.Environ()); err != nil {
		_ = os.Remove(bin + upgradePendingSuffix)
		_ = os.Remove(bin)
		_ = os.Rename(old, bin)
		return fmt.Errorf("exec new agent: %w", err)
	}
	return nil
}

func writeUpgradePending(bin string, p upgradePending) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := bin + upgradePendingSuffix + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, bin+upgradePendingSuffix)
}

// UpgradeGuard confirms or rolls back an upgrade that installed the running
// binary. The zero value (no upgrade pending) does nothing.
type UpgradeGuard struct {
	bin        string
	log        *slog.Logger
	mu         sync.Mutex
	pending    bool
	done       bool
	timer      *time.Timer
	rolledBack string // why the previous binary was abandoned, to report once
}

var activeGuard struct {
	sync.Mutex
	g *UpgradeGuard
}

// StartUpgradeGuard is called once when `run` starts. With an upgrade
// pending it counts this start and arms the deadline; a pending upgrade past
// its deadline or start budget is rolled back immediately.
func StartUpgradeGuard(log *slog.Logger) *UpgradeGuard {
	bin, err := os.Executable()
	if err != nil {
		return &UpgradeGuard{log: log}
	}
	return startUpgradeGuard(bin, log, time.Now())
}

func startUpgradeGuard(bin string, log *slog.Logger, now time.Time) *UpgradeGuard {
	g := &UpgradeGuard{bin: bin, log: log}
	if note, err := os.ReadFile(bin + upgradeRolledBackSuffix); err == nil {
		g.rolledBack = strings.TrimSpace(string(note))
	}
	activeGuard.Lock()
	activeGuard.g = g
	activeGuard.Unlock()

	raw, err := os.ReadFile(bin + upgradePendingSuffix)
	if err != nil {
		return g
	}
	var p upgradePending
	if err := json.Unmarshal(raw, &p); err != nil || p.Deadline.IsZero() {
		log.Warn("discarding unreadable upgrade marker", "path", bin+upgradePendingSuffix, "err", err)
		_ = os.Remove(bin + upgradePendingSuffix)
		return g
	}
	g.pending = true
	p.Starts++
	switch {
	case p.Starts > upgradeMaxUnconfirmedStarts:
		g.rollback(fmt.Sprintf("new agent binary restarted %d times without registering with the server", p.Starts-1))
		return g
	case !now.Before(p.Deadline):
		g.rollback("new agent binary was not connected to the server before the upgrade deadline")
		return g
	}
	if err := writeUpgradePending(bin, p); err != nil {
		log.Warn("update upgrade marker", "err", err)
	}
	log.Info("upgraded agent awaiting confirmation", "deadline", p.Deadline, "start", p.Starts)
	g.mu.Lock()
	g.timer = time.AfterFunc(p.Deadline.Sub(now), func() {
		g.rollback("new agent binary was not connected to the server before the upgrade deadline")
	})
	g.mu.Unlock()
	return g
}

// confirm marks the running binary good and returns a rollback note left by
// an earlier attempt, if any, so it is reported exactly once.
func (g *UpgradeGuard) confirm() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending && !g.done {
		g.done = true
		if g.timer != nil {
			g.timer.Stop()
		}
		_ = os.Remove(g.bin + upgradePendingSuffix)
		g.log.Info("agent upgrade confirmed")
	}
	note := g.rolledBack
	if note != "" {
		g.rolledBack = ""
		_ = os.Remove(g.bin + upgradeRolledBackSuffix)
	}
	return note
}

// rollback restores bin.old over the unconfirmed binary and execs it with the
// same arguments.
func (g *UpgradeGuard) rollback(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return
	}
	g.done = true
	_ = os.Remove(g.bin + upgradePendingSuffix)
	old := g.bin + ".old"
	if _, err := os.Stat(old); err != nil {
		g.log.Error("agent upgrade unconfirmed but no previous binary to roll back to", "reason", reason, "err", err)
		return
	}
	g.log.Error("rolling back agent upgrade", "reason", reason)
	_ = os.WriteFile(g.bin+upgradeRolledBackSuffix, []byte(reason), 0o600)
	if err := os.Rename(old, g.bin); err != nil {
		g.log.Error("restore previous agent binary", "err", err)
		return
	}
	if err := execFn(g.bin, os.Args, os.Environ()); err != nil {
		g.log.Error("exec previous agent binary", "err", err)
	}
}

// confirmUpgrade runs after a registered handshake: the server can reach
// this binary, so a pending upgrade is good. A rollback note from an
// abandoned upgrade is reported as an upgrade failure.
func (c *Conn) confirmUpgrade() {
	activeGuard.Lock()
	g := activeGuard.g
	activeGuard.Unlock()
	if g == nil {
		return
	}
	if note := g.confirm(); note != "" {
		_ = c.send(proto.TAgentUpgradeStatus, 0, proto.AgentUpgradeStatus{State: "failed", Error: "rolled back: " + note})
	}
}

// downloadAndSmoke performs every pre-install step of a pulled upgrade.
// Keeping it separate from the final rename/exec makes the failure boundary
// explicit and allows hermetic tests to prove that a bad artifact never
// replaces the currently running binary.
func downloadAndSmoke(ctx context.Context, tmp, serverURL, token string) error {
	name := proto.AgentBinaryName(runtime.GOOS, runtime.GOARCH)
	dlURL := strings.TrimRight(serverURL, "/") + "/dl/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return err
	}
	client := upgradeHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("download agent: http %s", resp.Status)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	smokeCtx, cancel := context.WithTimeout(ctx, upgradeSmokeTimeout)
	defer cancel()
	cmd := exec.CommandContext(smokeCtx, tmp, "smoke", "--server", serverURL, "--token", token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("smoke test failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func upgradeHTTPClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}
	clone := transport.Clone()
	dialer := &net.Dialer{Timeout: upgradeConnectTimeout, KeepAlive: 30 * time.Second}
	clone.DialContext = dialer.DialContext
	clone.TLSHandshakeTimeout = upgradeConnectTimeout
	clone.ResponseHeaderTimeout = upgradeResponseHeaderTimeout
	return &http.Client{Transport: clone}
}

func replacementExecArgs(bin string, creds Credentials, current []string) []string {
	if len(current) <= 1 || current[1] != "upgrade" {
		return current
	}
	return append([]string{bin, "run"}, creds.ResumeArgs...)
}

// Upgrade downloads, smoke-tests, atomically installs, and execs the current
// agent binary. It is used by the manual upgrade subcommand as well as the RPC
// path.
func Upgrade(ctx context.Context, serverURL, token string, log *slog.Logger) error {
	if !upgradeMu.TryLock() {
		return errors.New("agent upgrade already in progress")
	}
	defer upgradeMu.Unlock()
	c := New(Credentials{ServerURL: serverURL, NodeToken: token}, nil, nil, nil, log)
	return c.selfUpgrade(ctx)
}

// UpgradeWithResumeArgs is the manual-upgrade entry point. resumeArgs are
// the arguments of the replacement `run` — `--state DIR` for an agent whose
// configuration is in its state directory — so an in-place upgrade does not
// switch a server_dial agent back to agent_dial or lose its state directory
// or driver.
func UpgradeWithResumeArgs(ctx context.Context, serverURL, token string, resumeArgs []string, log *slog.Logger) error {
	if !upgradeMu.TryLock() {
		return errors.New("agent upgrade already in progress")
	}
	defer upgradeMu.Unlock()
	c := New(Credentials{ServerURL: serverURL, NodeToken: token, ResumeArgs: append([]string(nil), resumeArgs...)}, nil, nil, nil, log)
	return c.selfUpgrade(ctx)
}

// AlignWithServer replaces the running binary with the server's own agent
// binary when the two differ, and execs it; it returns nil without doing
// anything when they are the same. A Kubernetes agent freshly seeded from
// its image runs whatever agent the image carried, so it takes the server's
// once before it registers (internal/agent/kubeboot). The replacement is
// an ordinary unconfirmed upgrade: if it never registers, the guard puts
// the image's binary back.
func AlignWithServer(ctx context.Context, serverURL, token string) error {
	if !upgradeMu.TryLock() {
		return errors.New("agent upgrade already in progress")
	}
	defer upgradeMu.Unlock()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > selfUpgradeTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, selfUpgradeTimeout)
		defer cancel()
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	return alignWithServer(ctx, bin, serverURL, token)
}

func alignWithServer(ctx context.Context, bin, serverURL, token string) error {
	tmp := bin + ".new"
	defer os.Remove(tmp)
	if err := downloadAndSmoke(ctx, tmp, serverURL, token); err != nil {
		return err
	}
	same, err := sameFile(bin, tmp)
	if err != nil || same {
		return err
	}
	return installAndExec(bin, tmp, os.Args)
}

func sameFile(a, b string) (bool, error) {
	sa, err := fileSum(a)
	if err != nil {
		return false, err
	}
	sb, err := fileSum(b)
	if err != nil {
		return false, err
	}
	return sa == sb, nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
