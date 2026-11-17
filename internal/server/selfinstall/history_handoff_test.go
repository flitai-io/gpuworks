package selfinstall

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A root install hands the tree to the service account, and that account is
// what confirms a console upgrade by appending to the install history. The log
// therefore has to exist when the last handoff runs: created after it, it
// stays root's and the confirmation cannot open it.
func TestInstallCreatesTheHistoryBeforeTheFinalHandoff(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("gpuworks binary v1")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	req.Password = "fresh-install-password-2026"
	handoff := "sudo chown -R " + DefaultServiceUser + ":" + DefaultServiceUser + " " + h.layout.Dir
	var existedAtHandoff []bool
	h.run.on = func(cmd Command) {
		if cmd.String() == handoff {
			_, err := os.Stat(h.layout.HistoryPath())
			existedAtHandoff = append(existedAtHandoff, err == nil)
		}
	}

	h.mustInstall(req)

	if len(existedAtHandoff) == 0 || !existedAtHandoff[len(existedAtHandoff)-1] {
		t.Fatalf("history present at each handoff = %v; the final one must cover it (%v)", existedAtHandoff, h.run.executed())
	}
	entries, err := ReadHistory(h.layout.HistoryPath())
	if err != nil || len(entries) != 1 || entries[0].Action != ActionInstall || entries[0].Result != "ok" {
		t.Fatalf("history = %+v, %v", entries, err)
	}
}

// The handoff still decides whether the install is recorded as done: if it
// fails, the log may exist but holds no "ok".
func TestInstallWhoseHandoffFailsRecordsNoSuccess(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("gpuworks binary v1")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	req.Password = "fresh-install-password-2026"
	handoff := "sudo chown -R " + DefaultServiceUser + ":" + DefaultServiceUser + " " + h.layout.Dir
	seen := 0
	h.run.on = func(cmd Command) {
		if cmd.String() == handoff {
			seen++
			if seen == 2 {
				h.run.mu.Lock()
				h.run.fail[handoff] = errors.New("chown: operation not permitted")
				h.run.mu.Unlock()
			}
		}
	}

	_, err := h.installer.Install(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "protect completed installation") {
		t.Fatalf("install error = %v", err)
	}
	if entries, _ := ReadHistory(h.layout.HistoryPath()); len(entries) != 0 {
		t.Fatalf("history = %+v; a failed handoff must not be recorded as a successful install", entries)
	}
}

// Installs made before the fix still have a root-owned log. Confirming the
// upgrade must not depend on it: the marker goes (so the next start does not
// roll back) and the missing record is reported, not fatal.
func TestConfirmStartSurvivesAnUnwritableHistory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can append to a read-only file")
	}
	h, _ := stageForGuard(t)
	writeFile(t, h.layout.HistoryPath(), "", 0o400)
	guard, err := GuardStart(h.layout, "v2.0.0", ModeSystemd, time.Now())
	if err != nil || guard.Pending == nil {
		t.Fatal(guard, err)
	}

	err = ConfirmStart(h.layout, *guard.Pending, ModeSystemd, time.Now(), DefaultBackupKeep)
	if !errors.Is(err, ErrHistoryNotRecorded) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("ConfirmStart = %v, want ErrHistoryNotRecorded wrapping the permission error", err)
	}
	if pendingOf(t, h.layout) != nil {
		t.Fatal("the marker survived; the next start would roll a working upgrade back")
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	again, err := GuardStart(h.layout, "v2.0.0", ModeSystemd, time.Now())
	if err != nil || again.RolledBack || again.Pending != nil {
		t.Fatalf("next start = %+v, %v; want a plain start", again, err)
	}
}

// A CLI upgrade (root) hands the tree over again before it starts the new
// version. A history removed by hand is recreated before that handoff, not by
// the closing audit line after it, so the service account can still append.
func TestUpgradeRecreatesAMissingHistoryBeforeItsHandoff(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	_ = os.Remove(h.layout.HistoryPath())
	handoff := "sudo chown -R " + DefaultServiceUser + ":" + DefaultServiceUser + " " + h.layout.Dir
	var existedAtHandoff []bool
	h.run.on = func(cmd Command) {
		if cmd.String() == handoff {
			_, err := os.Stat(h.layout.HistoryPath())
			existedAtHandoff = append(existedAtHandoff, err == nil)
		}
	}

	if _, err := h.installer.Upgrade(context.Background(), h.upgradeReq()); err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if len(existedAtHandoff) == 0 || !existedAtHandoff[0] {
		t.Fatalf("history present at each handoff = %v (%v)", existedAtHandoff, h.run.executed())
	}
	if entries, err := ReadHistory(h.layout.HistoryPath()); err != nil || len(entries) != 1 || entries[0].Action != ActionUpgrade {
		t.Fatalf("history = %+v, %v", entries, err)
	}
}
