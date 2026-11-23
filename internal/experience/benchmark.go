package experience

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// seedBenchmark runs one small throughput evaluation on the starter
// deployment, so the evaluation list opens on a finished report. It is a real
// run through the gateway, stored and reported like any other.
func seedBenchmark(ctx context.Context, c *apiClient, marker string, log *slog.Logger) {
	if _, err := os.Stat(marker); err == nil {
		return
	}
	target, err := waitRunning(ctx, c, sampleDeployment.Name)
	if err != nil {
		log.Warn("sample evaluation skipped", "err", err)
		return
	}
	var run struct {
		ID uint `json:"id"`
	}
	if err := c.post(ctx, "/api/v1/benchmarks", map[string]any{
		"name": "示例：吞吐与延迟评测", "target_kind": "instance", "deployment_id": target.ID,
		"load_origin": "server", "mode": "grid", "tokenizer_source": "auto",
		"dataset": map[string]any{"kind": "random", "input_tokens": 512, "output_tokens": 128},
		"search":  map[string]any{"concurrency": []int{1, 2, 4, 8}, "isl": []int{512}, "osl": []int{128}, "requests_per_point_multiplier": 3},
	}, &run); err != nil {
		log.Warn("sample evaluation was not created", "err", err)
		return
	}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		var st struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		if err := c.get(ctx, fmt.Sprintf("/api/v1/benchmarks/%d", run.ID), &st); err != nil {
			continue
		}
		switch st.State {
		case "done":
			_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
			return
		case "failed", "canceled", "cancelled":
			log.Warn("sample evaluation did not finish", "state", st.State, "err", st.Error)
			return
		}
	}
}
