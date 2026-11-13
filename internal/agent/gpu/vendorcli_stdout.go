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
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdin = stdin
		cmd.Stderr = &stderr
		return cmd
	})
	if err != nil {
		slog.Warn("structured gpu command failed", "command", name, "stderr_tail", stderr.String())
	}
	return out, err
}
