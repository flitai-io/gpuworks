package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// On a Kubernetes node the agent's image has no vendor CLI: a fresh
// structured query (Kunlun's injection check reads xpu-smi -q -x this way)
// must run the host's binary in the host's mount namespace, as every other
// vendor CLI call does. Running it in the agent's own file system failed with
// "fork/exec /usr/local/bin/xpu-smi: no such file or directory" on a real
// P800 node, and no card was offered to the kubelet.
func TestVendorStdoutCommandRunsOnTheHostOnKubernetes(t *testing.T) {
	withHostRoot(t, "/usr/local/bin/xpu-smi")
	cmd := vendorStdoutCommand(context.Background(), nil, nil, "/usr/local/bin/xpu-smi", "-q", "-x")
	if got := strings.Join(cmd.Args, " "); got != "nsenter -t 1 -m -- /usr/local/bin/xpu-smi -q -x" {
		t.Fatalf("on the host = %s", got)
	}
	cliOnHost.Store(false)
	cmd = vendorStdoutCommand(context.Background(), nil, nil, "/usr/local/bin/xpu-smi", "-q", "-x")
	if got := strings.Join(cmd.Args, " "); got != "/usr/local/bin/xpu-smi -q -x" {
		t.Fatalf("off the host = %s", got)
	}
}

// Off Kubernetes the command runs as before: stdout is the answer and the
// diagnostics on stderr stay out of it; a failing CLI reports its error.
func TestRunVendorCLIStdoutKeepsStderrOutOfTheAnswer(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "fake-smi")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho '<xml/>'\necho 'driver warning' >&2\n[ \"$1\" = fail ] && exit 3\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := RunVendorCLIStdout(context.Background(), nil, tool, "-q")
	if err != nil || strings.TrimSpace(out) != "<xml/>" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, err := RunVendorCLIStdout(context.Background(), nil, tool, "fail"); err == nil {
		t.Fatal("a failing CLI returned no error")
	}
}
