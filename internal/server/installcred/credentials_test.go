package installcred

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRoundTrip pins the install → server handoff: the file the installer
// writes is the file the server reads, 0600 for the service account and trusted operator group because it holds a clear-text
// password.
func TestRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	if err := WriteInitialCredentials(dataDir, "admin", "s3cret-token"); err != nil {
		t.Fatal(err)
	}
	path := CredentialsPath(dataDir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %v, want 0600 (the server reads it as the service account)", info.Mode().Perm())
	}
	creds, err := ReadInitialCredentials(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if creds.User != "admin" || creds.Password != "s3cret-token" {
		t.Fatalf("credentials = %+v", creds)
	}
	if creds.CreatedAt == "" {
		t.Fatal("CreatedAt is empty")
	}
}

func TestWriteRejectsIncompleteRecords(t *testing.T) {
	dataDir := t.TempDir()
	if err := WriteInitialCredentials(dataDir, "admin", ""); err == nil {
		t.Fatal("empty password accepted")
	}
	if err := WriteInitialCredentials(dataDir, "", "pw"); err == nil {
		t.Fatal("empty user accepted")
	}
	if _, err := os.Stat(CredentialsPath(dataDir)); err == nil {
		t.Fatal("a rejected record must not be written")
	}
}

// TestReadInitialCredentialsToleratesDamage pins that a hand-edited or
// truncated file degrades to "no credentials" instead of blocking startup.
func TestReadToleratesDamage(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := ReadInitialCredentials(dataDir); err == nil {
		t.Fatal("missing file must be an error")
	}
	for name, payload := range map[string]string{
		"truncated":       `{"user":"admin","passw`,
		"no-password":     `{"user":"admin"}`,
		"empty-password":  `{"user":"admin","password":""}`,
		"plain-text-file": "admin:hunter2\n",
	} {
		if err := os.WriteFile(CredentialsPath(dataDir), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadInitialCredentials(dataDir); err == nil {
			t.Fatalf("%s: read succeeded, want error", name)
		}
	}
}

func TestProtectInitialCredentialsRestrictsLegacyFileAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	path := CredentialsPath(dir)
	if err := WriteInitialCredentials(dir, "admin", "synthetic-fixture-password"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	if err := ProtectInitialCredentials(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("legacy credential bytes changed")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("legacy credential mode=%#o", info.Mode().Perm())
	}
	os.Remove(path)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, before, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := ProtectInitialCredentials(dir); err == nil {
		t.Fatal("linked credential file accepted")
	}
	info, _ = os.Stat(target)
	if info.Mode().Perm() != 0644 {
		t.Fatal("symlink target was changed")
	}
}

func TestForgetInitialCredentials(t *testing.T) {
	dataDir := t.TempDir()
	if err := WriteInitialCredentials(dataDir, "admin", "s3cret-token"); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dataDir, "gpuworks.db")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Another account changing its password leaves the admin's record.
	if err := ForgetInitialCredentials(dataDir, "zhangsan"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(CredentialsPath(dataDir)); err != nil {
		t.Fatalf("another account removed the record: %v", err)
	}
	for i := 0; i < 2; i++ { // the second call finds nothing and is not an error
		if err := ForgetInitialCredentials(dataDir, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(CredentialsPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("record still present: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("removed more than the record: %v", err)
	}
}

func TestForgetInitialCredentialsKeepsADamagedRecord(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(CredentialsPath(dataDir), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ForgetInitialCredentials(dataDir, "admin"); err == nil {
		t.Fatal("a damaged record was reported as handled")
	}
	if _, err := os.Stat(CredentialsPath(dataDir)); err != nil {
		t.Fatalf("damaged record removed: %v", err)
	}
}
