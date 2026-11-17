package enginelog_test

import (
	"testing"

	"gpuworks.ai/gpuworks/pkg/enginelog"
)

func TestRootCauseLooksPastGenericApplicationException(t *testing.T) {
	// The wrapper spelling and prefix are preserved from the existing engine
	// log fixture in internal/agent/app/background_test.go. The preceding
	// exceptions are constructed controls, not a capture of the Jira incident.
	const wrapper = "(APIServer pid=83923) [ERROR] 2026-09-18-20:39:28 (PID:83923) ERR99999 UNKNOWN applicaiton exception" //nolint:misspell // External engine output.
	for _, tc := range []struct {
		name string
		log  string
		want string
	}{
		{"missing dependency", "ModuleNotFoundError: No module named 'missing_backend'\n" + wrapper, "ModuleNotFoundError: No module named 'missing_backend'"},
		{"tagged worker", "(Worker_TP0 pid=9) ERROR 09-18 20:39:27 [worker.py:12] ValueError: unsupported backend\n" + wrapper, "ValueError: unsupported backend"},
		{"bracketed worker", "(Worker_TP0 pid=9) [ERROR] 2026-09-18-20:39:27 (PID:9, Device:0, RankID:0) ValueError: unsupported backend\n" + wrapper, "ValueError: unsupported backend"},
		{"correctly spelled wrapper", "ImportError: cannot import name 'backend'\n[ERROR] ERR99999 UNKNOWN application exception", "ImportError: cannot import name 'backend'"},
		{"wrapper only", wrapper, wrapper},
		{"specific error with same code", "ValueError: handled earlier\n[ERROR] ERR99999 RuntimeError: invalid kernel configuration", "[ERROR] ERR99999 RuntimeError: invalid kernel configuration"},
		{"wrapper with concrete detail", "ValueError: handled earlier\n[ERROR] ERR99999 UNKNOWN application exception: RuntimeError: invalid kernel configuration", "[ERROR] ERR99999 UNKNOWN application exception: RuntimeError: invalid kernel configuration"},
		{"different code", "ValueError: handled earlier\n[ERROR] ERR12345 UNKNOWN application exception", "[ERROR] ERR12345 UNKNOWN application exception"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := enginelog.RootCause(tc.log); got != tc.want {
				t.Fatalf("RootCause = %q, want %q", got, tc.want)
			}
		})
	}
}
