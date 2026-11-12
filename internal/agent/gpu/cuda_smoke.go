package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// cudaSmokeCommand runs the check, with only one card visible to it when
// visible is set, and with the driver's own library directory first on
// LD_LIBRARY_PATH when it was found under nvidiaDriverContainerRoot: the
// script loads libcuda.so.1 by its bare name, and the dynamic linker never
// searches there on its own.
//
// WaitDelay bounds the wait after the context ends: a process stuck in the
// driver ignores the kill until it leaves the kernel, and without it the
// caller would wait as long as the driver does.
var cudaSmokeCommand = func(ctx context.Context, visible string, name string, args ...string) ([]byte, error) {
	overrides := map[string]string{}
	if visible != "" {
		overrides["CUDA_VISIBLE_DEVICES"] = visible
	}
	if dir, ok := cudaLibraryDir(); ok {
		overrides["LD_LIBRARY_PATH"] = dir
	}
	env := os.Environ()
	if len(overrides) > 0 {
		env = environmentWith(env, overrides)
	}
	return runRuntimeCheck(ctx, env, name, args...)
}

// CUDARuntimeSmokeOn asks whether the runtime can use one card, named as
// CUDA_VISIBLE_DEVICES wants it (a UUID, so it cannot drift onto another
// card the way an index does).
//
// Per card rather than once per node, because the answers differ: a card
// can be the only one a driver failed to bring up, and asking about
// "device 0" says nothing about the rest. It is also cheap — 0.3 s on a
// working card, including one whose memory is 99% spoken for by a running
// engine (measured on gpu3).
func CUDARuntimeSmokeOn(ctx context.Context, visible string) error {
	out, err := cudaSmokeCommand(ctx, visible, "python3", "-c", accelerator.CUDASmokeScript)
	return cudaSmokeVerdict(out, err)
}

// CUDARuntimeSmoke performs a bounded, dependency-light CUDA driver check.
// It intentionally uses Python ctypes instead of cgo or nvcc so the agent
// remains CGO_ENABLED=0 and the check does not depend on a compiler toolchain.
func CUDARuntimeSmoke(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := cudaSmokeCommand(probeCtx, "", "python3", "-c", accelerator.CUDASmokeScript)
	return cudaSmokeVerdict(out, err)
}

// cudaSmokeVerdict reads the attempt's output. It is shared so a per-card
// check and the whole-node one cannot disagree about what counts as a pass.
func cudaSmokeVerdict(out []byte, err error) error {
	if refused := stillRunning(err); refused != nil {
		return refused
	}
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("cuda runtime smoke failed: %s", detail)
	}
	if !strings.Contains(string(out), "CUDA_RUNTIME_SMOKE_OK") {
		return fmt.Errorf("cuda runtime smoke returned no success marker: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// CUDASmokePrerequisite says why the CUDA runtime check cannot run on this
// host, or "". The check needs python3 and the driver's user-space library;
// a trimmed host or one whose driver lives in a container has neither in
// the usual place. Saying so is the honest answer — a check that cannot run
// is not a card that failed.
func CUDASmokePrerequisite() string {
	if _, err := lookCLI("python3"); err != nil {
		return "the runtime check needs python3, which is not installed on this node"
	}
	if _, ok := cudaLibraryDir(); !ok {
		return "the CUDA driver library (libcuda.so.1) is not where the runtime check can load it"
	}
	return ""
}

// cudaLibraryDir is the directory the driver's user-space library was found
// in, so the smoke check can name it on LD_LIBRARY_PATH explicitly: a node
// whose driver lives only under nvidiaDriverContainerRoot is not on any
// default search path.
func cudaLibraryDir() (string, bool) {
	for _, path := range cudaLibraryPaths {
		if _, err := statTool(path); err == nil {
			return filepath.Dir(path), true
		}
	}
	return "", false
}

// cudaLibraryPaths are where the driver's user-space library is found. A
// host running the driver from a container (the GPU Operator's pattern)
// keeps it under /run/nvidia/driver rather than in the system path.
var cudaLibraryPaths = []string{
	"/usr/lib/x86_64-linux-gnu/libcuda.so.1",
	"/usr/lib64/libcuda.so.1",
	"/usr/lib/aarch64-linux-gnu/libcuda.so.1",
	"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1",
	"/run/nvidia/driver/usr/lib64/libcuda.so.1",
}
