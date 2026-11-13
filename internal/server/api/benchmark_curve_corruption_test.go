package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestBenchmarkCurveKeepsValidSamplesAroundCorruptMeasurement(t *testing.T) {
	var diagnostics bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&diagnostics, nil))
	handler, path, token := benchmarkCurveHTTPFixture(t, log,
		`[{"t":1000,"output_tput":11,"ttft_p50":10},{"t":2000,"output_tput":22,"ttft_p50":"PRIVATE_BENCHMARK_TOKEN"},{"t":3000,"output_tput":33,"ttft_p50":30}]`,
		`[{"t":4000,"output_tput":44,"ttft_p50":40}]`,
	)

	wire := readBenchmarkCurveHTTP(t, handler, path, token)
	var points []struct {
		Seq     int                          `json:"seq"`
		Samples []map[string]json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(wire["points"], &points); err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].Seq != 1 || points[1].Seq != 2 {
		t.Fatalf("corrupt measurement removed a point: %s", wire["points"])
	}
	if len(points[0].Samples) != 2 || len(points[1].Samples) != 1 {
		t.Fatalf("valid samples were lost or corrupt sample was retained: %s", wire["points"])
	}
	for i, want := range []struct{ at, output string }{{"1000", "11"}, {"3000", "33"}} {
		if got := points[0].Samples[i]; string(got["t"]) != want.at || string(got["output_tput"]) != want.output {
			t.Errorf("surviving sample %d=%s, want t=%s output_tput=%s", i, wire["points"], want.at, want.output)
		}
	}
	if got := points[1].Samples[0]; string(got["t"]) != "4000" || string(got["output_tput"]) != "44" {
		t.Errorf("unrelated point changed: %s", wire["points"])
	}
	var diagnostic map[string]json.RawMessage
	if err := json.Unmarshal(diagnostics.Bytes(), &diagnostic); err != nil {
		t.Fatalf("missing structured corruption diagnostic: %q (%v)", diagnostics.String(), err)
	}
	if string(diagnostic["point_seq"]) != "1" || string(diagnostic["sample_index"]) != "1" || string(diagnostic["run_id"]) == "" {
		t.Errorf("diagnostic cannot identify the discarded sample: %s", diagnostics.String())
	}
	if strings.Contains(diagnostics.String(), "PRIVATE_BENCHMARK_TOKEN") {
		t.Fatal("corruption diagnostic leaked the stored payload")
	}
}
