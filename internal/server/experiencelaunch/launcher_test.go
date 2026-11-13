package experiencelaunch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// The launcher starts this test binary in place of gpuworks: with
// helperEnv set it plays the environment, recording its arguments, serving
// /healthz on its address, and leaving on SIGTERM (or at once, exiting 3,
// when helperEnv is "exit").
const (
	helperEnv     = "GPUWORKS_EXPERIENCE_TEST_HELPER"
	helperArgsEnv = "GPUWORKS_EXPERIENCE_TEST_ARGS"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(helper(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func helper(mode string, args []string) int {
	if f := os.Getenv(helperArgsEnv); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(args, "\n")), 0o600)
	}
	if mode == "exit" {
		return 3
	}
	addr := ""
	for i, a := range args {
		if (a == "--addr" || a == "--listen") && i+1 < len(args) {
			addr = strings.Split(args[i+1], ",")[0]
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	if ln, err := net.Listen("tcp", addr); err == nil {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		go func() { _ = http.Serve(ln, mux) }()
	}
	<-sig
	return 0
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type rig struct {
	l        *Launcher
	dir      string
	argsFile string
	port     int
}

func newRig(t *testing.T, mode string, edit func(*Config)) *rig {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv(helperEnv, mode)
	t.Setenv(helperArgsEnv, argsFile)
	port := freePort(t)
	cfg := Config{Binary: bin, DataDir: dir, Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Port: port}
	if edit != nil {
		edit(&cfg)
	}
	l := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = l.SetEnabled(context.Background(), false) })
	return &rig{l: l, dir: dir, argsFile: argsFile, port: port}
}

func (r *rig) args(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(r.argsFile); err == nil && len(raw) > 0 {
			return strings.Split(string(raw), "\n")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the environment did not start")
	return nil
}

func (r *rig) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.l.Status(context.Background()).Ready {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the environment never answered /healthz")
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestSwitchingOnStartsWithANewPasswordAndFreshData(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "run", nil)
	stale := filepath.Join(r.dir, "data", "seeded")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(stale, []byte("x"), 0o600)

	if st := r.l.Status(ctx); st.Enabled || st.Password != "" || st.Port != r.port || st.User != User {
		t.Fatalf("before = %+v", st)
	}
	if err := r.l.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	args := r.args(t)
	if args[0] != "experience" || flagValue(args, "--data") != filepath.Join(r.dir, "data") ||
		flagValue(args, "--password-file") != filepath.Join(r.dir, "console-password") ||
		flagValue(args, "--parent-pid") != strconv.Itoa(os.Getpid()) ||
		flagValue(args, "--addr") != net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port)) ||
		flagValue(args, "--listen") != "" || flagValue(args, "--tls-cert") != "" {
		t.Fatalf("args = %q", args)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the previous data survived: %v", err)
	}
	info, err := os.Stat(filepath.Join(r.dir, "console-password"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("password file = %v, %v", info, err)
	}
	r.waitReady(t)
	first := r.l.Status(ctx)
	raw, _ := os.ReadFile(filepath.Join(r.dir, "console-password"))
	if !first.Enabled || len(first.Password) != 16 || string(raw) != first.Password {
		t.Fatalf("on = %+v, file %q", first, raw)
	}

	// On again while running changes nothing.
	if err := r.l.SetEnabled(ctx, true); err != nil || r.l.Status(ctx).Password != first.Password {
		t.Fatalf("second on: %v, %+v", err, r.l.Status(ctx))
	}

	if err := r.l.SetEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if st := r.l.Status(ctx); st.Enabled || st.Ready || st.Password != "" {
		t.Fatalf("off = %+v", st)
	}
	if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port))); err != nil {
		t.Fatalf("the port is still held after off: %v", err)
	} else {
		ln.Close()
	}

	_ = os.Remove(r.argsFile)
	if err := r.l.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	r.args(t)
	r.waitReady(t)
	if second := r.l.Status(ctx); second.Password == first.Password || second.Password == "" {
		t.Fatalf("switching on again kept the password %q", second.Password)
	}
}

func TestListenModePassesEveryAddressAndTheCertificate(t *testing.T) {
	r := newRig(t, "run", func(c *Config) {
		c.Listen = []string{c.Addr, "127.0.0.1:" + strconv.Itoa(c.Port)}
		c.Addr = ""
		c.TLSCertFile, c.TLSKeyFile = "/etc/gpuworks/tls.crt", "/etc/gpuworks/tls.key"
	})
	if err := r.l.SetEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	args := r.args(t)
	want := fmt.Sprintf("127.0.0.1:%d,127.0.0.1:%d", r.port, r.port)
	if flagValue(args, "--listen") != want || flagValue(args, "--addr") != "" ||
		flagValue(args, "--tls-cert") != "/etc/gpuworks/tls.crt" || flagValue(args, "--tls-key") != "/etc/gpuworks/tls.key" {
		t.Fatalf("args = %q", args)
	}
}

func TestResetAndRotateNeedARunningEnvironment(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "run", nil)
	if err := r.l.Reset(ctx); err == nil {
		t.Fatal("reset of a stopped environment succeeded")
	}
	if err := r.l.RotatePassword(ctx); err == nil {
		t.Fatal("rotating the password of a stopped environment succeeded")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "console-password")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused action wrote a password: %v", err)
	}

	if err := r.l.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	r.waitReady(t)
	password := r.l.Status(ctx).Password
	marker := filepath.Join(r.dir, "data", "visitor-made")
	_ = os.MkdirAll(filepath.Dir(marker), 0o700)
	_ = os.WriteFile(marker, []byte("x"), 0o600)
	if err := r.l.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	r.waitReady(t)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset kept the data: %v", err)
	}
	if got := r.l.Status(ctx).Password; got != password {
		t.Fatalf("reset changed the password %q -> %q", password, got)
	}

	if err := r.l.RotatePassword(ctx); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(r.dir, "console-password"))
	if got := r.l.Status(ctx).Password; got == password || string(raw) != got {
		t.Fatalf("rotate = %q, file %q (was %q)", got, raw, password)
	}
}

func TestAPortHeldElsewhereIsNamed(t *testing.T) {
	r := newRig(t, "run", nil)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = r.l.SetEnabled(context.Background(), true)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorExperiencePortInUse || product.Params()["port"] != r.port {
		t.Fatalf("err = %v, want %s naming port %d", err, messages.ErrorExperiencePortInUse, r.port)
	}
	if r.l.Status(context.Background()).Enabled {
		t.Fatal("the environment counts as running")
	}
}

func TestAnEnvironmentThatExitsReportsWhy(t *testing.T) {
	r := newRig(t, "exit", nil)
	if err := r.l.SetEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := r.l.Status(context.Background()); !st.Enabled {
			if !strings.Contains(st.Error, "exit status 3") || st.Password != "" {
				t.Fatalf("exited = %+v", st)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the exit was not noticed")
}

func TestLastErrorNamesTheLoggedCause(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "experience.log")
	exit := errors.New("exit status 1")
	if got := lastError(filepath.Join(dir, "missing.log"), exit); got != "exit status 1" {
		t.Fatalf("no log: %q", got)
	}
	log := "time=x level=WARN msg=slow\n" +
		`time=y level=ERROR msg="experience environment exited" err="this build carries no agent for linux/amd64"` + "\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastError(path, exit); got != "this build carries no agent for linux/amd64" {
		t.Fatalf("logged cause: %q", got)
	}
	if err := os.WriteFile(path, []byte("time=x level=INFO msg=ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastError(path, exit); got != "exit status 1" {
		t.Fatalf("no error logged: %q", got)
	}
}
