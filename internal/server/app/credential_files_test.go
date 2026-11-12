package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStandaloneStartupDoesNotTrustThePrimaryGroup(t *testing.T) {
	for _, configPath := range []string{"", filepath.Join(t.TempDir(), "server.yaml")} {
		policy, err := startupCredentialPolicy(Config{ConfigPath: configPath})
		if err != nil || policy.Mode() != securefile.OwnerMode {
			t.Fatalf("standalone credential mode=%04o err=%v", policy.Mode(), err)
		}
	}
}

func TestStartupProtectsInitialCredentialsWithoutReadingTheirPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "initial-credentials")
	if err := os.WriteFile(path, []byte("synthetic legacy credentials"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := protectStartupCredentials(Config{DataDir: dir, AdminPassword: "an explicit long passphrase"}, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o007 != 0 {
		t.Fatalf("credentials remain public: info=%v err=%v", info, err)
	}
}

func TestStartupOnlyHardensBackupsBelongingToThisInstalledBinary(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone config", true: "installed binary"}[installed], func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{DataDir: filepath.Join(dir, "data"), ConfigPath: filepath.Join(dir, "gpuworks.yaml")}
			layout := selfinstall.NewLayout(dir, cfg.DataDir, "")
			if err := os.MkdirAll(layout.BackupsDir, 0o755); err != nil {
				t.Fatal(err)
			}
			backup := layout.BackupPath("1.2.3", time.Date(2026, 11, 4, 1, 2, 3, 0, time.UTC))
			if err := os.WriteFile(backup, []byte("synthetic old archive"), 0o664); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(backup, 0o664); err != nil {
				t.Fatal(err)
			}
			if installed {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(executable, layout.Binary()); err != nil {
					t.Fatal(err)
				}
			}
			if err := protectStartupCredentials(cfg, nil); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(backup)
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(0o664)
			if installed {
				var policy securefile.Policy
				policy, err = selfinstall.SensitiveFilePolicy(layout)
				want = policy.Mode()
				if err != nil {
					t.Fatal(err)
				}
			}
			if info.Mode().Perm() != want {
				t.Fatalf("backup mode %04o, want %04o", info.Mode().Perm(), want)
			}
		})
	}
}

func TestNewRefusesWeakBootstrapBeforeCreatingUsersOrAPIKeys(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "gpuworks.db")
	application, err := New(Config{DataDir: dir, DBPath: db, AdminUser: "admin", AdminPassword: "123"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || application != nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("weak bootstrap result: app=%v err=%v", application, err)
	}
	st, err := store.Open(db, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var users int64
	if err := st.DB().Model(&store.User{}).Count(&users).Error; err != nil {
		t.Fatal(err)
	}
	if users != 0 || countLiveAPIKeys(t, st) != 0 {
		t.Fatalf("weak bootstrap left users=%d or an API key", users)
	}
}
