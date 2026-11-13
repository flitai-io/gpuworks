package main

import (
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestResetPasswordRejectsWeakInputBeforeDatabaseAccess(t *testing.T) {
	for _, password := range []string{"123", "password", "admin123"} {
		t.Run(password, func(t *testing.T) {
			dir := t.TempDir()
			err := resetPassword(discardLog(), []string{"--db", filepath.Join(dir, "missing.db"), "--password", password})
			if !errors.Is(err, auth.ErrPasswordPolicy) {
				t.Fatalf("weak password did not fail before database access: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected password left files: count=%d err=%v", len(entries), err)
			}
		})
	}
}

// seedAdmin creates dataDir/gpuworks.db holding the admin user.
func seedAdmin(t *testing.T, dataDir string) string {
	t.Helper()
	db := filepath.Join(dataDir, "gpuworks.db")
	st, err := store.Open(db, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.EnsureAdmin("admin", "old-admin-password-2026"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return db
}

func verifyPassword(t *testing.T, db, password string) bool {
	t.Helper()
	st, err := store.Open(db, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, ok := st.VerifyUser("admin", password)
	return ok
}

func TestResetPasswordWithDataDir(t *testing.T) {
	data := t.TempDir()
	db := seedAdmin(t, data)
	if err := resetPassword(discardLog(), []string{"--data", data, "--password", "new-admin-password-2026"}); err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(t, db, "new-admin-password-2026") {
		t.Fatal("the new password does not log in")
	}
}

func TestResetPasswordUsesTheDriversPersistentDSNPath(t *testing.T) {
	for _, format := range []string{"plain-query", "file-uri"} {
		t.Run(format, func(t *testing.T) {
			db := seedAdmin(t, t.TempDir())
			dsn := db + "?cache=shared"
			if format == "file-uri" {
				dsn = (&url.URL{Scheme: "file", Path: db, RawQuery: "cache=shared"}).String()
			}
			if err := resetPassword(discardLog(), []string{"--db", dsn, "--password", "new-admin-password-2026"}); err != nil {
				t.Fatal(err)
			}
			if !verifyPassword(t, db, "new-admin-password-2026") {
				t.Fatal("the database selected by the driver was not updated")
			}
		})
	}
}

func TestResetPasswordRejectsMemoryDSN(t *testing.T) {
	for _, dsn := range []string{":memory:", "file:reset-memory?mode=memory&cache=shared"} {
		if _, err := resetPasswordDB("", dsn); err == nil || !strings.Contains(err.Error(), "persistent") {
			t.Errorf("memory database was accepted as a password-reset target: %v", err)
		}
	}
}

// A database that is not there is reported by its path and how that path was
// chosen, not by SQLite's "out of memory (14)", and no empty database is left
// behind in its place.
func TestResetPasswordMissingDatabase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   func(dir string) []string
		source string
	}{
		{"data dir without a database", func(dir string) []string { return []string{"--data", dir} }, "(from --data)"},
		{"data dir that does not exist", func(dir string) []string { return []string{"--data", filepath.Join(dir, "missing")} }, "(from --data)"},
		{"db path", func(dir string) []string { return []string{"--db", filepath.Join(dir, "gpuworks.db")} }, "(from --db)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := resetPassword(discardLog(), append(tc.args(dir), "--password", "new-admin-password-2026"))
			if err == nil {
				t.Fatal("reset succeeded without a database")
			}
			for _, want := range []string{dir, "gpuworks.db", tc.source, "does not exist", "--data"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "out of memory") {
				t.Errorf("error %q still carries the SQLite message", err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Errorf("reset left %d entries behind in %s", len(entries), dir)
			}
		})
	}
}

// The installed binary run from another directory, without --data, finds the
// database through the server.yaml beside it — the operator in /root running
// /opt/gpuworks/server/gpuworks reset-password.
func TestResetPasswordReadsServerYAMLNextToBinary(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_SERVER_CLI") == "1" {
		return // the help test's re-exec hook handles the child
	}
	install := t.TempDir()
	data := filepath.Join(install, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	db := seedAdmin(t, data)
	if err := os.WriteFile(filepath.Join(install, "server.yaml"), []byte("data_dir: "+data+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(install, "gpuworks")
	copyFile(t, os.Args[0], binary)

	cmd := exec.Command(binary, "-test.run=^TestServerCLIHelp$", "--", "reset-password", "--password", "new-admin-password-2026")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "GPUWORKS_TEST_SERVER_CLI=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reset-password: %v\n%s", err, out)
	}
	if !verifyPassword(t, db, "new-admin-password-2026") {
		t.Fatal("the database beside the binary was not changed")
	}
	if entries, _ := os.ReadDir(cmd.Dir); len(entries) != 0 {
		t.Errorf("reset touched the working directory: %d entries", len(entries))
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResetPasswordPromptsWhenNoneIsGiven(t *testing.T) {
	original := promptNewPassword
	t.Cleanup(func() { promptNewPassword = original })
	data := t.TempDir()
	db := seedAdmin(t, data)

	promptNewPassword = func() (string, error) { return "", errPasswordMismatch }
	if err := resetPassword(discardLog(), []string{"--data", data}); !errors.Is(err, errPasswordMismatch) {
		t.Fatalf("a failed prompt: %v", err)
	}
	promptNewPassword = func() (string, error) { return "short", nil }
	if err := resetPassword(discardLog(), []string{"--data", data}); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("a weak prompted password: %v", err)
	}
	if !verifyPassword(t, db, "old-admin-password-2026") {
		t.Fatal("a refused reset changed the password")
	}
	promptNewPassword = func() (string, error) { return "Tiger#Lake9", nil }
	if err := resetPassword(discardLog(), []string{"--data", data}); err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(t, db, "Tiger#Lake9") {
		t.Fatal("the prompted password does not log in")
	}
}

func TestResetPasswordWithoutATerminalNeedsTheFlag(t *testing.T) {
	// go test's stdin is not a terminal, so the real prompt refuses.
	if _, err := promptNewPassword(); err == nil || !strings.Contains(err.Error(), "-password is required") {
		t.Fatalf("prompt without a terminal: %v", err)
	}
}

func TestResetPasswordForgetsOnlyItsOwnInitialCredentials(t *testing.T) {
	for _, tc := range []struct {
		recorded string
		removed  bool
	}{{"admin", true}, {"ops", false}} {
		t.Run(tc.recorded, func(t *testing.T) {
			data := t.TempDir()
			seedAdmin(t, data)
			if err := installcred.WriteInitialCredentials(data, tc.recorded, "old-admin-password-2026"); err != nil {
				t.Fatal(err)
			}
			if err := resetPassword(discardLog(), []string{"--data", data, "--password", "new-admin-password-2026"}); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(installcred.CredentialsPath(data))
			if removed := os.IsNotExist(err); removed != tc.removed {
				t.Fatalf("record for %s removed=%v, want %v (stat err %v)", tc.recorded, removed, tc.removed, err)
			}
		})
	}
}

func TestResetPasswordSucceedsWhenTheRecordCannotBeRead(t *testing.T) {
	data := t.TempDir()
	db := seedAdmin(t, data)
	if err := os.WriteFile(installcred.CredentialsPath(data), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resetPassword(discardLog(), []string{"--data", data, "--password", "new-admin-password-2026"}); err != nil {
		t.Fatalf("an unreadable record failed the reset: %v", err)
	}
	if !verifyPassword(t, db, "new-admin-password-2026") {
		t.Fatal("the new password does not log in")
	}
	if _, err := os.Stat(installcred.CredentialsPath(data)); err != nil {
		t.Fatalf("a damaged record was removed: %v", err)
	}
}
