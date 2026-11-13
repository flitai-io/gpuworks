package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withHostRoot points hostRoot at a fresh directory holding files (each
// created empty and executable) and switches cliOnHost on for the test's
// duration, returning the directory.
func withHostRoot(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot := hostRoot
	hostRoot = root
	cliOnHost.Store(true)
	t.Cleanup(func() { hostRoot = oldRoot; cliOnHost.Store(false) })
	return root
}

func TestReadToolReadsOnTheHost(t *testing.T) {
	root := withHostRoot(t)
	path := "/usr/local/Ascend/driver/version.info"
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("host content"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTool(path)
	if err != nil || string(got) != "host content" {
		t.Fatalf("readTool(on host) = %q, %v", got, err)
	}
	cliOnHost.Store(false)
	if _, err := readTool(path); err == nil {
		t.Fatal("readTool off the host found a file that only exists under hostRoot")
	}
}

// TestHostCommandRoutesThroughCliCommand: HostCommand is what callers
// outside this package (the doctor's python3 diagnostics) use to place a
// process the way a vendor CLI runs; it must agree with cliCommand both on
// and off the host.
func TestHostCommandRoutesThroughCliCommand(t *testing.T) {
	withHostRoot(t, "/usr/bin/python3")
	if !CLIsOnHost() {
		t.Fatal("CLIsOnHost did not reflect cliOnHost after withHostRoot turned it on")
	}
	if got := strings.Join(HostCommand(context.Background(), "python3", "-c", "1").Args, " "); got != "nsenter -t 1 -m -- /usr/bin/python3 -c 1" {
		t.Fatalf("HostCommand on the host = %s", got)
	}
	cliOnHost.Store(false)
	if CLIsOnHost() {
		t.Fatal("CLIsOnHost did not reflect cliOnHost after it was turned off")
	}
	if got := strings.Join(HostCommand(context.Background(), "python3", "-c", "1").Args, " "); got != "python3 -c 1" {
		t.Fatalf("HostCommand off the host = %s", got)
	}
}

func TestCLIsOnHost(t *testing.T) {
	root := t.TempDir()
	for path, mode := range map[string]os.FileMode{
		"/usr/local/bin/ppu-smi":                   0o755,
		"/usr/local/Ascend/driver/tools/hccn_tool": 0o755,
		"/usr/bin/not-executable":                  0o644,
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	// Hygon's hy-smi is an absolute link into the driver's tree, and the
	// tree's own link is relative.
	for link, target := range map[string]string{
		"/usr/local/bin/hy-smi":       "/usr/local/hyhal/bin/hy-smi",
		"/usr/local/hyhal/bin/hy-smi": "hy-smi.real",
		"/usr/bin/loop-a":             "/usr/bin/loop-b",
		"/usr/bin/loop-b":             "/usr/bin/loop-a",
	} {
		full := filepath.Join(root, link)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "/usr/local/hyhal/bin/hy-smi.real"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	oldRoot := hostRoot
	hostRoot = root
	cliOnHost.Store(true)
	t.Cleanup(func() { hostRoot = oldRoot; cliOnHost.Store(false) })

	if p, err := lookCLI("ppu-smi"); err != nil || p != "/usr/local/bin/ppu-smi" {
		t.Fatalf("lookCLI(ppu-smi) = %q, %v", p, err)
	}
	if p, err := lookCLI("hccn_tool"); err != nil || p != "/usr/local/Ascend/driver/tools/hccn_tool" {
		t.Fatalf("lookCLI(hccn_tool) = %q, %v", p, err)
	}
	if p, err := lookCLI("hy-smi"); err != nil || p != "/usr/local/bin/hy-smi" {
		t.Fatalf("lookCLI(hy-smi) through host links = %q, %v", p, err)
	}
	// An absolute path (DTK's /opt/dtk/bin/hy-smi, ldconfig found earlier)
	// is looked up on the host as well, not in the agent's own image.
	if p, err := lookCLI("/usr/local/bin/ppu-smi"); err != nil || p != "/usr/local/bin/ppu-smi" {
		t.Fatalf("lookCLI(absolute host path) = %q, %v", p, err)
	}
	for _, missing := range []string{"npu-smi", "not-executable", "loop-a", "/usr/bin/not-executable", "/opt/dtk/bin/hy-smi"} {
		if _, err := lookCLI(missing); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("lookCLI(%s) = %v", missing, err)
		}
	}
	cmd := cliCommand(context.Background(), "ppu-smi", "-L")
	if got := strings.Join(cmd.Args, " "); got != "nsenter -t 1 -m -- /usr/local/bin/ppu-smi -L" {
		t.Fatalf("command = %s", got)
	}
	cliOnHost.Store(false)
	if got := strings.Join(cliCommand(context.Background(), "ppu-smi", "-L").Args, " "); got != "ppu-smi -L" {
		t.Fatalf("command off the host = %s", got)
	}
}

// TestNVIDIAProbeFindsTheHostsNvidiaSmi: an agent pod under the node's
// default runc (k3s) has no nvidia-smi of its own; the probe must find and
// run the host's instead of reporting cards without a driver.
func TestNVIDIAProbeFindsTheHostsNvidiaSmi(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	smi := filepath.Join(root, "/usr/bin/nvidia-smi")
	if err := os.MkdirAll(filepath.Dir(smi), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(smi, []byte("#!/bin/sh\necho 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// nsenter runs the host path under the fake host root.
	nsenter := "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done; shift\nexec " + root + "\"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "nsenter"), []byte(nsenter), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	oldRoot := hostRoot
	hostRoot = root
	t.Cleanup(func() { hostRoot = oldRoot; cliOnHost.Store(false) })

	if (&SMIProber{}).Available() {
		t.Fatal("nvidia-smi found with CLIs in the container and none there")
	}
	cliOnHost.Store(true)
	if !(&SMIProber{}).Available() {
		t.Fatal("the host's nvidia-smi was not found and run")
	}
}

func symlinkUnder(t *testing.T, root, link, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, link)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
		t.Fatal(err)
	}
}

// Through /proc/1/root an absolute link anywhere in a path would point into
// the agent's own file system; every component is resolved on the host.
func TestHostReadsFollowLinksInTheMiddleOfAPath(t *testing.T) {
	root := withHostRoot(t, "opt/rocm-7.2.0/.info/version", "run/cdi/a.json", "usr/lib/libx.so")
	if err := os.WriteFile(filepath.Join(root, "opt/rocm-7.2.0/.info/version"), []byte("7.2.0-43\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// /opt/rocm -> /etc/alternatives/rocm -> /opt/rocm-7.2.0, as the ROCm
	// packages lay it out; /var/run -> /run as on Ubuntu; a relative link.
	symlinkUnder(t, root, "opt/rocm", "/etc/alternatives/rocm")
	symlinkUnder(t, root, "etc/alternatives/rocm", "/opt/rocm-7.2.0")
	symlinkUnder(t, root, "var/run", "/run")
	symlinkUnder(t, root, "lib", "usr/lib")
	symlinkUnder(t, root, "run/xdrdrv", "/dev/null")
	if raw, err := readTool("/opt/rocm/.info/version"); err != nil || string(raw) != "7.2.0-43\n" {
		t.Fatalf("readTool through two absolute links = %q, %v", raw, err)
	}
	if fi, err := statTool("/var/run/cdi/a.json"); err != nil || fi.IsDir() {
		t.Fatalf("statTool under /var/run = %v, %v", fi, err)
	}
	if fi, err := statTool("/lib/libx.so"); err != nil || fi.IsDir() {
		t.Fatalf("statTool through a relative link = %v, %v", fi, err)
	}
	if got := globTool("/var/run/cdi/*.json"); len(got) != 1 || got[0] != "/var/run/cdi/a.json" {
		t.Fatalf("globTool under /var/run = %v", got)
	}
	// lstatTool resolves the directories but not the last link.
	if fi, err := lstatTool("/var/run/xdrdrv"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("lstatTool of a link behind /var/run = %v, %v", fi, err)
	}
	if _, err := readTool("/opt/rocm/.info/missing"); err == nil {
		t.Fatal("a missing file read as present")
	}
	symlinkUnder(t, root, "loop/a", "/loop/b")
	symlinkUnder(t, root, "loop/b", "/loop/a")
	if _, err := readTool("/loop/a/x"); err == nil {
		t.Fatal("a link loop resolved")
	}
}
