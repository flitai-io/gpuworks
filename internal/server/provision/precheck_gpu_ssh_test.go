package provision

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestPrecheckSSHWithKunlunSDKOutsidePath(t *testing.T) {
	sdk := t.TempDir()
	writeExecutable(t, filepath.Join(sdk, "xpu-smi"), `[ "$*" = "-L" ] || exit 2
printf '%s\n' 'XPU 0: P800 PCIe' 'XPU 1: P800 PCIe'`)
	for _, tc := range []struct {
		name        string
		intelOnPath bool
	}{
		{"SDK only", false},
		{"Intel xpu-smi on PATH", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if tc.intelOnPath {
				writeExecutable(t, filepath.Join(bin, "xpu-smi"), `echo 'unsupported Intel XPU Manager option: -L' >&2
exit 1`)
			}
			report := precheckGPUOnLinux(t, func(cmd string, next fakeSSHHandler, stdin io.Reader) (string, string, uint32) {
				cmd = strings.NewReplacer("/dev/xpuctrl", "/dev/null", "/usr/local/xpu/bin", sdk).Replace(cmd)
				return next("PATH="+shellQuote(bin)+":$PATH\n"+cmd, stdin)
			})
			wantCheck(t, report.Checks, CheckIDGPU, "pass", "XPU 0: P800 PCIe", "XPU 1: P800 PCIe")
		})
	}
}

func TestPrecheckSSHReturnsOtherChecksWhenGPUStalls(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	report := precheckGPUOnLinux(t, func(string, fakeSSHHandler, io.Reader) (string, string, uint32) {
		<-release
		return "late answer", "", 0
	})
	gpu := checkByName(t, report.Checks, CheckIDGPU)
	if gpu.Status != "skip" || gpu.Code != messages.CheckCommandNotRun || !strings.Contains(gpu.Evidence, "deadline") {
		t.Fatalf("stalled GPU check = %+v", gpu)
	}
	wantCheck(t, report.Checks, CheckIDOSArch, "pass")
	// Install directory probing runs on the same SSH connection after the batch.
	wantCheck(t, report.Checks, CheckIDInstallDir, "pass")
}

func precheckGPUOnLinux(t *testing.T, gpu func(string, fakeSSHHandler, io.Reader) (string, string, uint32)) SSHPrecheckReport {
	t.Helper()
	dir := realTempDir(t)
	handler := scenarioHandler(t, dirScenario{}, dir)
	host, port := startFakeSSHServer(t, "root", "pw", func(cmd string, stdin io.Reader) (string, string, uint32) {
		switch cmd {
		case "uname -s":
			return "Linux", "", 0
		case gpuProbeCommand():
			return gpu(cmd, handler, stdin)
		default:
			return handler(cmd, stdin)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	report, err := NewSSHPrechecker().PrecheckSSH(ctx, SSHPrecheckRequest{
		Target: "root@" + host, Port: port, InstallDir: dir, ConnectMode: "agent_dial",
		SSHAuth: SSHAuth{Mode: AuthPassword, Password: "pw"},
	})
	if err != nil || ctx.Err() != nil {
		t.Fatalf("precheck did not finish within the caller's deadline: err=%v context=%v", err, ctx.Err())
	}
	return report
}
