package gpu

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
)

// RunVendorCLIStdout shares the tracked command lifecycle but keeps diagnostics
// out of XML or JSON stdout. Other callers retain combined output.
func RunVendorCLIStdout(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	var stderr limitedDiagnostic
	out, err := RunTracked(ctx, binaryKey(name), vendorCLITimeout, func(ctx context.Context) *exec.Cmd {
		return vendorStdoutCommand(ctx, stdin, &stderr, name, args...)
	})
	if err != nil {
		slog.Warn("structured gpu command failed", "command", name, "stderr_tail", stderr.String())
	}
	return out, err
}

// vendorStdoutCommand places the CLI where every other vendor CLI runs
// (cliCommand): on a Kubernetes node, in the host's mount namespace with the
// host's own binary, since the agent's image carries no vendor tools.
func vendorStdoutCommand(ctx context.Context, stdin io.Reader, stderr io.Writer, name string, args ...string) *exec.Cmd {
	cmd := cliCommand(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stderr = stderr
	return cmd
}
