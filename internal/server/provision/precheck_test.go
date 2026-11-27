package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShouldCheckServerReachability(t *testing.T) {
	if !shouldCheckServerReachability("agent_dial") {
		t.Fatal("agent_dial should check server reachability")
	}
	if !shouldCheckServerReachability("") {
		t.Fatal("empty connect mode should preserve the default agent_dial check")
	}
	if shouldCheckServerReachability("server_dial") {
		t.Fatal("server_dial should not require target-to-server reachability")
	}
}

func TestVMPrecheckCommands(t *testing.T) {
	checks := vmPrecheckCommands()
	if len(checks) != 2 {
		t.Fatalf("VM precheck command count = %d, want 2", len(checks))
	}
	if checks[0].id != CheckIDDocker || !strings.Contains(checks[0].command, "docker --version") || checks[0].required || !strings.Contains(checks[0].command, "udocker") {
		t.Fatalf("Docker check = %+v", checks[0])
	}
	if checks[1].id != CheckIDNvidiaToolkit || !strings.Contains(checks[1].command, "nvidia-ctk") || checks[1].required || !strings.Contains(checks[1].command, "Docker runtime") {
		t.Fatalf("toolkit check = %+v", checks[1])
	}
}

func TestFUSEPrecheckIsInformational(t *testing.T) {
	checks := basePrecheckCommands()
	var fuse *precheckCommand
	for i := range checks {
		if checks[i].id == CheckIDFUSE {
			fuse = &checks[i]
		}
	}
	if fuse == nil || !fuse.required || !strings.Contains(fuse.command, "optional unless udocker") || !strings.Contains(fuse.command, "else echo") {
		t.Fatalf("FUSE informational check = %+v", fuse)
	}
}

func TestPythonPrecheckSupportsNonInteractiveCondaPath(t *testing.T) {
	checks := basePrecheckCommands()
	var python *precheckCommand
	for i := range checks {
		if checks[i].id == CheckIDPython {
			python = &checks[i]
		}
	}
	if python == nil || !strings.Contains(python.command, "/root/miniconda3/bin/python3") || !strings.Contains(python.command, "/opt/conda/bin/python3") {
		t.Fatalf("python3 check = %+v", python)
	}
	// runveil, the default udocker launcher, needs no Python: a node without
	// it must not fail precheck.
	if python.required || !strings.Contains(python.command, "GPUWORKS_UDOCKER_LAUNCHER=udocker") {
		t.Fatalf("python3 must be advisory and say who needs it: %+v", python)
	}
}

func TestPrecheckCommandsForKindSkipsContainerChecks(t *testing.T) {
	if got := precheckCommandsForKind("container"); len(got) != 0 {
		t.Fatalf("container-specific checks = %+v, want none", got)
	}
	if got := precheckCommandsForKind("VM"); len(got) != 2 {
		t.Fatalf("VM-specific checks = %d, want 2", len(got))
	}
}

func TestAwaitSSHCommandDrainsRunBeforeReadingCanceledOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	closed := make(chan struct{})
	got, err := awaitSSHCommand(ctx, func() error {
		go func() {
			out.WriteString("remote command stopped")
			done <- errors.New("remote exit after close")
			close(closed)
		}()
		return nil
	}, done, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if got != "remote command stopped" {
		t.Fatalf("output = %q, want drained remote output", got)
	}
	<-closed
}

func TestAgentListenPortCheckUsesRequestedAddress(t *testing.T) {
	cmd := agentListenPortCheckCommand("127.0.0.1:3389")
	for _, want := range []string{"'127.0.0.1'", "3389", "s.bind((host, port))"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("listen-port command missing %q: %s", want, cmd)
		}
	}
	host, port, err := parseListenAddress(":19091")
	if err != nil || host != "0.0.0.0" || port != 19091 {
		t.Fatalf("parseListenAddress = %q/%d, %v", host, port, err)
	}
	if _, _, err := parseListenAddress(":invalid"); err == nil {
		t.Fatal("invalid listen port should fail")
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
		user   string
		port   int
		wantU  string
		wantH  string
		wantP  int
	}{
		{name: "user host port", target: "root@example.com:2222", wantU: "root", wantH: "example.com", wantP: 2222},
		{name: "fallback user", target: "example.com", user: "ubuntu", wantU: "ubuntu", wantH: "example.com", wantP: 22},
		{name: "fallback port", target: "ubuntu@example.com", port: 2200, wantU: "ubuntu", wantH: "example.com", wantP: 2200},
		{name: "ipv6", target: "root@[2001:db8::1]:2022", wantU: "root", wantH: "2001:db8::1", wantP: 2022},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, h, p, err := ParseTarget(tt.target, tt.user, tt.port)
			if err != nil {
				t.Fatal(err)
			}
			if u != tt.wantU || h != tt.wantH || p != tt.wantP {
				t.Fatalf("ParseTarget = %q/%q/%d, want %q/%q/%d", u, h, p, tt.wantU, tt.wantH, tt.wantP)
			}
		})
	}
}

func TestGPUProbeCommandValidatesCLIAndSupportsPPU(t *testing.T) {
	cmd := gpuProbeCommand()
	for _, want := range []string{"nvidia-smi -L 2>&1", "npu-smi info 2>&1", "ppu-smi -L 2>&1"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("gpu probe command missing %q: %s", want, cmd)
		}
	}
	if strings.Contains(cmd, "command -v nvidia-smi >/dev/null 2>&1; then") {
		t.Fatal("probe must not accept a stale nvidia-smi based on PATH alone")
	}
}

func TestGPUProbeKunlun(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"grep", "head"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(bin, "xpu-smi"), `[ "$*" = "-L" ] || exit 2
printf '%s\n' "$CARDS"
exit "$CLI_STATUS"`)

	const cards = "XPU 0: P800 PCIe (UUID: GPU-test-0)\nXPU 1: P800 PCIe (UUID: GPU-test-1)"
	const missing = "no supported GPU CLI found"
	probe := strings.ReplaceAll(gpuProbeCommand(), "/usr/local/xpu/bin", shellQuote(t.TempDir()))
	for _, tc := range []struct {
		name, control, output, want string
		status, wantExit            int
	}{
		{"P800 lists both cards", "/dev/null", cards, cards, 0, 0},
		{"same CLI without Kunlun driver", filepath.Join(bin, "absent"), cards, missing, 0, 1},
		{"regular file as control", filepath.Join(bin, "xpu-smi"), cards, missing, 0, 1},
		{"CLI fails after partial output", "/dev/null", cards, missing, 1, 1},
		{"CLI reports no cards", "/dev/null", "No devices found", missing, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := strings.ReplaceAll(probe, "/dev/xpuctrl", shellQuote(tc.control))
			cmd := exec.Command("/bin/sh", "-c", probe)
			cmd.Env = []string{"PATH=" + bin, "CARDS=" + tc.output, fmt.Sprintf("CLI_STATUS=%d", tc.status)}
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.wantExit || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("probe returned %v, %q; want exit %d, %q", err, out, tc.wantExit, tc.want)
			}
		})
	}
	t.Run("NVIDIA still takes precedence", func(t *testing.T) {
		writeExecutable(t, filepath.Join(bin, "nvidia-smi"), `[ "$*" = "-L" ] && echo 'GPU 0: NVIDIA'`)
		cmd := exec.Command("/bin/sh", "-c", strings.ReplaceAll(probe, "/dev/xpuctrl", "/dev/null"))
		cmd.Env = []string{"PATH=" + bin, "CARDS=" + cards, "CLI_STATUS=0"}
		if out, err := cmd.CombinedOutput(); err != nil || string(out) != "GPU 0: NVIDIA\n" {
			t.Fatalf("mixed host probe = %q, %v", out, err)
		}
	})
}

func TestParseTargetRequiresUserAndHost(t *testing.T) {
	if _, _, _, err := ParseTarget("", "", 0); err == nil {
		t.Fatal("empty target should fail")
	}
	if _, _, _, err := ParseTarget("example.com", "", 0); err == nil {
		t.Fatal("missing user should fail")
	}
}

// runDeviceAccess runs the device-access check as a shell would on the node:
// devices under root, `id` answering as uid with name alice, and `sudo`
// either granting passwordless use or refusing. The real stat and tr are
// kept on PATH behind the fakes.
func runDeviceAccess(t *testing.T, root string, uid int, sudo bool) (string, error) {
	t.Helper()
	bin := openTempDir(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("id", fmt.Sprintf(`case "$1" in -u) echo %d ;; -un) echo alice ;; *) exit 2 ;; esac`, uid))
	if sudo {
		write("sudo", `[ "$*" = "-n true" ] || exit 2`)
	} else {
		write("sudo", `echo "sudo: a password is required" >&2; exit 1`)
	}
	argv := []string{"sh", "-c", deviceAccessCommand(root)}
	if os.Geteuid() == 0 {
		// Root reads and writes every file whatever its mode, so the check
		// runs as an unprivileged account to see a denial at all.
		setpriv, err := exec.LookPath("setpriv")
		if err != nil {
			t.Skip("running as root without setpriv: a permission denial cannot be observed")
		}
		argv = append([]string{setpriv, "--reuid=65534", "--regid=65534", "--clear-groups"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// openTempDir is a temporary directory every account can enter: the check
// runs as an unprivileged account when the test runs as root, and
// t.TempDir's parent is closed to everyone but its owner.
func openTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gwprecheck")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A non-root agent opens the accelerator device nodes as itself under
// runveil, so precheck names each one the SSH user cannot read and write,
// with its group, and the usermod an administrator runs. It is a warning:
// Docker opens the devices as root and may never need this. Root and
// passwordless sudo install a root agent, which supplies the groups itself.
func TestDeviceAccessPrecheckNamesTheDevicesARootlessAgentCannotOpen(t *testing.T) {
	root := openTempDir(t)
	for _, dir := range []string{"nvidia-caps", "dri"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	device := func(name string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, nil, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	device("nvidiactl", 0o666)
	device("nvidia0", 0o666)
	device("nvidia-uvm", 0o666)
	// A render node an NVIDIA host's display stack owns is never given to a
	// CUDA workload (found unreadable on an Aliyun A10, 2026-10-17).
	device("dri/renderD128", 0o000)

	out, err := runDeviceAccess(t, root, 1000, false)
	if err != nil || !strings.Contains(out, "alice can read and write 3 accelerator device nodes") {
		t.Fatalf("all readable: err=%v out=%q", err, out)
	}

	// The test's own files are the test user's; a mode that shuts out
	// even the owner stands in for a device owned by root:video 0660.
	device("nvidia1", 0o000)
	out, err = runDeviceAccess(t, root, 1000, false)
	if err == nil {
		t.Fatalf("a device the user cannot open passed: %q", out)
	}
	for _, want := range []string{"cannot read and write: nvidia1 (group ", "sudo usermod -aG ", " alice", "runveil"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "nvidia0") || strings.Contains(out, "nvidia-caps") || strings.Contains(out, "renderD128") {
		t.Fatalf("output names a device the user can open, or a directory: %q", out)
	}

	if out, err := runDeviceAccess(t, root, 0, false); err != nil || !strings.HasPrefix(out, "root:") {
		t.Fatalf("root: err=%v out=%q", err, out)
	}
	if out, err := runDeviceAccess(t, root, 1000, true); err != nil || !strings.HasPrefix(out, "passwordless sudo:") {
		t.Fatalf("sudo: err=%v out=%q", err, out)
	}
	// A Hygon host's workloads are given its render nodes, so there an
	// unreadable one is named.
	hygon := openTempDir(t)
	if err := os.MkdirAll(filepath.Join(hygon, "dri"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"kfd": 0o666, "dri/card0": 0o666, "dri/renderD128": 0o000} {
		path := filepath.Join(hygon, name)
		if err := os.WriteFile(path, nil, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := runDeviceAccess(t, hygon, 1000, false); err == nil || !strings.Contains(out, "cannot read and write: dri/renderD128 (group ") || strings.Contains(out, "kfd") {
		t.Fatalf("hygon: err=%v out=%q", err, out)
	}

	if out, err := runDeviceAccess(t, openTempDir(t), 1000, false); err != nil || out != "no accelerator device nodes found" {
		t.Fatalf("no devices: err=%v out=%q", err, out)
	}

	var check *precheckCommand
	for _, c := range basePrecheckCommands() {
		if c.id == CheckIDDeviceAccess {
			c := c
			check = &c
		}
	}
	if check == nil || check.required || !strings.Contains(check.command, "D='/dev'") {
		t.Fatalf("device access precheck = %+v, want an advisory check of /dev", check)
	}
}

func TestDeviceAccessPrecheckChecksOnlyExistingAscendSharedNodes(t *testing.T) {
	root := openTempDir(t)
	device := func(name string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, nil, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"davinci0", "davinci_manager", "ummu", "uburma"} {
		device(name, 0o666)
	}
	out, err := runDeviceAccess(t, root, 1000, false)
	if err != nil || !strings.Contains(out, "alice can read and write 4 accelerator device nodes") {
		t.Fatalf("existing Ascend nodes: err=%v out=%q", err, out)
	}

	device("ummu", 0o000)
	out, err = runDeviceAccess(t, root, 1000, false)
	if err == nil || !strings.Contains(out, "cannot read and write: ummu") {
		t.Fatalf("unreadable ummu: err=%v out=%q", err, out)
	}
	for _, absent := range []string{"devmm_svm", "hisi_hdc"} {
		if strings.Contains(out, absent) {
			t.Fatalf("missing shared node %q was reported: %q", absent, out)
		}
	}
}
