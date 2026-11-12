package app

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A first start must not create an API key. The gateway requires one by
// default; a key minted here would be logged without its secret, so nothing
// could authenticate with it, and a later restart would mint another after the
// first was revoked. The operator is told to create one instead.
func TestFirstStartCreatesNoAPIKeyAndSaysSo(t *testing.T) {
	t.Setenv(adminPasswordEnvVar, "")
	dir := t.TempDir()
	var out strings.Builder
	logger := slog.New(slog.NewTextHandler(&out, nil))
	st := bootstrapPolicyStoreIn(t, filepath.Join(dir, "gpuworks.db"))
	if err := bootstrapAdminAccount(Config{AdminUser: "admin", DataDir: dir}, st, logger); err != nil {
		t.Fatal(err)
	}
	if n := countLiveAPIKeys(t, st); n != 0 {
		t.Fatalf("bootstrap created %d API keys; a key whose secret is discarded cannot authenticate anyone", n)
	}
	logged := out.String()
	if strings.Contains(logged, "created default API key") {
		t.Fatalf("bootstrap still logs a minted key: %s", logged)
	}
	// /v1 refuses everything until a key exists, so the log must point at the
	// console rather than leaving the operator to guess.
	if !strings.Contains(logged, "requires an API key") || !strings.Contains(logged, "API tokens") {
		t.Fatalf("bootstrap did not explain that the gateway needs a key: %s", logged)
	}
	// A second start says the same thing instead of quietly adding a key.
	out.Reset()
	if err := bootstrapAdminAccount(Config{AdminUser: "admin", DataDir: dir}, st, logger); err != nil {
		t.Fatal(err)
	}
	if n := countLiveAPIKeys(t, st); n != 0 {
		t.Fatalf("a restart created %d API keys", n)
	}
}

// Once an administrator has a key, the startup hint is noise and stays quiet.
func TestFirstStartHintIsQuietWhenAKeyExists(t *testing.T) {
	t.Setenv(adminPasswordEnvVar, "")
	dir := t.TempDir()
	st := bootstrapPolicyStoreIn(t, filepath.Join(dir, "gpuworks.db"))
	if _, err := st.CreateAPIKey("operator", 0); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := bootstrapAdminAccount(Config{AdminUser: "admin", DataDir: dir}, st, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "requires an API key") {
		t.Fatalf("bootstrap hinted about a missing key when one exists: %s", out.String())
	}
	if n := countLiveAPIKeys(t, st); n != 1 {
		t.Fatalf("bootstrap changed the key count: %d", n)
	}
}

// An installation that explicitly opened the gateway needs no key, so the hint
// would be wrong there too.
func TestFirstStartHintIsQuietWithOpenAccessOn(t *testing.T) {
	t.Setenv(adminPasswordEnvVar, "")
	dir := t.TempDir()
	st := bootstrapPolicyStoreIn(t, filepath.Join(dir, "gpuworks.db"))
	if err := st.SetSecurityPolicy(store.SecurityPolicy{InferenceOpenAccess: true}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := bootstrapAdminAccount(Config{AdminUser: "admin", DataDir: dir}, st, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "requires an API key") {
		t.Fatalf("bootstrap hinted for a gateway that was explicitly opened: %s", out.String())
	}
}

func bootstrapPolicyStoreIn(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
