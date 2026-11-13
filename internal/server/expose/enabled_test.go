package expose

import (
	"os"
	"testing"
)

// The server keeps one watch for its lifetime while the CLI enables, disables
// and removes the entry point underneath it; every change must be seen.
func TestEnabledWatchFollowsTheDesiredState(t *testing.T) {
	store := NewStore(t.TempDir())
	watch := store.WatchEnabled()
	if watch.Enabled() {
		t.Fatal("no configuration must not be enabled")
	}
	write := func(desired string) {
		t.Helper()
		if err := store.WriteConfig(&Config{Revision: NewRevision(), Provider: ProviderQuick, Desired: desired, Origin: "http://[::1]:8800"}); err != nil {
			t.Fatal(err)
		}
	}
	write(DesiredEnabled)
	for range 2 {
		if !watch.Enabled() {
			t.Fatal("enabled configuration not seen")
		}
	}
	write(DesiredDisabled)
	if watch.Enabled() {
		t.Fatal("expose down not seen")
	}
	write(DesiredEnabled)
	if !watch.Enabled() {
		t.Fatal("enabled again not seen")
	}
	if err := os.WriteFile(store.ConfigPath(), []byte("{not json"), 0o664); err != nil {
		t.Fatal(err)
	}
	if watch.Enabled() {
		t.Fatal("an unreadable configuration must not be enabled")
	}
	write(DesiredEnabled)
	if err := os.Remove(store.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	if watch.Enabled() {
		t.Fatal("expose rm not seen")
	}
}

// A read that fails is retried on the next question rather than remembered
// against an unchanged file.
func TestEnabledWatchRetriesAFailedRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	store := NewStore(t.TempDir())
	if err := store.WriteConfig(&Config{Revision: NewRevision(), Provider: ProviderQuick, Desired: DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
	watch := store.WatchEnabled()
	if err := os.Chmod(store.ConfigPath(), 0); err != nil {
		t.Fatal(err)
	}
	if watch.Enabled() {
		t.Fatal("an unreadable file must not be enabled")
	}
	if err := os.Chmod(store.ConfigPath(), 0o664); err != nil {
		t.Fatal(err)
	}
	if !watch.Enabled() {
		t.Fatal("the failed read was cached")
	}
}
