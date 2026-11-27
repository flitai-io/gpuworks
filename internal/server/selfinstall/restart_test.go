package selfinstall

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Without --addr a fresh install binds the address of the default route, so
// the console is on the management network rather than every interface.
func TestInstallBindsTheDefaultRouteAddressWhenNoneIsGiven(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("binary")
	req := h.quiet()
	req.Addr, req.AddrSet = "", false
	// The detected address is not on this machine, so preflight's bind check
	// has to be told the port is ours.
	h.writeExistingInstall("v1.0.0")
	if err := os.Remove(h.layout.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	report := h.mustInstall(req)
	if got := h.config().Listen.Console; len(got) != 1 || got[0] != "192.0.2.10:8800" {
		t.Fatalf("listen = %v", got)
	}
	if report.URL != "http://192.0.2.10:8800" || report.HealthURL != "http://127.0.0.1:8800/healthz" {
		t.Fatalf("report = %q, %q", report.URL, report.HealthURL)
	}
}

func TestInstallTakesAnAddrWithoutAHostAsEveryAddress(t *testing.T) {
	l, err := listenForAddr(":9100")
	if err != nil || len(l.Console) != 1 || l.Console[0] != "0.0.0.0:9100" {
		t.Fatalf("listen = %+v, %v", l, err)
	}
	if _, err := listenForAddr("example.com:80"); err == nil || !strings.Contains(err.Error(), "invalid --addr") {
		t.Fatalf("hostname accepted: %v", err)
	}
	if got := probeAddr(l); got != "127.0.0.1:9100" {
		t.Fatalf("probe = %q", got)
	}
}

// Moving a systemd installation to nohup removes the unit: its presence is
// what marks the systemd mode, so a stale unit would be read back as systemd.
func TestInstallRemovesTheUnitWhenMovingToNohup(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.installSelf("binary v2")
	req := h.quiet()
	req.NoSystemd = true

	report := h.mustInstall(req)
	if report.Mode != ModeNohup {
		t.Fatalf("mode = %q", report.Mode)
	}
	if !h.run.ranInOrder("sudo systemctl stop gpuworks", "sudo systemctl disable gpuworks", "sudo rm -f "+h.layout.UnitPath()) {
		t.Fatalf("the unit was not retired: %v", h.run.executed())
	}
	if _, err := os.Stat(h.layout.UnitPath()); !os.IsNotExist(err) {
		t.Fatalf("unit still present: %v", err)
	}
	if inst, err := h.installer.readInstallation(context.Background()); err != nil || inst.Mode != ModeNohup {
		t.Fatalf("installation reads back as %+v, %v", inst, err)
	}
}

func TestRestartUsesTheInstallationsServiceMode(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	mode, err := h.installer.Restart(context.Background())
	if err != nil || mode != ModeSystemd || !h.run.ran("sudo systemctl restart gpuworks") {
		t.Fatalf("systemd restart = %q, %v: %v", mode, err, h.run.executed())
	}

	h2 := newHarness(t, t.TempDir())
	h2.writeExistingInstall("v1.0.0")
	h2.supervisedByNohup()
	mode, err = h2.installer.Restart(context.Background())
	if err != nil || mode != ModeNohup || len(h2.starts) != 1 || h2.run.ran("sudo systemctl") {
		t.Fatalf("nohup restart = %q, %v, starts %v, ran %v", mode, err, h2.starts, h2.run.executed())
	}
	if got := strings.Join(h2.starts[0], " "); got != h2.layout.Binary()+" serve --supervise --config "+h2.layout.ConfigPath() {
		t.Fatalf("started %q", got)
	}

	h3 := newHarness(t, t.TempDir())
	h3.writeExistingInstall("v1.0.0")
	h3.healthErr = errors.New("connection refused")
	if _, err := h3.installer.Restart(context.Background()); err == nil || !strings.Contains(h3.errOut.String(), h3.layout.LogFile()) {
		t.Fatalf("an unhealthy restart = %v; stderr %s", err, h3.errOut.String())
	}
	if _, err := newHarness(t, t.TempDir()).installer.Restart(context.Background()); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("restart without an installation = %v", err)
	}
}

// A restart stops first and checks the address, so another server answering
// /healthz there is not taken for this one coming back.
func TestRestartRefusesAnAddressStillHeldAfterTheStop(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.supervisedByNohup()
	addr := holdAddr(t)
	h.listensOn(addr)

	_, err := h.installer.Restart(context.Background())

	if err == nil || !strings.Contains(err.Error(), addr+" is still in use") {
		t.Fatalf("restart = %v, want the held address reported", err)
	}
	if len(h.starts) != 0 {
		t.Fatalf("detached starts = %v, want none", h.starts)
	}
}

// The restarted server must report the installed binary's release: a
// different version answering is some other server.
func TestRestartWaitsForTheInstalledRelease(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.healthVersion = "v0.9.0"

	if _, err := h.installer.Restart(context.Background()); err == nil || !strings.Contains(err.Error(), "reports version v0.9.0, not v1.0.0") {
		t.Fatalf("restart = %v", err)
	}
}
