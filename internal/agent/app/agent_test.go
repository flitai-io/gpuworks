package app

import (
	"io"
	"log/slog"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestDetectDriverExplicitNoneAndInvalid(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := detectDriver(log, t.TempDir(), "none", nil).driver; got != nil {
		t.Fatalf("detectDriver(none) = %T, want nil", got)
	}
	if got := detectDriver(log, t.TempDir(), "bogus", nil).driver; got != nil {
		t.Fatalf("detectDriver(bogus) = %T, want nil", got)
	}
	// The kube runtime outside a pod has no API server to talk to.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if got := detectDriver(log, t.TempDir(), "kube", nil).driver; got != nil {
		t.Fatalf("detectDriver(kube) outside a pod = %T, want nil", got)
	}
}

// The host runtime runs macOS environment packages and runveil is Linux PRoot:
// forcing either on the other OS leaves the node without a runtime rather than
// one that cannot start anything.
func TestForcedDriverMustSuitTheOS(t *testing.T) {
	for _, c := range []struct {
		goos, driver string
		refused      bool
	}{
		{"linux", "host", true},
		{"darwin", "host", false},
		{"darwin", "udocker", true},
		{"linux", "udocker", false},
		{"linux", "docker", false},
		{"darwin", "", false},
	} {
		if got := driverUnavailableOn(c.goos, c.driver) != ""; got != c.refused {
			t.Errorf("driverUnavailableOn(%s, %q) refused=%v, want %v", c.goos, c.driver, got, c.refused)
		}
	}
	if goruntime.GOOS != "darwin" {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		if got := detectDriver(log, t.TempDir(), "host", nil).driver; got != nil {
			t.Fatalf("detectDriver(host) on %s = %T, want nil", goruntime.GOOS, got)
		}
	}
}

func newTestAgent(t *testing.T, cfg Config) *Agent {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRunRequiresPerNodeToken(t *testing.T) {
	a := newTestAgent(t, Config{ServerURL: "https://server.example"})
	err := a.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "node token") {
		t.Fatalf("Run() error = %v, want per-node token validation", err)
	}
}

func TestRunRequiresServerURL(t *testing.T) {
	a := newTestAgent(t, Config{Token: "node-token"})
	err := a.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "server URL") {
		t.Fatalf("Run() error = %v, want server URL validation", err)
	}
}
