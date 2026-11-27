package gpu

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMetaXSmokeSelectsAndAllocatesOnNamedCard(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	const fixture = `import ctypes, os
os.path.isfile = lambda p: True
class Fn:
    def __init__(self, name): self.name = name
    def __call__(self, *args):
        name = self.name
        if name == "mcInit" and os.getenv("FAIL") == "init": return 7
        if name == "mcGetDeviceCount":
            if os.getenv("FAIL") == "count_error": return 8
            args[0]._obj.value = 0 if os.getenv("FAIL") == "count_zero" else 3
        if name == "mcDeviceGetByPCIBusId":
            assert args[1] == b"0000:34:00.0"
            args[0]._obj.value = 2
        if name == "mcSetDevice":
            assert args[0] == 2
            state.append("select")
            if os.getenv("FAIL") == "select": return 1
        if name == "mcMemGetInfo":
            assert state == ["select"]
            args[1]._obj.value = 0 if os.getenv("FAIL") == "meminfo_zero" else 1024
            if os.getenv("FAIL") == "meminfo": return 9
        if name == "mcMalloc":
            assert state == ["select"]
            if os.getenv("FAIL") == "alloc": return 1
            args[0]._obj.value = 123
            state.append("alloc")
        if name == "mcFree":
            assert state == ["select", "alloc"]
            state.append("free")
        return 0
class Library:
    def __getattr__(self, name):
        if os.getenv("FAIL") == "symbol" and name == "mcSetDevice": raise AttributeError(name)
        return Fn(name)
state = []
def load(path):
    if os.getenv("FAIL") == "load": raise OSError("missing dependency")
    return Library()
ctypes.CDLL = load
`
	for _, tc := range []struct {
		failure, marker string
		exitCode        int
	}{
		{"", "MACA_RUNTIME_SMOKE_OK", 0},
		{"init", "MACA_RUNTIME_SMOKE_UNAVAILABLE: mcInit=7", 10},
		{"count_error", "MACA_RUNTIME_SMOKE_UNAVAILABLE: mcGetDeviceCount rc=8 count=0", 10},
		{"count_zero", "MACA_RUNTIME_SMOKE_UNAVAILABLE: mcGetDeviceCount rc=0 count=0", 10},
		{"select", "MACA_RUNTIME_SMOKE_DIE_FAILED: mcSetDevice", 1},
		{"alloc", "MACA_RUNTIME_SMOKE_DIE_FAILED: mcMalloc", 1},
		{"meminfo", "MACA_RUNTIME_SMOKE_DIE_FAILED: 0000:34:00.0 mcMemGetInfo rc=9 total=1024", 1},
		{"meminfo_zero", "MACA_RUNTIME_SMOKE_DIE_FAILED: 0000:34:00.0 mcMemGetInfo rc=0 total=0", 1},
		{"load", "MACA_RUNTIME_SMOKE_UNAVAILABLE:", 10},
		{"symbol", "MACA_RUNTIME_SMOKE_UNAVAILABLE:", 10},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			cmd := exec.Command(python, "-c", fixture+metaxHostSmokeScript)
			cmd.Env = append(os.Environ(), "MACA_SMOKE_BDF=0000:34:00.0", "FAIL="+tc.failure)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					code = exitErr.ExitCode()
				} else {
					t.Fatalf("running Python: %v", err)
				}
			}
			if !strings.Contains(string(out), tc.marker) || code != tc.exitCode {
				t.Fatalf("output %q, exit code %d, want marker %q and exit code %d", out, code, tc.marker, tc.exitCode)
			}
		})
	}
}
