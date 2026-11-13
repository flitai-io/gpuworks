package conn

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A replacement binary that, like the real agent, exits 0 for `version`.
const goodAgentScript = "#!/bin/sh\n[ \"$1\" = version ] && echo gpuworks-agent-next\n"

type execCall struct {
	bin  string
	args []string
}

// execLog records exec hand-overs instead of replacing the test process.
type execLog struct {
	mu    sync.Mutex
	calls []execCall
}

func (l *execLog) snapshot() []execCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]execCall(nil), l.calls...)
}

func stubExec(t *testing.T, fail error) *execLog {
	t.Helper()
	log := &execLog{}
	prev := execFn
	execFn = func(bin string, args, _ []string) error {
		log.mu.Lock()
		defer log.mu.Unlock()
		log.calls = append(log.calls, execCall{bin: bin, args: append([]string(nil), args...)})
		return fail
	}
	t.Cleanup(func() { execFn = prev })
	return log
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// pushUpgrade plays the server side of a StreamAgentUpgrade stream: it sends
// payload, reads the agent's one-line answer, then closes like the hub does.
func pushUpgrade(t *testing.T, c *Conn, hdr proto.StreamHeader, payload string) proto.AgentUpgradeStatus {
	t.Helper()
	server, agent := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.handleUpgradePush(hdr, agent)
	}()
	go func() { _, _ = server.Write([]byte(payload)) }()
	line, err := bufio.NewReader(server).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read agent answer: %v", err)
	}
	_ = server.Close()
	<-done
	var status proto.AgentUpgradeStatus
	if err := json.Unmarshal(line, &status); err != nil {
		t.Fatalf("answer %q: %v", line, err)
	}
	return status
}

func TestPushedUpgradeInstallsVerifiedBinaryAndExecsIt(t *testing.T) {
	if !upgradeSupported() {
		t.Skip("in-place upgrade is linux-only")
	}
	calls := stubExec(t, nil)
	bin := filepath.Join(t.TempDir(), "gpuworks-agent")
	writeFile(t, bin, "old binary")
	c := &Conn{creds: Credentials{BinaryPath: bin}, ctrl: &nopRWC{}, log: slog.Default()}

	before := time.Now()
	status := pushUpgrade(t, c, proto.StreamHeader{Kind: proto.StreamAgentUpgrade, Size: int64(len(goodAgentScript)), Ref: sha(goodAgentScript)}, goodAgentScript)
	if status.State != "verified" || status.Error != "" {
		t.Fatalf("answer = %+v", status)
	}
	if got := readFile(t, bin); got != goodAgentScript {
		t.Fatalf("installed binary = %q", got)
	}
	if got := readFile(t, bin+".old"); got != "old binary" {
		t.Fatalf("previous binary = %q", got)
	}
	if exists(bin + ".new") {
		t.Fatal("staging file left behind")
	}
	var pending upgradePending
	if err := json.Unmarshal([]byte(readFile(t, bin+upgradePendingSuffix)), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Starts != 0 || pending.Deadline.Before(before.Add(upgradeConfirmTimeout)) || pending.Deadline.After(time.Now().Add(upgradeConfirmTimeout)) {
		t.Fatalf("pending marker = %+v", pending)
	}
	if len(calls.snapshot()) != 1 || calls.snapshot()[0].bin != bin || strings.Join(calls.snapshot()[0].args, " ") != strings.Join(os.Args, " ") {
		t.Fatalf("exec calls = %+v", calls.snapshot())
	}
}

func TestPushedUpgradeRejectsBadBinariesWithoutTouchingTheInstalledOne(t *testing.T) {
	if !upgradeSupported() {
		t.Skip("in-place upgrade is linux-only")
	}
	tests := []struct {
		name    string
		hdr     proto.StreamHeader
		payload string
		want    string
	}{
		{name: "digest mismatch", hdr: proto.StreamHeader{Size: int64(len(goodAgentScript)), Ref: sha("something else")}, payload: goodAgentScript, want: "does not match"},
		{name: "truncated stream", hdr: proto.StreamHeader{Size: int64(len(goodAgentScript)) + 10, Ref: sha(goodAgentScript)}, payload: goodAgentScript, want: "receive agent binary"},
		{name: "no size", hdr: proto.StreamHeader{Ref: sha(goodAgentScript)}, payload: goodAgentScript, want: "out of range"},
		{name: "oversized", hdr: proto.StreamHeader{Size: upgradeMaxBinaryBytes + 1, Ref: sha(goodAgentScript)}, payload: goodAgentScript, want: "out of range"},
		{name: "no digest", hdr: proto.StreamHeader{Size: int64(len(goodAgentScript))}, payload: goodAgentScript, want: "no valid sha256"},
		{name: "does not run", hdr: proto.StreamHeader{Size: int64(len("#!/bin/sh\nexit 3\n")), Ref: sha("#!/bin/sh\nexit 3\n")}, payload: "#!/bin/sh\nexit 3\n", want: "does not run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubExec(t, nil)
			bin := filepath.Join(t.TempDir(), "gpuworks-agent")
			writeFile(t, bin, "old binary")
			c := &Conn{creds: Credentials{BinaryPath: bin}, ctrl: &nopRWC{}, log: slog.Default()}
			tt.hdr.Kind = proto.StreamAgentUpgrade
			// A truncated push ends when the server closes; play that by
			// closing after the payload instead of waiting for an answer.
			if tt.name == "truncated stream" {
				server, agent := net.Pipe()
				done := make(chan struct{})
				go func() { defer close(done); c.handleUpgradePush(tt.hdr, agent) }()
				_, _ = server.Write([]byte(tt.payload))
				_ = server.Close()
				<-done
			} else {
				status := pushUpgrade(t, c, tt.hdr, tt.payload)
				if status.State != "failed" || !strings.Contains(status.Error, tt.want) {
					t.Fatalf("answer = %+v, want failure containing %q", status, tt.want)
				}
			}
			if got := readFile(t, bin); got != "old binary" {
				t.Fatalf("installed binary changed to %q", got)
			}
			for _, leftover := range []string{bin + ".new", bin + ".old", bin + upgradePendingSuffix} {
				if exists(leftover) {
					t.Fatalf("%s left behind", leftover)
				}
			}
			if len(calls.snapshot()) != 0 {
				t.Fatalf("exec calls = %+v", calls.snapshot())
			}
		})
	}
}

func TestPushedUpgradeRefusesWhileAnotherUpgradeRuns(t *testing.T) {
	if !upgradeSupported() {
		t.Skip("in-place upgrade is linux-only")
	}
	stubExec(t, nil)
	bin := filepath.Join(t.TempDir(), "gpuworks-agent")
	writeFile(t, bin, "old binary")
	c := &Conn{creds: Credentials{BinaryPath: bin}, ctrl: &nopRWC{}, log: slog.Default()}
	upgradeMu.Lock()
	status := pushUpgrade(t, c, proto.StreamHeader{Kind: proto.StreamAgentUpgrade, Size: int64(len(goodAgentScript)), Ref: sha(goodAgentScript)}, "")
	upgradeMu.Unlock()
	if status.State != "failed" || !strings.Contains(status.Error, "already in progress") {
		t.Fatalf("answer = %+v", status)
	}
}

func TestInstallAndExecRestoresPreviousBinaryWhenExecFails(t *testing.T) {
	stubExec(t, errors.New("exec format error"))
	dir := t.TempDir()
	bin := filepath.Join(dir, "gpuworks-agent")
	writeFile(t, bin, "old binary")
	writeFile(t, bin+".new", goodAgentScript)
	err := installAndExec(bin, bin+".new", []string{bin, "run"})
	if err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, bin); got != "old binary" {
		t.Fatalf("binary after failed exec = %q", got)
	}
	if exists(bin+upgradePendingSuffix) || exists(bin+".old") {
		t.Fatal("failed install left upgrade state behind")
	}
}

// installed lays out the files a successful installAndExec leaves: the new
// binary, the previous one, and an unconfirmed marker.
func installed(t *testing.T, p upgradePending) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gpuworks-agent")
	writeFile(t, bin, "new binary")
	writeFile(t, bin+".old", "old binary")
	if err := writeUpgradePending(bin, p); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestUpgradeGuardConfirmsOnRegistration(t *testing.T) {
	calls := stubExec(t, nil)
	now := time.Now()
	bin := installed(t, upgradePending{Deadline: now.Add(time.Hour)})
	g := startUpgradeGuard(bin, slog.Default(), now)

	var p upgradePending
	if err := json.Unmarshal([]byte(readFile(t, bin+upgradePendingSuffix)), &p); err != nil || p.Starts != 1 {
		t.Fatalf("marker after start = %+v err=%v", p, err)
	}
	ctrl := &nopRWC{}
	c := &Conn{ctrl: ctrl, log: slog.Default()}
	c.confirmUpgrade()
	if exists(bin + upgradePendingSuffix) {
		t.Fatal("confirmed upgrade kept its marker")
	}
	if got := readFile(t, bin); got != "new binary" {
		t.Fatalf("confirmed binary = %q", got)
	}
	if len(ctrl.BytesCopy()) != 0 {
		t.Fatalf("confirmation sent %q, want nothing", ctrl.BytesCopy())
	}
	g.mu.Lock()
	stopped := g.timer.Stop() // false: confirm already stopped it
	g.mu.Unlock()
	if stopped || len(calls.snapshot()) != 0 {
		t.Fatalf("timer still armed=%v exec calls=%+v", stopped, calls.snapshot())
	}
}

func TestUpgradeGuardRollsBackAndReportsOnce(t *testing.T) {
	tests := []struct {
		name    string
		pending upgradePending
		reason  string
	}{
		{name: "deadline passed", pending: upgradePending{Deadline: time.Now().Add(-time.Second)}, reason: "before the upgrade deadline"},
		{name: "crash loop", pending: upgradePending{Deadline: time.Now().Add(time.Hour), Starts: upgradeMaxUnconfirmedStarts}, reason: "restarted 3 times"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubExec(t, nil)
			bin := installed(t, tt.pending)
			startUpgradeGuard(bin, slog.Default(), time.Now())
			if got := readFile(t, bin); got != "old binary" {
				t.Fatalf("binary after rollback = %q", got)
			}
			if exists(bin+".old") || exists(bin+upgradePendingSuffix) {
				t.Fatal("rollback left upgrade state behind")
			}
			if len(calls.snapshot()) != 1 || calls.snapshot()[0].bin != bin {
				t.Fatalf("exec calls = %+v", calls.snapshot())
			}

			// The restored binary starts, registers, and reports why.
			startUpgradeGuard(bin, slog.Default(), time.Now())
			ctrl := &nopRWC{}
			c := &Conn{ctrl: ctrl, log: slog.Default()}
			c.confirmUpgrade()
			sent := string(ctrl.BytesCopy())
			if !strings.Contains(sent, `"t":"agent.upgrade.status"`) || !strings.Contains(sent, `"state":"failed"`) || !strings.Contains(sent, tt.reason) {
				t.Fatalf("report = %q", sent)
			}
			c.confirmUpgrade()
			if again := string(ctrl.BytesCopy()); again != sent {
				t.Fatalf("rollback reported twice: %q", again)
			}
		})
	}
}

func TestUpgradeGuardRollsBackWhenTheServerNeverConnects(t *testing.T) {
	calls := stubExec(t, nil)
	now := time.Now()
	bin := installed(t, upgradePending{Deadline: now.Add(30 * time.Millisecond)})
	startUpgradeGuard(bin, slog.Default(), now)
	deadline := time.Now().Add(5 * time.Second)
	for len(calls.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := readFile(t, bin); got != "old binary" {
		t.Fatalf("binary after deadline = %q", got)
	}
	if len(calls.snapshot()) != 1 {
		t.Fatalf("exec calls = %+v", calls.snapshot())
	}
	// A late registration cannot resurrect the abandoned binary.
	c := &Conn{ctrl: &nopRWC{}, log: slog.Default()}
	c.confirmUpgrade()
	if got := readFile(t, bin); got != "old binary" {
		t.Fatalf("binary after late confirm = %q", got)
	}
}

func TestUpgradeGuardWithoutPreviousBinaryOnlyDropsTheMarker(t *testing.T) {
	calls := stubExec(t, nil)
	bin := installed(t, upgradePending{Deadline: time.Now().Add(-time.Second)})
	if err := os.Remove(bin + ".old"); err != nil {
		t.Fatal(err)
	}
	startUpgradeGuard(bin, slog.Default(), time.Now())
	if exists(bin+upgradePendingSuffix) || len(calls.snapshot()) != 0 || readFile(t, bin) != "new binary" {
		t.Fatalf("marker=%v calls=%+v", exists(bin+upgradePendingSuffix), calls.snapshot())
	}
}

func TestUpgradeGuardIgnoresAbsentOrUnreadableMarkers(t *testing.T) {
	calls := stubExec(t, nil)
	bin := filepath.Join(t.TempDir(), "gpuworks-agent")
	writeFile(t, bin, "binary")
	startUpgradeGuard(bin, slog.Default(), time.Now())
	writeFile(t, bin+upgradePendingSuffix, "{not json")
	startUpgradeGuard(bin, slog.Default(), time.Now())
	if exists(bin+upgradePendingSuffix) || len(calls.snapshot()) != 0 {
		t.Fatalf("marker=%v calls=%+v", exists(bin+upgradePendingSuffix), calls.snapshot())
	}
}

// A Kubernetes agent freshly seeded from its image takes the server's binary
// once: it installs it as an unconfirmed upgrade and execs it when the two
// differ, and leaves everything alone when they are the same.
func TestAlignWithServerInstallsTheServersBinaryOnlyWhenItDiffers(t *testing.T) {
	const serverBinary = "#!/bin/sh\n[ \"$1\" = smoke ]\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(serverBinary))
	}))
	defer server.Close()

	t.Run("different", func(t *testing.T) {
		calls := stubExec(t, nil)
		bin := filepath.Join(t.TempDir(), "gpuworks-agent")
		writeFile(t, bin, "#!/bin/sh\n# the image's agent\n")
		if err := alignWithServer(context.Background(), bin, server.URL, "node-token"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(bin); string(got) != serverBinary {
			t.Fatalf("installed binary = %q, want the server's", got)
		}
		if old, _ := os.ReadFile(bin + ".old"); !strings.Contains(string(old), "the image's agent") {
			t.Fatalf("previous binary kept as .old = %q", old)
		}
		if _, err := os.Stat(bin + upgradePendingSuffix); err != nil {
			t.Fatalf("the replacement is not marked unconfirmed: %v", err)
		}
		if c := calls.snapshot(); len(c) != 1 || c[0].bin != bin {
			t.Fatalf("exec calls = %+v, want one of %s", c, bin)
		}
	})
	t.Run("same", func(t *testing.T) {
		calls := stubExec(t, nil)
		bin := filepath.Join(t.TempDir(), "gpuworks-agent")
		writeFile(t, bin, serverBinary)
		if err := alignWithServer(context.Background(), bin, server.URL, "node-token"); err != nil {
			t.Fatal(err)
		}
		if len(calls.snapshot()) != 0 {
			t.Fatal("re-executed although the binary is already the server's")
		}
		for _, leftover := range []string{".new", ".old", upgradePendingSuffix} {
			if _, err := os.Stat(bin + leftover); err == nil {
				t.Fatalf("%s left behind", leftover)
			}
		}
	})
	t.Run("server unreachable", func(t *testing.T) {
		calls := stubExec(t, nil)
		bin := filepath.Join(t.TempDir(), "gpuworks-agent")
		writeFile(t, bin, "#!/bin/sh\n")
		if err := alignWithServer(context.Background(), bin, "http://127.0.0.1:1", "node-token"); err == nil {
			t.Fatal("no error from an unreachable server")
		}
		if got, _ := os.ReadFile(bin); string(got) != "#!/bin/sh\n" || len(calls.snapshot()) != 0 {
			t.Fatal("the running binary was touched although nothing was downloaded")
		}
	})
}
