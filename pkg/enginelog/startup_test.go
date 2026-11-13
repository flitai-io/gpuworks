package enginelog_test

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/enginelog"
)

func startupWire(t *testing.T, dialect, logs string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(enginelog.ParseDialect(dialect, logs))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

// These are explicit elapsed figures, not the adjacent model/graph memory
// sizes. Parallel workers repeat their summary; their durations are not added.
func TestVLLMExplicitStartupDurations(t *testing.T) {
	logs := `(Worker_TP0 pid=4) Model loading took 14.9889 GiB and 4.32 seconds
(Worker_TP1 pid=5) Model loading took 14.9889 GiB and 5.75 seconds
(Worker_TP0 pid=4) torch.compile takes 7.51 s in total
(Worker_TP1 pid=5) torch.compile takes 8.25 s in total
(Worker_TP0 pid=4) Graph capturing finished in 12 secs, took 0.58 GiB
(Worker_TP1 pid=5) Graph capturing finished in 13.5 secs, took 0.59 GiB`
	wire := startupWire(t, "vllm", logs)
	for key, want := range map[string]float64{"ModelLoadingSeconds": 5.75, "CompilationSeconds": 8.25, "GraphCaptureSeconds": 13.5, "ModelWeightsGiB": 14.9889, "GraphCaptureGiB": 0.58} {
		if got := wire[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestSGLangExplicitStartupDurations(t *testing.T) {
	logs := `[2026-10-24 01:58:26] server_args={'model_path':'/model'}
[2026-10-24 02:00:15 TP0 EP0] Load weight end. elapsed=42.25 s, type=Model, mem usage=4.00 GB.
[2026-10-24 02:00:15 TP1 EP1] Load weight end. elapsed=43.50 s, type=Model, mem usage=4.00 GB.
[2026-10-24 02:03:50 TP0 EP0] Capture target verify CUDA graph end. elapsed=49.66 s, mem usage=0.86 GB.
[2026-10-24 02:03:50 TP1 EP1] Capture target verify CUDA graph end. elapsed=49.67 s, mem usage=0.86 GB.
[2026-10-24 02:04:00 TP0 EP0] Capture draft decode CUDA graph end. elapsed=10.71 s, mem usage=0.21 GB.`
	wire := startupWire(t, "sglang", logs)
	for key, want := range map[string]float64{"ModelLoadingSeconds": 43.5, "GraphCaptureSeconds": 49.67} {
		if got := wire[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if _, exists := wire["CompilationSeconds"]; exists {
		t.Fatal("a missing compilation summary became an observed duration")
	}
	legacy := startupWire(t, "sglang", `[2025-04-24 01:09:01 TP0] Capture cuda graph end. Time elapsed: 7.23 s. avail mem=5.86 GB. mem usage=1.95 GB.`)
	if got := legacy["GraphCaptureSeconds"]; got != 7.23 {
		t.Errorf("legacy graph seconds = %v, want 7.23", got)
	}
}

func TestStartupDurationsKeepUnknownSeparateFromZero(t *testing.T) {
	for _, dialect := range []string{"vllm", "sglang"} {
		wire := startupWire(t, dialect, "Model loading took 14.9889 GiB\nGraph capturing finished, took 0.58 GiB\nLoad weight end. mem usage=4.00 GB.\nCapture CUDA graph end. mem usage=0.86 GB.")
		for _, key := range []string{"ModelLoadingSeconds", "CompilationSeconds", "GraphCaptureSeconds", "StartupSeconds"} {
			if _, exists := wire[key]; exists {
				t.Errorf("%s: %s was inferred without an observed duration", dialect, key)
			}
		}
	}
	wire := startupWire(t, "vllm", "Model loading took 0 GiB and 0 seconds\ntorch.compile takes 0 s in total\nGraph capturing finished in 0 secs, took 0 GiB")
	for _, key := range []string{"ModelLoadingSeconds", "CompilationSeconds", "GraphCaptureSeconds"} {
		if got, exists := wire[key]; !exists || got != float64(0) {
			t.Errorf("known zero %s = %v (present=%t)", key, got, exists)
		}
	}
	if wire["Found"] != false {
		t.Fatal("timings alone were presented as measured memory")
	}
	for _, bad := range []string{"-1", "NaN", "Inf", "1e999", strings.Repeat("9", 400)} {
		wire := startupWire(t, "vllm", "torch.compile takes "+bad+" s in total")
		if _, exists := wire["CompilationSeconds"]; exists {
			t.Errorf("invalid duration %s became %v", bad, wire["CompilationSeconds"])
		}
	}
}
