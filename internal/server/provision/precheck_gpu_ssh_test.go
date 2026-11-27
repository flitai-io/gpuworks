package provision

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrecheckSSHWithKunlunSDKOutsidePath(t *testing.T) {
	sdk := t.TempDir()
	writeExecutable(t, filepath.Join(sdk, "xpu-smi"), `[ "$*" = "-L" ] || exit 2
printf '%s\n' 'XPU 0: P800 PCIe' 'XPU 1: P800 PCIe'`)
	report := precheckGPUOnLinux(t, func(cmd string, next fakeSSHHandler, stdin io.Reader) (string, string, uint32) {
		cmd = strings.NewReplacer("/dev/xpuctrl", "/dev/null", "/usr/local/xpu/bin", sdk).Replace(cmd)
		return next(cmd, stdin)
	})
	wantCheck(t, report.Checks, CheckIDGPU, "pass", "XPU 0: P800 PCIe", "XPU 1: P800 PCIe")
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
