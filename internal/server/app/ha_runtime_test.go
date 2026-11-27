package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/agentbin"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
)

// recordingRuntime is a member runtime whose every hook can fail, so the
// tests below hold Run and Close to what each failure must do.
type recordingRuntime struct {
	mu     sync.Mutex
	events []string

	closed         bool
	closedErr      error
	startErr       error
	fail           chan error // sent on Run's failure channel after start
	beforeCloseErr error
	afterCloseErr  error
	refuse         bool // the admission handler answers 503
	controllerRan  chan struct{}
}

func (r *recordingRuntime) note(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingRuntime) seen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.events, ",")
}

func (r *recordingRuntime) runClosed(ctx context.Context, a *App) (bool, error) {
	if !r.closed {
		return false, nil
	}
	r.note("closed:bound=" + a.Addr())
	if r.closedErr != nil {
		return true, r.closedErr
	}
	<-ctx.Done()
	return true, nil
}

func (r *recordingRuntime) generation() bool { return false }

func (r *recordingRuntime) assemble(*App, *auth.TokenSigner, *packages.Store) error { return nil }

func (r *recordingRuntime) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.refuse {
			http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *recordingRuntime) start(ctx context.Context, a *App, failed chan<- error) (func(), error) {
	bound := a.Addr() != "" && !strings.HasSuffix(a.Addr(), ":0")
	r.note("start:bound=" + map[bool]string{true: "yes", false: "no"}[bound])
	if r.startErr != nil {
		return nil, r.startErr
	}
	if r.fail != nil {
		go func() {
			select {
			case err := <-r.fail:
				failed <- err
			case <-ctx.Done():
			}
		}()
	}
	return func() {
		// Run has drained business by now: its context is done.
		r.note("stop:ctx=" + map[bool]string{true: "done", false: "live"}[ctx.Err() != nil])
	}, nil
}

func (r *recordingRuntime) runController(ctx context.Context, _ *App, run func(context.Context)) {
	r.note("controller")
	if r.controllerRan != nil {
		close(r.controllerRan)
	}
	run(ctx)
}

func (r *recordingRuntime) beforeClose(*App) error { r.note("beforeClose"); return r.beforeCloseErr }
func (r *recordingRuntime) afterClose(*App) error  { r.note("afterClose"); return r.afterCloseErr }

func newMemberApp(t *testing.T, ha haRuntime) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := normalizeRuntimeConfig(Config{
		Addr: "127.0.0.1:0", DataDir: dir,
		DBPath:    filepath.Join(dir, "gpuworks.db"),
		AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	events := bus.New()
	st, master, err := openCredentialStore(cfg, events, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg.agents, err = agentbin.Prepare(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := assembleBusiness(cfg, log, events, st, master, ha)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func runUntilBound(t *testing.T, a *App) (addr string, cancel context.CancelFunc, done chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		addr = a.Addr()
		if addr != "" && !strings.HasSuffix(addr, ":0") {
			return addr, cancel, done
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("Run returned before binding: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	t.Fatal("listener did not bind")
	return "", nil, nil
}

func waitRun(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestSingleNodeServesReadinessWithoutMemberRuntime(t *testing.T) {
	a := newMemberApp(t, nil)
	addr, cancel, done := runUntilBound(t, a)
	if code, body := getStatus(t, "http://"+addr+"/api/v1/readyz"); code != http.StatusOK || body != "{\"status\":\"ready\"}\n" {
		t.Fatalf("readyz = %d %q", code, body)
	}
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMemberRuntimeTakesPartInRunAndClose(t *testing.T) {
	ha := &recordingRuntime{controllerRan: make(chan struct{})}
	a := newMemberApp(t, ha)
	addr, cancel, done := runUntilBound(t, a)
	select {
	case <-ha.controllerRan:
	case <-time.After(5 * time.Second):
		t.Fatal("the controller did not run through the member runtime")
	}
	if code, body := getStatus(t, "http://"+addr+"/api/v1/readyz"); code != http.StatusOK || body != "{\"status\":\"ready\"}\n" {
		t.Fatalf("admitted readyz = %d %q", code, body)
	}
	// The admission sits in front of every business route, readiness included.
	ha.mu.Lock()
	ha.refuse = true
	ha.mu.Unlock()
	for _, path := range []string{"/api/v1/readyz", "/api/v1/nodes", "/healthz"} {
		if code, body := getStatus(t, "http://"+addr+path); code != http.StatusServiceUnavailable || body != "member business service is not ready\n" {
			t.Fatalf("refused %s = %d %q", path, code, body)
		}
	}
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if got, want := ha.seen(), "start:bound=yes,controller,stop:ctx=done"; got != want {
		t.Fatalf("Run hooks = %s, want %s", got, want)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := ha.seen(), "start:bound=yes,controller,stop:ctx=done,beforeClose,afterClose"; got != want {
		t.Fatalf("hooks = %s, want %s", got, want)
	}
	// The store is closed before afterClose: nothing reads it any more.
	if _, err := a.store.CountNodes(); err == nil {
		t.Fatal("store still open after Close")
	}
}

func TestMemberStartFailureFailsRunAndReleasesTheListener(t *testing.T) {
	boom := errors.New("enrollment listener cannot bind")
	ha := &recordingRuntime{startErr: boom}
	a := newMemberApp(t, ha)
	t.Cleanup(func() { _ = a.Close() })
	if err := a.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the start failure", err)
	}
	if got := ha.seen(); got != "start:bound=yes" {
		t.Fatalf("hooks = %s; no business loop may start after a failed member start", got)
	}
	if addr := a.Addr(); addr != "" && !strings.HasSuffix(addr, ":0") {
		t.Fatalf("listener still published at %s", addr)
	}
	// The App can run again once the member can start.
	ha.startErr = nil
	_, cancel, done := runUntilBound(t, a)
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestMemberFailureAfterStartEndsRunWithItsError(t *testing.T) {
	boom := errors.New("enrollment HTTPS server stopped unexpectedly")
	ha := &recordingRuntime{fail: make(chan error, 1)}
	a := newMemberApp(t, ha)
	t.Cleanup(func() { _ = a.Close() })
	_, cancel, done := runUntilBound(t, a)
	defer cancel()
	ha.fail <- boom
	if err := waitRun(t, done); !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the member failure", err)
	}
	if got := ha.seen(); !strings.HasSuffix(got, "stop:ctx=done") {
		t.Fatalf("hooks = %s; the member must be stopped after the drain", got)
	}
}

func TestClosedMemberIsServedByItsRuntimeOnly(t *testing.T) {
	ha := &recordingRuntime{closed: true}
	a := &App{cfg: Config{Addr: "127.0.0.1:0"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ha: ha}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for ha.seen() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Business never bound a listener and no other hook ran.
	if got := ha.seen(); got != "closed:bound=127.0.0.1:0" {
		t.Fatalf("hooks = %q", got)
	}
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second Run = %v", err)
	}
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close of a closed member: %v", err)
	}
	if got := ha.seen(); got != "closed:bound=127.0.0.1:0,beforeClose,afterClose" {
		t.Fatalf("hooks = %q", got)
	}
	if err := a.Run(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after Close = %v", err)
	}

	failing := &recordingRuntime{closed: true, closedErr: errors.New("raft session lost")}
	b := &App{cfg: Config{Addr: "127.0.0.1:0"}, log: a.log, ha: failing}
	if err := b.Run(context.Background()); err == nil || err.Error() != "raft session lost" {
		t.Fatalf("closed Run = %v", err)
	}
}

func TestCloseKeepsTheStoreWhileTheMemberCannotWithdraw(t *testing.T) {
	stuck := errors.New("material proof still running")
	ha := &recordingRuntime{beforeCloseErr: stuck, afterCloseErr: errors.New("raft session close failed")}
	a := newMemberApp(t, ha)
	if err := a.Close(); !errors.Is(err, stuck) {
		t.Fatalf("Close = %v, want the withdrawal failure", err)
	}
	if got := ha.seen(); got != "beforeClose" {
		t.Fatalf("hooks = %s; nothing may be released while the member still uses the store", got)
	}
	if _, err := a.store.CountNodes(); err != nil {
		t.Fatalf("store closed under a member that could not withdraw: %v", err)
	}
	// A refused Close released nothing and is not final: the App still runs.
	_, cancel, done := runUntilBound(t, a)
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatal(err)
	}
	ha.mu.Lock()
	ha.events = nil
	ha.mu.Unlock()
	if err := a.Close(); !errors.Is(err, stuck) {
		t.Fatalf("repeated Close = %v, want the withdrawal failure again", err)
	}
	ha.beforeCloseErr = nil
	if err := a.Close(); err == nil || err.Error() != "raft session close failed" {
		t.Fatalf("second Close = %v, want the session release failure", err)
	}
	if got := ha.seen(); got != "beforeClose,beforeClose,afterClose" {
		t.Fatalf("hooks = %s", got)
	}
	// Released: Close is final now and repeats its result.
	if err := a.Close(); err == nil || err.Error() != "raft session close failed" {
		t.Fatalf("Close after release = %v", err)
	}
	if err := a.Run(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after Close = %v", err)
	}
}
