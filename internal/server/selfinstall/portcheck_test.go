package selfinstall

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// holdAddr listens on a loopback port for the rest of the test, standing in
// for another installation's server that keeps the address this one binds.
func holdAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// listensOn points the installation's server.yaml at addr.
func (h *harness) listensOn(addr string) {
	h.t.Helper()
	cfg := h.config()
	cfg.Listen = serverconfig.Listen{Console: []string{addr}}
	h.writeConfig(cfg)
}

// Two installations on one host bound to one port: stopping this one leaves
// the address held, so the new binary could never bind it. The upgrade names
// the holder and stops before the backup, and brings back the service it
// stopped.
func TestUpgradeRefusesAnAddressStillHeldAfterTheStop(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	addr := holdAddr(t)
	h.listensOn(addr)
	h.useSite("v2.0.0", []byte("new binary"))

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())

	if err == nil {
		t.Fatal("upgrade went ahead although the address is held by another process")
	}
	for _, want := range []string{
		addr + " is still in use after stopping the service of the installation at " + h.layout.Dir,
		fmt.Sprintf("pid %d", os.Getpid()),
		"nothing was changed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if report.Backup != "" || report.RolledBack {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if entries, _ := os.ReadDir(h.layout.BackupsDir); len(entries) != 0 {
		t.Fatalf("a backup was taken: %v", entries)
	}
	if !h.run.ranInOrder("sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("the stopped service was not started again: %v", h.run.executed())
	}
	if got := countCommands(h, "sudo systemctl restart gpuworks"); got != 1 {
		t.Fatalf("restarts = %d, want only the one bringing the old version back: %v", got, h.run.executed())
	}
	if entries, _ := ReadHistory(h.layout.HistoryPath()); len(entries) != 0 {
		t.Fatalf("history = %+v, want nothing recorded for an upgrade that never started", entries)
	}
}

// The gpu3 case: a nohup installation that was not running, whose address a
// different installation serves. Nothing of this one is started, since it was
// not running before.
func TestUpgradeLeavesAStoppedInstallationStoppedWhenItsAddressIsHeld(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.supervisedByNohup()
	h.listensOn(holdAddr(t))
	h.useSite("v2.0.0", []byte("new binary"))

	if _, err := h.installer.Upgrade(context.Background(), h.upgradeReq()); err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("err = %v", err)
	}
	if len(h.starts) != 0 {
		t.Fatalf("detached starts = %v, want none", h.starts)
	}
}

// The upgrade names the directory it acts on before it changes anything.
func TestUpgradeNamesTheInstallationItActsOn(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))

	if _, err := h.installer.Upgrade(context.Background(), h.upgradeReq()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if want := "upgrading the installation at " + h.layout.Dir + " (v1.0.0, systemd)"; !strings.Contains(h.out.String(), want) {
		t.Fatalf("output does not contain %q:\n%s", want, h.out.String())
	}
}

func TestServerDirOfReadsTheServedConfig(t *testing.T) {
	installed := t.TempDir()
	writeFile(t, filepath.Join(installed, binaryName), "binary", 0o755)
	writeFile(t, serverconfig.Path(installed), "listen: {}\n", 0o644)
	bare := t.TempDir()
	writeFile(t, filepath.Join(bare, binaryName), "binary", 0o755)

	for _, tc := range []struct {
		argv []string
		dir  string
		ok   bool
	}{
		{[]string{"/opt/gpuworks1/gpuworks", "serve", "--config", "/opt/gpuworks1/server.yaml"}, "/opt/gpuworks1", true},
		{[]string{"/opt/b/gpuworks", "serve", "--config=/etc/b/server.yaml"}, "/etc/b", true},
		{[]string{"/opt/b/gpuworks", "serve", "-config", "/srv/b/server.yaml"}, "/srv/b", true},
		{[]string{filepath.Join(installed, binaryName), "serve"}, mustEvalSymlinks(t, installed), true},
		{[]string{filepath.Join(bare, binaryName), "serve"}, "", false},
		{[]string{"/usr/sbin/nginx", "-c", "/etc/nginx.conf"}, "", false},
		{nil, "", false},
	} {
		dir, ok := serverDirOf(tc.argv)
		if dir != tc.dir || ok != tc.ok {
			t.Errorf("serverDirOf(%q) = %q, %v; want %q, %v", tc.argv, dir, ok, tc.dir, tc.ok)
		}
	}
}

func TestDirOfIsTheInstallationTheBinaryBelongsTo(t *testing.T) {
	withConfig := t.TempDir()
	writeFile(t, filepath.Join(withConfig, binaryName), "binary", 0o755)
	writeFile(t, serverconfig.Path(withConfig), "listen: {}\n", 0o644)
	withInstallJSON := t.TempDir()
	writeFile(t, filepath.Join(withInstallJSON, binaryName), "binary", 0o755)
	writeFile(t, filepath.Join(withInstallJSON, installJSONFile), "{}", 0o644)
	bare := t.TempDir()
	writeFile(t, filepath.Join(bare, binaryName), "binary", 0o755)
	renamed := filepath.Join(withConfig, "gpuworks-6fae92c9")
	writeFile(t, renamed, "binary", 0o755)
	link := filepath.Join(t.TempDir(), binaryName)
	if err := os.Symlink(filepath.Join(withConfig, binaryName), link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ exe, want string }{
		{filepath.Join(withConfig, binaryName), mustEvalSymlinks(t, withConfig)},
		{filepath.Join(withInstallJSON, binaryName), mustEvalSymlinks(t, withInstallJSON)},
		// A downloaded binary next to no installation acts on the default.
		{filepath.Join(bare, binaryName), DefaultDir},
		// Only the installed binary itself belongs to the installation.
		{renamed, DefaultDir},
		// A symlink (e.g. /usr/local/bin/gpuworks) reaches what it points at.
		{link, mustEvalSymlinks(t, withConfig)},
	} {
		if got := DirOf(tc.exe); got != tc.want {
			t.Errorf("DirOf(%s) = %s, want %s", tc.exe, got, tc.want)
		}
	}
}
