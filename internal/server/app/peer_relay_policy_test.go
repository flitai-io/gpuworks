package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestNewRejectsInvalidInProcessPeerRelayPolicy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.PeerRelay = serverconfig.PeerRelay{RateLimitKiBPerSecond: -1}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || app != nil || !strings.Contains(err.Error(), "peer_relay.rate_limit_kib_s") {
		t.Fatalf("invalid direct Config bypassed policy: app=%v err=%v", app, err)
	}
}
func TestNewWiresCustomPeerRelayPolicy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.AdminPassword = "gpuworks-test-password-2026"
	cfg.PeerRelay = serverconfig.PeerRelay{RateLimitKiBPerSecond: 8192, BurstKiB: 32, NoProgressTimeout: "3m", ConfirmationTimeout: "6m"}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.obs.close(); app.store.Close() })
	want, err := cfg.PeerRelay.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if got := app.hub.PeerTransferSnapshot().Policy; got != want || got == prepobs.DefaultPeerRelayPolicy() {
		t.Fatalf("startup policy=%+v want=%+v", got, want)
	}
}
