package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/selflog"
)

// backgroundLoopMarkers are substrings of a goroutine's stack trace that
// uniquely identify one of the loops Run's bg group tracks (see app.go and
// shutdown.go's backgroundGroup). They are the fully qualified function
// names rather than bare method names so they cannot collide with an
// unrelated goroutine elsewhere in the test binary.
func backgroundLoopMarkers() []string {
	return []string{
		"internal/server/app.(*App).staleLoop(",
		"internal/server/app.(*App).eventRetentionLoop(",
		"internal/server/app.(*App).kubeNodeLoop(",
		"internal/server/hub.(*Hub).RunServerDialReconnect(",
		"internal/server/hub.(*Hub).RunPeerTransferMetrics(",
		"internal/server/recovery.(*Recoverer).Run(",
		"internal/server/controller.(*Controller).Run(",
		"internal/server/app.runPDVerifier(",
		"internal/server/app.(*opsStats).run(",
		"internal/server/app.(*observability).logIngestLoop(",
		"internal/server/app.(*observability).selfLogLoop(",
	}
}

// stacks dumps every goroutine's stack as text, growing the buffer until the
// whole dump fits.
func stacks() string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, len(buf)*2)
	}
}

// countGoroutines returns how many currently running goroutines have marker
// anywhere in their stack trace (each goroutine contributes at most once:
// marker is a function name, which appears at most once per goroutine's own
// frame list in a non-recursive loop).
func countGoroutines(marker string) int {
	return strings.Count(stacks(), marker)
}

// waitForGoroutineCount polls (no fixed sleep) until marker's goroutine count
// reaches at least want, or fails the test after timeout. Used to prove a
// loop is actually running before we ask it to stop - so a later "it's gone"
// assertion cannot pass vacuously because the marker never matched anything.
func waitForGoroutineCount(t *testing.T, marker string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if n := countGoroutines(marker); n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for >= %d goroutine(s) matching %q (have %d)", want, marker, countGoroutines(marker))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startShutdownTestApp builds an App bound to an ephemeral loopback port and
// starts Run in the background, returning once the listener is up (so every
// loop Run starts has had a chance to launch).
func startShutdownTestApp(t *testing.T) (a *App, ctx context.Context, cancel context.CancelFunc, done chan error) {
	t.Helper()
	dir := t.TempDir()
	var err error
	// Start from DefaultConfig rather than a bare Config{} literal: recovery.Run
	// returns immediately when RecoveryEnabled is false (its zero value), which
	// would make the recovery loop marker below never match anything -
	// production always runs with the real defaults applied. selfLogLoop has
	// the same shape (returns immediately when SelfLog is nil), so wire in a
	// real collector too.
	cfg := DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.DataDir = dir
	cfg.DBPath = filepath.Join(dir, "gpuworks.db")
	cfg.AdminUser = "admin"
	cfg.AdminPassword = "gpuworks-test-password-2026"
	cfg.SelfLog = selflog.NewCollector(slog.NewTextHandler(io.Discard, nil), slog.LevelWarn, 16)
	a, err = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.Addr(); strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasSuffix(addr, ":0") {
			return a, ctx, cancel, done
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not bind")
	return nil, nil, nil, nil
}

// TestRunWaitsForAllBackgroundLoopsOnGracefulShutdown is T1: every loop Run
// starts must have actually returned by the time Run itself returns on a
// plain ctx cancel, with nothing left to poll for afterward.
func TestRunWaitsForAllBackgroundLoopsOnGracefulShutdown(t *testing.T) {
	markers := backgroundLoopMarkers()
	baseline := make(map[string]int, len(markers))
	for _, m := range markers {
		baseline[m] = countGoroutines(m)
	}
	_, _, cancel, done := startShutdownTestApp(t)
	// Prove each loop is actually running before cancel, so the "gone after
	// Run returns" assertion below cannot pass vacuously if a marker drifted
	// out of sync with the real function names.
	for _, m := range markers {
		waitForGoroutineCount(t, m, baseline[m]+1, 3*time.Second)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancel")
	}

	// Run has already returned: assert immediately, no polling. That is
	// exactly the property this fix adds - Run does not return while a loop
	// it started is still unwinding.
	for _, m := range markers {
		if n := countGoroutines(m); n > baseline[m] {
			t.Fatalf("goroutine matching %q is still running after Run returned (have %d, baseline %d)", m, n, baseline[m])
		}
	}
}

// TestRunWaitsForAllBackgroundLoopsOnListenerFailure is T2: when a listener's
// Serve fails and Run returns through the errc branch (not ctx.Done), every
// tracked loop must still be stopped and waited for - that branch used to
// return immediately with no cancellation or wait at all.
func TestRunWaitsForAllBackgroundLoopsOnListenerFailure(t *testing.T) {
	markers := backgroundLoopMarkers()
	baseline := make(map[string]int, len(markers))
	for _, m := range markers {
		baseline[m] = countGoroutines(m)
	}
	a, _, cancel, done := startShutdownTestApp(t)
	defer cancel()
	for _, m := range markers {
		waitForGoroutineCount(t, m, baseline[m]+1, 3*time.Second)
	}

	// Force a listener failure: close the bound socket out from under
	// http.Serve so it reports an error that is not http.ErrServerClosed,
	// driving Run's `case err := <-errc` branch instead of ctx.Done.
	a.lnMu.RLock()
	lns := append([]*trackingListener(nil), a.lns...)
	a.lnMu.RUnlock()
	for _, ln := range lns {
		_ = ln.Close()
	}

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of the listener failing")
	}
	if runErr == nil {
		t.Fatal("Run returned nil, want the listener's Serve error (errc branch not exercised)")
	}

	for _, m := range markers {
		if n := countGoroutines(m); n > baseline[m] {
			t.Fatalf("goroutine matching %q is still running after Run returned via errc (have %d, baseline %d)", m, n, baseline[m])
		}
	}
}

// TestRunRestartLeavesExactlyOneControllerLoop is T3: an App stopped and
// started again in-process (the self-restart path every embedder and the
// restart API relies on) must end up with exactly one controller Run
// goroutine while the second Run is active, and zero once it, too, is
// stopped - never two left over from the first Run.
func TestRunRestartLeavesExactlyOneControllerLoop(t *testing.T) {
	const marker = "internal/server/controller.(*Controller).Run("
	baseline := countGoroutines(marker)
	a, _, cancel1, done1 := startShutdownTestApp(t)
	waitForGoroutineCount(t, marker, baseline+1, 3*time.Second)

	cancel1()
	select {
	case err := <-done1:
		if err != nil {
			t.Fatalf("first Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first Run did not return")
	}
	if n := countGoroutines(marker); n > baseline {
		t.Fatalf("first Run left a controller Run goroutine running (have %d, baseline %d)", n, baseline)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- a.Run(ctx2) }()
	t.Cleanup(func() {
		cancel2()
		select {
		case <-done2:
		case <-time.After(10 * time.Second):
		}
	})

	waitForGoroutineCount(t, marker, baseline+1, 3*time.Second)
	if n := countGoroutines(marker); n != baseline+1 {
		t.Fatalf("second Run: controller Run goroutine count = %d, want exactly %d (no leftover from the first Run)", n, baseline+1)
	}

	cancel2()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("second Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second Run did not return")
	}
	if n := countGoroutines(marker); n > baseline {
		t.Fatalf("second Run left a controller Run goroutine running (have %d, baseline %d)", n, baseline)
	}
}
