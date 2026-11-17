package provision

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// unwritableDirHost answers like a host where the SSH user (uid 1000) has
// passwordless sudo, but the install directory sits on a mount neither sudo
// nor the user can write; only /tmp/gpuworks is writable.
func unwritableDirHost(t *testing.T) (host string, port int, cmds func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	handler := func(cmd string, _ io.Reader) (string, string, uint32) {
		mu.Lock()
		seen = append(seen, cmd)
		mu.Unlock()
		switch {
		case cmd == "uname -s":
			return "Linux\n", "", 0
		case strings.HasPrefix(cmd, "uname -m"):
			return "x86_64\n", "", 0
		case cmd == "id -u", cmd == "id -g":
			return "1000\n", "", 0
		case cmd == "sudo -n true":
			return "", "", 0
		case strings.HasPrefix(cmd, "sudo -n mkdir"):
			return "", "mkdir: cannot create directory: Read-only file system\n", 1
		case strings.HasPrefix(cmd, "mkdir -p '/tmp/gpuworks'"):
			return "", "", 0
		case strings.HasPrefix(cmd, "mkdir -p"):
			return "", "mkdir: Permission denied\n", 1
		}
		return "", "not available on this fake host\n", 127
	}
	host, port = startFakeSSHServer(t, "ubuntu", "pw", handler)
	return host, port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func installOnUnwritableDirHost(t *testing.T, dir string) (SSHInstallReport, error, []string) {
	t.Helper()
	host, port, cmds := unwritableDirHost(t)
	installer := NewSSHInstaller(func(string) ([]byte, error) { return nil, errors.New("agent binary is not available in this test") })
	report, err := installer.InstallSSH(t.Context(), SSHInstallRequest{
		Target: "ubuntu@" + host, Port: port, ServerURL: "https://server.example", Token: "tok", InstallDir: dir,
		InstanceKind: "container", SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	})
	return report, err, cmds()
}

// A directory the operator chose is where the agent goes or the install
// fails: it is never swapped for /tmp/gpuworks, which boot empties.
func TestInstallSSHNeverSwapsAChosenDirectoryForTheFallback(t *testing.T) {
	report, err, cmds := installOnUnwritableDirHost(t, "/data/gpuworks")
	if !errors.Is(err, ErrInstallDirNotWritable) || !strings.Contains(err.Error(), "/data/gpuworks") {
		t.Fatalf("install error = %v", err)
	}
	if report.InstallDir != "/data/gpuworks" {
		t.Fatalf("report install dir = %q", report.InstallDir)
	}
	wantCheck(t, report.Checks, "privilege", "fail", "cannot write the chosen install directory /data/gpuworks", "Read-only file system")
	for _, cmd := range cmds {
		if strings.Contains(cmd, "/tmp/gpuworks") || strings.Contains(cmd, "uname -m") {
			t.Fatalf("install went on after the chosen directory failed: %q", cmd)
		}
	}
}

// Without a choice the default directory still falls back to a writable one.
func TestInstallSSHFallsBackOnlyFromTheDefaultDirectory(t *testing.T) {
	report, err, _ := installOnUnwritableDirHost(t, "")
	if errors.Is(err, ErrInstallDirNotWritable) {
		t.Fatalf("default directory refused instead of falling back: %v", err)
	}
	if report.InstallDir != "/tmp/gpuworks" {
		t.Fatalf("report install dir = %q, want the fallback", report.InstallDir)
	}
	wantCheck(t, report.Checks, "privilege", "warn", "sudo cannot claim /opt/gpuworks", "/tmp/gpuworks")
}
