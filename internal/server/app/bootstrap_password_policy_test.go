package app

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func bootstrapPolicyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestBootstrapPreservesExplicitTrustedServiceGroupMode(t *testing.T) {
	t.Setenv(adminPasswordEnvVar, "")
	st := bootstrapPolicyStore(t)
	const password = "seven teal otters cross the meadow"
	if _, _, _, err := st.EnsureAdmin("admin", password); err != nil {
		t.Fatal(err)
	}
	cfg := Config{AdminUser: "admin", DataDir: t.TempDir()}
	policy, err := securefile.TrustedGroup(os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	if err := installcred.WriteInitialCredentials(cfg.DataDir, "admin", password, policy); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := bootstrapAdminAccount(cfg, st, slog.New(slog.NewTextHandler(&out, nil)), policy); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(installcred.CredentialsPath(cfg.DataDir))
	if err != nil || info.Mode().Perm() != policy.Mode() {
		t.Fatalf("bootstrap changed the established service-group access: info=%v err=%v", info, err)
	}
	if _, valid := st.VerifyUser("admin", password); !valid || strings.Contains(out.String(), password) {
		t.Fatal("bootstrap changed or exposed the existing password")
	}
}

func TestBootstrapRejectsWeakPasswordBeforeCreatingDefaultKey(t *testing.T) {
	for _, source := range []string{"config", "env", "file"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv(adminPasswordEnvVar, "")
			st := bootstrapPolicyStore(t)
			cfg := Config{AdminUser: "admin", DataDir: t.TempDir()}
			switch source {
			case "config":
				cfg.AdminPassword = "123"
			case "env":
				t.Setenv(adminPasswordEnvVar, "password")
			case "file":
				if err := installcred.WriteInitialCredentials(cfg.DataDir, "admin", "admin123"); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			err := bootstrapAdminAccount(cfg, st, slog.New(slog.NewTextHandler(&out, nil)))
			if !errors.Is(err, auth.ErrPasswordPolicy) {
				t.Fatalf("weak %s password error = %v", source, err)
			}
			var users int64
			if err := st.DB().Model(&store.User{}).Count(&users).Error; err != nil || users != 0 || countLiveAPIKeys(t, st) != 0 {
				t.Fatalf("rejected credentials left authentication state: users=%d err=%v", users, err)
			}
		})
	}
}

func TestBootstrapCredentialsWriteFailureFailsClosedAndKeepsExistingAccountOnRetry(t *testing.T) {
	t.Setenv(adminPasswordEnvVar, "")
	st := bootstrapPolicyStore(t)
	data := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(data, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	const password = "seven teal otters cross the meadow"
	cfg := Config{AdminUser: "admin", AdminPassword: password, DataDir: data}
	var out strings.Builder
	logger := slog.New(slog.NewTextHandler(&out, nil))
	err := bootstrapAdminAccount(cfg, st, logger)
	if err == nil || !strings.Contains(err.Error(), "reset-password") {
		t.Fatalf("credentials write error lacks recovery instructions: %v", err)
	}
	if countLiveAPIKeys(t, st) != 0 || strings.Contains(out.String(), password) {
		t.Fatal("failed bootstrap created an API key or logged a password")
	}
	user, valid := st.VerifyUser("admin", password)
	if !valid {
		t.Fatal("the existing account was unexpectedly removed after a file error")
	}
	if err := os.Remove(data); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapAdminAccount(cfg, st, logger); err != nil {
		t.Fatal(err)
	}
	current, valid := st.VerifyUser("admin", password)
	if !valid || current.PasswordHash != user.PasswordHash {
		t.Fatal("retry replaced the existing password")
	}
	if _, err := os.Stat(installcred.CredentialsPath(data)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry rewrote a credentials file for an existing account: %v", err)
	}
	if strings.Contains(out.String(), password) {
		t.Fatal("retry exposed the account password in the log")
	}
}

// countLiveAPIKeys counts unrevoked API keys. Bootstrap must never create one:
// its secret would be discarded, so nothing could authenticate with it.
func countLiveAPIKeys(t *testing.T, st *store.Store) int {
	t.Helper()
	keys, err := st.ListAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for i := range keys {
		if keys[i].RevokedAt == nil {
			live++
		}
	}
	return live
}
