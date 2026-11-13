package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func shCmd(script string) func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "sh", "-c", script) }
}

// Calls under one key take turns: two callers arriving together never have
// two processes on the driver at once.
func TestRunTrackedCallsUnderOneKeyTakeTurns(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "running")
	script := `if [ -e "` + marker + `" ]; then echo overlap; exit 1; fi; touch "` + marker + `"; sleep 0.2; rm "` + marker + `"; echo ok`
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := RunTracked(t.Context(), "gw-test-turns", 5*time.Second, shCmd(script))
			if err != nil || strings.TrimSpace(out) != "ok" {
				errs <- errors.New(out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("calls overlapped: %v", err)
	}
}

// Once a call under a key is stuck past its kill, every other command under
// the same key is refused without starting, while other keys run on.
func TestRunTrackedStuckKeyRefusesEveryCommandUnderIt(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is needed to keep a process out of the killed group")
	}
	shortVendorCLITimeouts(t, time.Second, 50*time.Millisecond)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	// Wait for the escaped child to own the pipe before canceling its parent.
	// A 100 ms wall-clock delay can kill the shell before setsid even starts on
	// a loaded/race host; that never establishes the stuck-process precondition.
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := RunTracked(ctx, "gw-test-stuck", 10*time.Second, func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", `setsid sh -c ': > "$1"; sleep 2' escaped "$1" & wait`, "parent", ready)
		})
		done <- err
	}()
	// The escaped child has a finite lifetime even if this test fails.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("tracked child was not reaped")
		}
	})
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatal("escaped child did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled tracked call did not return")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first call err = %v", err)
	}
	_, err = RunTracked(t.Context(), "gw-test-stuck", time.Second, shCmd(`touch "`+started+`"`))
	if !errors.Is(err, ErrStillRunning) {
		t.Fatalf("second command err = %v, want refused", err)
	}
	if _, statErr := os.Stat(started); statErr == nil {
		t.Fatal("a command was started under a stuck key")
	}
	if out, err := RunTracked(t.Context(), "gw-test-other", time.Second, shCmd("echo free")); err != nil || strings.TrimSpace(out) != "free" {
		t.Fatalf("another key = %q %v", out, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := RunTracked(t.Context(), "gw-test-stuck", time.Second, shCmd("true"))
		if err == nil {
			break
		}
		if !errors.Is(err, ErrStillRunning) || time.Now().After(deadline) {
			t.Fatalf("key never recovered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A runtime check refused because an earlier one is stuck is the host's
// answer, never the card's, for every vendor's verdict.
func TestARefusedRuntimeCheckIsNotTheCards(t *testing.T) {
	refused := errors.Join(ErrStillRunning)
	for name, verdict := range map[string]error{
		"cuda":   cudaSmokeVerdict(nil, refused),
		"ascend": ascendSmokeVerdict(nil, refused),
		"hygon":  hygonSmokeVerdict(nil, refused),
	} {
		if HostRuntimeUnavailable(verdict) == "" {
			t.Errorf("%s: %v, want host-level", name, verdict)
		}
	}
}

func TestHCCNKeyIsTheDie(t *testing.T) {
	for args, want := range map[string]string{
		"-i 3 -ip -g":             "hccn_tool -i 3",
		"-i 3 -ping -g address 1": "hccn_tool -i 3",
		"-h":                      "hccn_tool",
	} {
		if got := hccnKey(strings.Fields(args)); got != want {
			t.Errorf("hccnKey(%q) = %q, want %q", args, got, want)
		}
	}
}
