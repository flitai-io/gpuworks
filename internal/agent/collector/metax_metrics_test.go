package collector

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The collector must not publish a conflicting or non-finite MetaX row, even
// when another card in the same mx-smi CSV has a valid sample.
func TestMetaXCollectorOmitsUntrustedSamples(t *testing.T) {
	dir := t.TempDir()
	listing := "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n" +
		"GPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n"
	program := `#!/bin/sh
case "$1" in
-L) printf '%s' "$METAX_LIST" ;;
-o) cp "$METAX_CSV" "$2" ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "mx-smi"), []byte(program), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("METAX_LIST", listing)
	csvPath := filepath.Join(dir, "metrics.csv")
	t.Setenv("METAX_CSV", csvPath)

	for _, tc := range []struct {
		name string
		row  string
	}{
		{"conflicting duplicate", "GPU#0,0000:0f:00.0,12\nGPU#0,0000:0f:00.0,99\n"},
		{"non-finite", "GPU#0,0000:0f:00.0,NaN\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "deviceId,bdfId,utilization.GPU [%]\n" + tc.row + "GPU#1,0000:34:00.0,42\n"
			if err := os.WriteFile(csvPath, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			c := New("node-1", &gpu.MetaXProber{}, func() []RunningInstance { return nil }, func(proto.MetricBatch) {}, slog.Default())
			samples := c.gpuSamples(context.Background(), 1000)
			if len(samples) != 1 || samples[0].Metric != "gpuworks_gpu_utilization" || samples[0].Value != 42 || samples[0].Labels["gpu_uuid"] != "GPU-b" {
				t.Fatalf("published samples = %+v, want only the verified GPU-b sample", samples)
			}
		})
	}
}
