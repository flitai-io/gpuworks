package gpu

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Absolute executable lookup must not bypass the existing stuck-driver guard.
func TestStructuredVendorCLIUsesSharedBinaryIdentity(t *testing.T) {
	name := "gw-structured-stuck-smi"
	dir := fakeCLI(t, name, "echo should-not-run\n")
	stuckCLIs.Lock()
	stuckCLIs.since[name] = time.Now()
	stuckCLIs.Unlock()
	t.Cleanup(func() {
		stuckCLIs.Lock()
		delete(stuckCLIs.since, name)
		stuckCLIs.Unlock()
	})
	out, err := RunVendorCLIStdout(t.Context(), nil, filepath.Join(dir, name))
	if !errors.Is(err, ErrStillRunning) || out != "" {
		t.Fatalf("stuck binary bypassed: output=%q error=%v", out, err)
	}
}
