package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/version"
)

// installedTree lays out an installation the way `gpuworks install` does and
// returns server settings running from it.
func installedTree(t *testing.T) (*serverSettings, string, *int) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(dir, "gpuworks"):       "#!/bin/sh\necho 'gpuworks v1.0.0 (old, 2026-01-01)'\n",
		filepath.Join(dataDir, "master.key"): "key",
		serverconfig.Path(dir):               "listen:\n  console: [\"127.0.0.1:8800\"]\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	restarts := 0
	settings := newServerSettings(Config{ConfigPath: serverconfig.Path(dir), DataDir: dataDir}, func() { restarts++ },
		func(dest string) error { return os.WriteFile(dest, []byte("live database"), 0o600) })
	settings.executablePath = filepath.Join(dir, "gpuworks")
	return settings, dir, &restarts
}

// releaseArchive is a release archive whose binary reports releaseID and, as
// every release since console upgrades does, the console upgrade protocol.
func releaseArchive(t *testing.T, releaseID string) []byte {
	t.Helper()
	return releaseArchiveWith(t, releaseID, `  `+selfinstall.ConsoleUpgradeProtocolCommand+`) echo 1 ;;`+"\n")
}

// legacyReleaseArchive is a release from before console upgrades: it rejects
// the protocol subcommand like any subcommand it does not know.
func legacyReleaseArchive(t *testing.T, releaseID string) []byte {
	t.Helper()
	return releaseArchiveWith(t, releaseID, "")
}

func releaseArchiveWith(t *testing.T, releaseID, extraCases string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	script := []byte("#!/bin/sh\ncase \"$1\" in\n  version) echo 'gpuworks " + releaseID + " (c0ffee, 2026-10-11)' ;;\n" + extraCases +
		"  *) echo \"unknown subcommand \\\"$1\\\"\" >&2; exit 2 ;;\nesac\n")
	if err := tw.WriteHeader(&tar.Header{Name: "gpuworks-" + releaseID + "-linux-amd64/gpuworks", Mode: 0o755, Size: int64(len(script))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(script); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSelfUpgradeBlockers(t *testing.T) {
	settings, dir, _ := installedTree(t)
	if got := settings.SelfUpgradeBlocker(); got != "" {
		t.Fatalf("an installed, supervised server: blocker = %q", got)
	}
	noManager := newServerSettings(settings.cfg, nil, settings.snapshot)
	noManager.executablePath = settings.executablePath
	if got := noManager.SelfUpgradeBlocker(); got != "no_service_manager" {
		t.Fatalf("blocker = %q", got)
	}
	// The page names the installation in the command it offers instead.
	if got := noManager.InstallDir(); got != dir {
		t.Fatalf("install dir = %q, want %q", got, dir)
	}
	noConfigCfg := settings.cfg
	noConfigCfg.ConfigPath = ""
	noConfig := newServerSettings(noConfigCfg, settings.restart, settings.snapshot)
	if got := noConfig.SelfUpgradeBlocker(); got != "not_installed" {
		t.Fatalf("blocker = %q", got)
	}
	if got := noConfig.InstallDir(); got != "" {
		t.Fatalf("install dir without server.yaml = %q", got)
	}
	elsewhere := newServerSettings(settings.cfg, settings.restart, settings.snapshot)
	elsewhere.executablePath = filepath.Join(t.TempDir(), "gpuworks")
	if got := elsewhere.SelfUpgradeBlocker(); got != "not_installed_binary" {
		t.Fatalf("blocker = %q", got)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if got := settings.SelfUpgradeBlocker(); got != "not_writable" {
			t.Fatalf("blocker = %q", got)
		}
	}
}

func TestSelfUpgradeSwapsTheBinaryAndRestarts(t *testing.T) {
	settings, dir, restarts := installedTree(t)
	started, err := settings.Upgrade(context.Background(), bytes.NewReader(releaseArchive(t, "v2.0.0")), "admin", dto.ServerUpgradeSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if started.From != version.Version || started.To != "v2.0.0" || started.Backup == "" || *restarts != 1 {
		t.Fatalf("started = %+v, restarts = %d", started, *restarts)
	}
	binary, _ := os.ReadFile(filepath.Join(dir, "gpuworks"))
	if !bytes.Contains(binary, []byte("gpuworks v2.0.0")) {
		t.Fatalf("binary = %q", binary)
	}
	if _, err := os.Stat(started.Backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// The next start is the new binary's first.
	guard, err := selfinstall.GuardStart(selfinstall.NewLayout(dir, filepath.Join(dir, "data"), ""), "v2.0.0", selfinstall.ModeSystemd, time.Now())
	if err != nil || guard.Pending == nil || guard.Pending.To != "v2.0.0" || guard.Pending.By != "admin" {
		t.Fatalf("guard = %+v, %v", guard, err)
	}
}

func TestSelfUpgradeRefusals(t *testing.T) {
	saved := version.Version
	t.Cleanup(func() { version.Version = saved })
	version.Version = "v1.0.0"
	for name, tc := range map[string]struct {
		archive []byte
		code    string
	}{
		"same release":   {archive: releaseArchive(t, "v1.0.0"), code: api.ErrorServerAlreadyAtRelease},
		"local build":    {archive: releaseArchive(t, "dev"), code: api.ErrorServerUpgradeArchiveInvalid},
		"not an archive": {archive: []byte("<html>"), code: api.ErrorServerUpgradeArchiveInvalid},
		// It would never confirm or roll back its own start; the CLI installs it.
		"release from before console upgrades": {archive: legacyReleaseArchive(t, "v0.9.0"), code: api.ErrorServerUpgradeArchiveInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			settings, dir, restarts := installedTree(t)
			_, err := settings.Upgrade(context.Background(), bytes.NewReader(tc.archive), "admin", dto.ServerUpgradeSelection{})
			var settingsErr *api.ServerSettingsError
			if !errors.As(err, &settingsErr) || settingsErr.Code != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if *restarts != 0 {
				t.Fatal("a refused upgrade restarted the server")
			}
			binary, _ := os.ReadFile(filepath.Join(dir, "gpuworks"))
			if !bytes.Contains(binary, []byte("gpuworks v1.0.0")) {
				t.Fatalf("binary changed: %q", binary)
			}
		})
	}
	settings, _, _ := installedTree(t)
	settings.restart = nil
	_, err := settings.Upgrade(context.Background(), bytes.NewReader(releaseArchive(t, "v2.0.0")), "admin", dto.ServerUpgradeSelection{})
	var settingsErr *api.ServerSettingsError
	if !errors.As(err, &settingsErr) || settingsErr.Code != api.ErrorServerUpgradeUnavailable || settingsErr.Params["reason"] != "no_service_manager" {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateChannelIsKeptInServerYAML(t *testing.T) {
	settings, dir, _ := installedTree(t)
	if channel, editable := settings.UpdateChannel(); channel != version.Channel || !editable {
		t.Fatalf("default channel = %q editable %v, want the build's", channel, editable)
	}
	if err := settings.SetUpdateChannel("release"); err != nil {
		t.Fatal(err)
	}
	if channel, _ := settings.UpdateChannel(); channel != "release" {
		t.Fatalf("channel after the switch = %q", channel)
	}
	file, err := serverconfig.Load(serverconfig.Path(dir))
	if err != nil || file.UpdateChannel != "release" || len(file.Listen.Console) != 1 {
		t.Fatalf("server.yaml = %+v, %v", file, err)
	}
	var settingsErr *api.ServerSettingsError
	if err := settings.SetUpdateChannel("preview"); !errors.As(err, &settingsErr) || settingsErr.Code != api.ErrorUpdateChannelInvalid {
		t.Fatalf("unknown channel = %v", err)
	}
	unconfigured := newServerSettings(Config{}, nil, nil)
	if _, editable := unconfigured.UpdateChannel(); editable {
		t.Fatal("a server without server.yaml cannot keep a channel")
	}
	if err := unconfigured.SetUpdateChannel("release"); !errors.As(err, &settingsErr) || settingsErr.Code != api.ErrorUpdateChannelReadOnly {
		t.Fatalf("read-only switch = %v", err)
	}
	// The next start reads the saved channel.
	restarted := newServerSettings(Config{ConfigPath: serverconfig.Path(dir), UpdateChannel: file.UpdateChannel}, nil, nil)
	if channel, _ := restarted.UpdateChannel(); channel != "release" {
		t.Fatalf("channel after a restart = %q", channel)
	}
}

func TestAutoCheckUpdatesIsKeptInServerYAML(t *testing.T) {
	settings, dir, _ := installedTree(t)
	if on, editable := settings.AutoCheckUpdates(); !on || !editable {
		t.Fatalf("default = %v editable %v, want on", on, editable)
	}
	if err := settings.SetAutoCheckUpdates(false); err != nil {
		t.Fatal(err)
	}
	if on, _ := settings.AutoCheckUpdates(); on {
		t.Fatal("still on after the switch")
	}
	file, err := serverconfig.Load(serverconfig.Path(dir))
	if err != nil || file.AutoCheckUpdates == nil || *file.AutoCheckUpdates || len(file.Listen.Console) != 1 {
		t.Fatalf("server.yaml = %+v, %v", file, err)
	}
	unconfigured := newServerSettings(Config{}, nil, nil)
	var settingsErr *api.ServerSettingsError
	if err := unconfigured.SetAutoCheckUpdates(false); !errors.As(err, &settingsErr) || settingsErr.Code != api.ErrorUpdateAutoCheckReadOnly {
		t.Fatalf("read-only switch = %v", err)
	}
	if on, editable := unconfigured.AutoCheckUpdates(); !on || editable {
		t.Fatalf("a server without server.yaml = %v editable %v", on, editable)
	}
	restarted := newServerSettings(Config{ConfigPath: serverconfig.Path(dir), ManualUpdateChecks: true}, nil, nil)
	if on, _ := restarted.AutoCheckUpdates(); on {
		t.Fatal("a restart forgot the switch")
	}
	if err := settings.SetAutoCheckUpdates(true); err != nil {
		t.Fatal(err)
	}
	if file, err := serverconfig.Load(serverconfig.Path(dir)); err != nil || file.AutoCheckUpdates != nil {
		t.Fatalf("switching on must remove the key: %+v, %v", file, err)
	}
}

// The installation's name for the update channel is stable, differs between
// installations, and does not contain the secret it is derived from.
func TestInstallIDIsAOneWayDerivativeOfTheMasterSecret(t *testing.T) {
	a, again, b := installIDOf("secret-a"), installIDOf("secret-a"), installIDOf("secret-b")
	if a != again || a == b || len(a) != 16 || strings.Contains(a, "secret") {
		t.Fatalf("ids = %q %q %q", a, again, b)
	}
}
