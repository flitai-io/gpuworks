package gpu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

func TestContainerSmokeVerdict(t *testing.T) {
	for _, tc := range []struct {
		vendor, out string
		exit        int
		ok, onHost  bool
	}{
		{"ppu", "CUDA_RUNTIME_SMOKE_OK\n", 0, true, false},
		{"ppu", "CUDA_RUNTIME_SMOKE_CARD_FAILED: cuCtxCreate=999\n", 1, false, false},
		{"ppu", "CUDA_RUNTIME_SMOKE_UNAVAILABLE: cuInit=100\n", 10, false, true},
		{"ppu", "CUDA_RUNTIME_SMOKE_OK\n", 139, false, true},
		{"ppu", "", 137, false, true},
		{"ascend", "ASCEND_ACL_SMOKE_OK\n", 0, true, false},
		{"ascend", "ASCEND_ACL_SMOKE_DIE_FAILED: aclrtSetDevice=507033\n", 1, false, false},
		{"ascend", "ASCEND_ACL_SMOKE_UNAVAILABLE: the image has no libascendcl.so\n", 10, false, true},
		// A vendor whose check does not run in an image has no grader,
		// whichever runtime's verdict the output carries.
		{"nvidia", "CUDA_RUNTIME_SMOKE_OK\nASCEND_ACL_SMOKE_OK\n", 0, false, true},
		{"hygon", "CUDA_RUNTIME_SMOKE_OK\nASCEND_ACL_SMOKE_OK\n", 0, false, true},
		{"amd", "CUDA_RUNTIME_SMOKE_OK\nASCEND_ACL_SMOKE_OK\n", 0, false, true},
		{"kunlunxin", "CUDA_RUNTIME_SMOKE_OK\nASCEND_ACL_SMOKE_OK\n", 0, false, true},
		{"apple", "CUDA_RUNTIME_SMOKE_OK\nASCEND_ACL_SMOKE_OK\n", 0, false, true},
	} {
		err := ContainerSmokeVerdict(tc.vendor, tc.out, tc.exit)
		if (err == nil) != tc.ok || (!tc.ok && (HostRuntimeUnavailable(err) != "") != tc.onHost) {
			t.Errorf("%s %q exit %d: err = %v, want ok=%v host=%v", tc.vendor, tc.out, tc.exit, err, tc.ok, tc.onHost)
		}
	}
}

// buildStub compiles a stand-in vendor library, or skips without a compiler.
func buildStub(t *testing.T, dir, name, source string) string {
	t.Helper()
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("a C compiler is needed to build the stand-in library")
	}
	src := filepath.Join(dir, name+".c")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, name)
	if out, err := exec.Command(compiler, "-shared", "-fPIC", "-o", lib, src).CombinedOutput(); err != nil {
		t.Skipf("cannot build the stub: %v %s", err, out)
	}
	return lib
}

func runPython(t *testing.T, script string, env ...string) (string, int) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to run the check script")
	}
	cmd := exec.Command(python, "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func TestCUDAGradedSmokeScriptAgainstAStubDriver(t *testing.T) {
	dir := t.TempDir()
	buildStub(t, dir, "libcuda.so.1", `#include <stdlib.h>
#include <stdint.h>
static int env(const char *n) { const char *v = getenv(n); return v ? atoi(v) : 0; }
int cuInit(unsigned f) { return env("STUB_INIT_RC"); }
int cuDeviceGet(int *d, int i) { *d = 0; return 0; }
int cuCtxCreate_v2(void **c, unsigned f, int d) { *c = (void*)1; return env("STUB_CTX_RC"); }
int cuMemAlloc_v2(uint64_t *p, size_t n) { *p = 1; return env("STUB_ALLOC_RC"); }
int cuMemFree_v2(uint64_t p) { return 0; }
int cuCtxDestroy_v2(void *c) { return 0; }
`)
	lib := "LD_LIBRARY_PATH=" + dir
	grade := func(env ...string) error {
		out, code := runPython(t, accelerator.CUDAGradedSmokeScript, append([]string{lib}, env...)...)
		return ContainerSmokeVerdict("ppu", out, code)
	}
	if err := grade(); err != nil {
		t.Fatalf("healthy: %v", err)
	}
	if err := grade("STUB_ALLOC_RC=2"); err == nil || HostRuntimeUnavailable(err) != "" || !strings.Contains(err.Error(), "cuMemAlloc=2") {
		t.Fatalf("allocation failure: %v, want the card's verdict", err)
	}
	if err := grade("STUB_CTX_RC=999"); err == nil || HostRuntimeUnavailable(err) != "" {
		t.Fatalf("context failure: %v, want the card's verdict", err)
	}
	if reason := HostRuntimeUnavailable(grade("STUB_INIT_RC=100")); !strings.Contains(reason, "cuInit=100") {
		t.Fatalf("init failure: %q, want host-level", reason)
	}
}

func TestCUDAGradedSmokeScriptReportsMissingLibrary(t *testing.T) {
	script := `import ctypes

original_cdll = ctypes.CDLL

def load_without_cuda(name, *args, **kwargs):
    if name == "libcuda.so.1":
        raise OSError("test CUDA library unavailable")
    return original_cdll(name, *args, **kwargs)

ctypes.CDLL = load_without_cuda
` + accelerator.CUDAGradedSmokeScript
	out, code := runPython(t, script)
	if code != 10 {
		t.Fatalf("missing library: exit %d, want 10; output: %s", code, out)
	}
	want := "the runtime in the image could not be used: libcuda.so.1 did not load: test CUDA library unavailable"
	if reason := HostRuntimeUnavailable(ContainerSmokeVerdict("ppu", out, code)); reason != want {
		t.Fatalf("missing library: %q, want %q; output: %s", reason, want, out)
	}
}

func TestAscendContainerScriptFindsTheImagesCANN(t *testing.T) {
	script, ok := accelerator.ContainerSmokeScript("ascend")
	if !ok {
		t.Fatal("no Ascend container script")
	}
	// An image without CANN is the host's problem.
	out, code := runPython(t, script, "ASCEND_HOME_PATH="+t.TempDir(), "ASCEND_ACL_LIBRARY=")
	if code != 10 || !strings.Contains(out, "the image has no libascendcl.so") {
		// The default homes may exist on a real Ascend test host.
		if _, err := os.Stat("/usr/local/Ascend"); err != nil {
			t.Fatalf("no CANN: exit %d %q", code, out)
		}
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "aarch64-linux", "lib64"), 0o755); err != nil {
		t.Fatal(err)
	}
	buildStub(t, filepath.Join(home, "aarch64-linux", "lib64"), "libascendcl.so", `
int aclInit(const char *c) { return 0; }
int aclrtSetDevice(int d) { return 0; }
int aclrtCreateContext(void **c, int d) { *c = (void*)1; return 0; }
int aclrtMalloc(void **p, unsigned long n, int policy) { *p = (void*)1; return 0; }
int aclrtFree(void *p) { return 0; }
int aclrtDestroyContext(void *c) { return 0; }
int aclrtResetDevice(int d) { return 0; }
int aclFinalize(void) { return 0; }
`)
	out, code = runPython(t, script, "ASCEND_HOME_PATH="+home, "ASCEND_ACL_LIBRARY=")
	if err := ContainerSmokeVerdict("ascend", out, code); err != nil {
		t.Fatalf("CANN under ASCEND_HOME_PATH: %v (%s)", err, out)
	}
}
