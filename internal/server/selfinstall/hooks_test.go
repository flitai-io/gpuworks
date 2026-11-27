package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// hookLog records the hook calls of one test, across hooks, in order.
type hookLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *hookLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *hookLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// fakeHook is an InstallHook that records its calls, runs a command through
// the runner it is handed (so a test sees it among the installer's commands)
// and fails when told to.
type fakeHook struct {
	name          string
	log           *hookLog
	groups        []string
	failInstall   error
	failUninstall error
	dropIn        string
}

func (f *fakeHook) Install(ctx context.Context, run Runner, layout Layout, serviceUser string) error {
	f.log.add(fmt.Sprintf("install %s user=%s dir=%s", f.name, serviceUser, layout.Dir))
	if f.failInstall != nil {
		return f.failInstall
	}
	if f.dropIn != "" {
		if err := WriteDropIn(ctx, run, layout, f.name, f.dropIn); err != nil {
			return err
		}
	}
	return run.Run(ctx, Command{Path: "hook-" + f.name, Args: []string{"install"}, Sudo: true})
}

func (f *fakeHook) Uninstall(ctx context.Context, run Runner, layout Layout) error {
	f.log.add("uninstall " + f.name)
	if f.failUninstall != nil {
		return f.failUninstall
	}
	if f.dropIn != "" {
		if err := RemoveDropIn(ctx, run, layout, f.name); err != nil {
			return err
		}
	}
	return run.Run(ctx, Command{Path: "hook-" + f.name, Args: []string{"uninstall"}, Sudo: true})
}

// groupHook is a fakeHook that asks for supplementary groups.
type groupHook struct{ *fakeHook }

func (g groupHook) ServiceGroups() []string { return g.groups }

func TestRenderUnitSupplementaryGroupsGolden(t *testing.T) {
	unit := RenderUnit(UnitConfig{
		Binary:              "/opt/gpuworks/server/gpuworks",
		ConfigPath:          "/opt/gpuworks/server/server.yaml",
		DataDir:             "/opt/gpuworks/server/data",
		ServiceUser:         "gpuworks",
		ServiceGroup:        DefaultCredentialGroup,
		SupplementaryGroups: []string{"ukey", "plugdev"},
	})
	want := `[Unit]
Description=gpuworks control plane
Documentation=https://flitai.io/docs/gpuworks
After=network-online.target
RequiresMountsFor=/opt/gpuworks/server/data

[Service]
Type=simple
User=gpuworks
Group=` + DefaultCredentialGroup + `
SupplementaryGroups=ukey plugdev
ExecStart=/opt/gpuworks/server/gpuworks serve --config /opt/gpuworks/server/server.yaml
ExecStartPre=/usr/bin/test -x /opt/gpuworks/server/gpuworks
Restart=always
RestartSec=3
TimeoutStopSec=30
Delegate=yes

[Install]
WantedBy=multi-user.target
`
	if unit != want {
		t.Fatalf("unit:\n%s\nwant:\n%s", unit, want)
	}
	// Without groups the unit is what it always was: no empty assignment,
	// which systemd would read as a reset.
	plain := RenderUnit(UnitConfig{Binary: "/b", ConfigPath: "/c", DataDir: "/d", ServiceUser: "gpuworks"})
	if strings.Contains(plain, "SupplementaryGroups") {
		t.Fatalf("a unit without groups names them:\n%s", plain)
	}
}

func TestInstallRunsHooksInOrderBeforeTheService(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("gpuworks binary v1")
	log := &hookLog{}
	first := &fakeHook{name: "first", log: log, dropIn: "[Service]\nDeviceAllow=char-usb_device rw\n"}
	second := groupHook{&fakeHook{name: "second", log: log, groups: []string{"ukey", "plugdev", "ukey"}}}
	h.installer.WithHooks(first, second)
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser

	h.mustInstall(req)

	want := []string{
		"install first user=" + DefaultServiceUser + " dir=" + h.layout.Dir,
		"install second user=" + DefaultServiceUser + " dir=" + h.layout.Dir,
	}
	if got := log.list(); !slices.Equal(got, want) {
		t.Fatalf("hook calls = %q, want %q", got, want)
	}
	// After the account exists, before the unit is loaded and started.
	if !h.run.ranInOrder("sudo useradd", "sudo hook-first install", "sudo hook-second install",
		"sudo systemctl daemon-reload", "sudo systemctl enable gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("hook order: %v", h.run.executed())
	}
	dropIn, ok := h.run.writtenFile(h.layout.DropInPath("first"))
	if !ok || dropIn != first.dropIn || !strings.HasSuffix(h.layout.DropInPath("first"), "/gpuworks.service.d/first.conf") {
		t.Fatalf("drop-in = %q (%v) at %s", dropIn, ok, h.layout.DropInPath("first"))
	}
	if !h.run.ran("sudo chmod 0644 " + h.layout.DropInPath("first")) {
		t.Fatalf("the drop-in mode was not set: %v", h.run.executed())
	}
	if unit := h.unitFile(); !strings.Contains(unit, "\nSupplementaryGroups=ukey plugdev\n") {
		t.Fatalf("unit:\n%s", unit)
	}
}

func TestInstallStopsAtAFailingHook(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("gpuworks binary v1")
	log := &hookLog{}
	failing := &fakeHook{name: "ukey", log: log, failInstall: errors.New("no udev on this host")}
	later := &fakeHook{name: "later", log: log}
	h.installer.WithHooks(failing, later)

	_, err := h.installer.Install(context.Background(), h.quiet())
	if err == nil || !strings.Contains(err.Error(), "install step 1") || !strings.Contains(err.Error(), "no udev on this host") {
		t.Fatalf("err = %v", err)
	}
	if got := log.list(); len(got) != 1 || !strings.HasPrefix(got[0], "install ukey") {
		t.Fatalf("hooks after the failure ran: %q", got)
	}
	if h.run.ran("sudo systemctl enable") || h.run.ran("sudo systemctl restart") {
		t.Fatalf("the service was set up after a failed hook: %v", h.run.executed())
	}
	if _, ok := h.run.writtenFile(h.layout.UnitPath()); ok || fileExists(h.layout.UnitPath()) {
		t.Fatal("the unit was written after a failed hook")
	}
}

// Without hooks nothing about install changes: no hook command, no groups.
func TestInstallWithoutHooksIsUnchanged(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installSelf("gpuworks binary v1")
	h.mustInstall(h.quiet())
	if unit := h.unitFile(); strings.Contains(unit, "SupplementaryGroups") {
		t.Fatalf("unit:\n%s", unit)
	}
	for _, cmd := range h.run.executed() {
		if strings.Contains(cmd, "hook-") || strings.Contains(cmd, ".service.d") {
			t.Fatalf("a community install ran %q", cmd)
		}
	}
}

// Detached installs have no unit and no account of their own: hooks still
// run, told so with an empty service user, and nothing names groups.
func TestInstallNohupRunsHooksWithoutAServiceUser(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.systemd = false
	h.installSelf("gpuworks binary v1")
	log := &hookLog{}
	h.installer.WithHooks(groupHook{&fakeHook{name: "g", log: log, groups: []string{"ukey"}}})
	report := h.mustInstall(h.quiet())
	if report.Mode != ModeNohup {
		t.Fatalf("mode = %s", report.Mode)
	}
	if got := log.list(); len(got) != 1 || !strings.HasPrefix(got[0], "install g user= dir=") {
		t.Fatalf("hook calls = %q", got)
	}
}

func TestUpgradeReappliesHooksBeforeStoppingTheService(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("new binary"))
	log := &hookLog{}
	h.installer.WithHooks(groupHook{&fakeHook{name: "ukey", log: log, groups: []string{"ukey"}}})

	if _, err := h.installer.Upgrade(context.Background(), archiveReq(archive)); err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if got := log.list(); len(got) != 1 || !strings.HasPrefix(got[0], "install ukey user="+DefaultServiceUser) {
		t.Fatalf("hook calls = %q", got)
	}
	if !h.run.ranInOrder("sudo hook-ukey install", "sudo systemctl daemon-reload", "sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("upgrade order: %v", h.run.executed())
	}
	if unit := h.unitFile(); !strings.Contains(unit, "\nSupplementaryGroups=ukey\n") {
		t.Fatalf("the unit was not rewritten with the hook's group:\n%s", unit)
	}
}

func TestUpgradeStopsAtAFailingHookWithTheOldBinary(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("new binary"))
	h.installer.WithHooks(&fakeHook{name: "ukey", log: &hookLog{}, failInstall: errors.New("udevadm failed")})

	_, err := h.installer.Upgrade(context.Background(), archiveReq(archive))
	if err == nil || !strings.Contains(err.Error(), "udevadm failed") || !strings.Contains(err.Error(), "not replaced") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" || h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("binary = %q, commands = %v", got, h.run.executed())
	}
}

// An installer without hooks leaves the unit as install wrote it (the
// server binary always has its own hook, so its upgrades rewrite the unit).
func TestUpgradeWithoutHooksLeavesTheUnit(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.offline()
	archive := h.writeArchive("v2.0.0", []byte("new binary"))
	if _, err := h.installer.Upgrade(context.Background(), archiveReq(archive)); err != nil {
		t.Fatal(err)
	}
	if h.run.ran("sudo systemctl daemon-reload") || h.run.ran("sudo systemctl enable gpuworks") {
		t.Fatalf("an upgrade without hooks rewrote the unit: %v", h.run.executed())
	}
}

func TestUninstallReversesHooksBeforeDeletingFiles(t *testing.T) {
	h := uninstallFixture(t)
	log := &hookLog{}
	h.installer.WithHooks(
		&fakeHook{name: "first", log: log, dropIn: "[Service]\n"},
		&fakeHook{name: "second", log: log},
	)
	if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.list(); !slices.Equal(got, []string{"uninstall second", "uninstall first"}) {
		t.Fatalf("hook calls = %q", got)
	}
	if !h.run.ranInOrder("sudo systemctl stop gpuworks", "sudo rm -f "+h.layout.UnitPath(), "sudo hook-second uninstall",
		"sudo rm -f "+h.layout.DropInPath("first"), "sudo hook-first uninstall") {
		t.Fatalf("uninstall order: %v", h.run.executed())
	}
	assertUninstallAbsent(t, h.layout.Binary())
}

func TestUninstallStopsAtAFailingHookAndKeepsTheProgram(t *testing.T) {
	h := uninstallFixture(t)
	log := &hookLog{}
	h.installer.WithHooks(&fakeHook{name: "first", log: log}, &fakeHook{name: "second", log: log, failUninstall: errors.New("udev rule is busy")})
	_, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "undo install step 2") || !strings.Contains(err.Error(), "udev rule is busy") {
		t.Fatalf("err = %v", err)
	}
	if got := log.list(); !slices.Equal(got, []string{"uninstall second"}) {
		t.Fatalf("hook calls = %q", got)
	}
	// The configuration and the program survive, so a retry finds the
	// installation and repeats the hooks.
	if !fileExists(h.layout.Binary()) || !fileExists(h.layout.ConfigPath()) {
		t.Fatal("files were deleted after a failed hook")
	}
}

func TestUninstallDryRunRunsNoHook(t *testing.T) {
	h := uninstallFixture(t)
	log := &hookLog{}
	h.installer.WithHooks(&fakeHook{name: "first", log: log})
	if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, DryRun: true}, nil); err != nil {
		t.Fatal(err)
	}
	if got := log.list(); len(got) != 0 {
		t.Fatalf("a dry run ran hooks: %q", got)
	}
}

func TestDropInRefusesBadNamesAndRunnerFailures(t *testing.T) {
	run := newFakeRunner()
	layout := Layout{UnitDir: t.TempDir()}
	ctx := context.Background()
	for _, name := range []string{"", "../escape", "Upper", "a/b", strings.Repeat("a", 65)} {
		if err := WriteDropIn(ctx, run, layout, name, "x"); err == nil {
			t.Errorf("WriteDropIn(%q) accepted a bad name", name)
		}
		if err := RemoveDropIn(ctx, run, layout, name); err == nil {
			t.Errorf("RemoveDropIn(%q) accepted a bad name", name)
		}
	}
	if len(run.calls) != 0 {
		t.Fatalf("a refused name ran commands: %v", run.executed())
	}
	run.fail["sudo tee "+layout.DropInPath("ukey")] = errors.New("read-only file system")
	if err := WriteDropIn(ctx, run, layout, "ukey", "[Service]\n"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v", err)
	}
	if run.ran("sudo chmod") {
		t.Fatal("chmod ran after the write failed")
	}
	run.fail["sudo rm -f "+layout.DropInPath("ukey")] = errors.New("permission denied")
	if err := RemoveDropIn(ctx, run, layout, "ukey"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v", err)
	}
}
