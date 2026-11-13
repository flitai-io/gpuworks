package selfinstall

import (
	"context"
	"errors"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestInstallRejectsWeakBootstrapPasswordsBeforeWrites(t *testing.T) {
	for _, source := range []string{"flag", "env", "prompt"} {
		// "Admin@2026" is the admin username under the account-aware rule.
		for _, password := range []string{"123", "password", "admin123", "Admin@2026"} {
			t.Run(source+"/"+password, func(t *testing.T) {
				t.Setenv(AdminPasswordEnvVar, "")
				h := newHarness(t, t.TempDir())
				h.installSelf("source binary")
				req := h.quiet()
				switch source {
				case "flag":
					req.Password = password
				case "env":
					t.Setenv(AdminPasswordEnvVar, password)
				case "prompt":
					h.terminal = true
					h.stdin = password + "\n"
					req.Quiet = false
				}
				_, err := h.installer.Install(context.Background(), req)
				if !errors.Is(err, auth.ErrPasswordPolicy) {
					t.Fatalf("weak %s password error = %v", source, err)
				}
				entries, err := os.ReadDir(h.layout.Dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("rejected password changed installation: entries=%d err=%v", len(entries), err)
				}
				if h.run.ran("sudo systemctl stop") || h.run.ran("sudo chown") {
					t.Fatalf("rejected password changed service or ownership: %v", h.run.executed())
				}
			})
		}
	}
}

func TestGeneratedInstallerPasswordMatchesSharedPolicy(t *testing.T) {
	password, source, err := generatedPassword()
	if err != nil {
		t.Fatal(err)
	}
	if source != PasswordGenerated || len(password) != 48 {
		t.Fatalf("generated credentials do not have expected entropy/source: source=%q length=%d", source, len(password))
	}
	if err := auth.ValidatePassword(password); err != nil {
		t.Fatal(err)
	}
}
