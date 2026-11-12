package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCUDARuntimeSmokeCommandFailure(t *testing.T) {
	original := cudaSmokeCommand
	t.Cleanup(func() { cudaSmokeCommand = original })
	cudaSmokeCommand = func(context.Context, string, string, ...string) ([]byte, error) {
		return []byte("libcuda missing"), errors.New("exit status 1")
	}
	err := CUDARuntimeSmoke(context.Background())
	if err == nil || !strings.Contains(err.Error(), "libcuda missing") {
		t.Fatalf("smoke error = %v", err)
	}
}

func TestCUDARuntimeSmokeRequiresSuccessMarker(t *testing.T) {
	original := cudaSmokeCommand
	t.Cleanup(func() { cudaSmokeCommand = original })
	cudaSmokeCommand = func(context.Context, string, string, ...string) ([]byte, error) {
		return []byte("unexpected output"), nil
	}
	err := CUDARuntimeSmoke(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no success marker") {
		t.Fatalf("smoke marker error = %v", err)
	}
}

// TestCUDASmokeCommandUsesTheDriverContainerLibraryPath: on a node whose
// driver lives only under nvidiaDriverContainerRoot (the GPU Operator's
// pattern), the smoke script's bare ctypes.CDLL("libcuda.so.1") would never
// find it without LD_LIBRARY_PATH naming that directory explicitly.
func TestCUDASmokeCommandUsesTheDriverContainerLibraryPath(t *testing.T) {
	logs := hostWith(t, "/usr/bin/python3", "/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1")
	if reason := CUDASmokePrerequisite(); reason != "" {
		t.Fatalf("CUDA prerequisite with the driver container's library = %q", reason)
	}
	if err := CUDARuntimeSmokeOn(context.Background(), "GPU-9"); err != nil {
		t.Fatalf("CUDARuntimeSmokeOn = %v", err)
	}
	env := readLog(t, logs, "env")
	want := "LD_LIBRARY_PATH=/run/nvidia/driver/usr/lib/x86_64-linux-gnu"
	if !strings.Contains("\n"+env, "\n"+want+"\n") {
		t.Fatalf("%s is not in the environment passed through nsenter:\n%s", want, env)
	}
}
