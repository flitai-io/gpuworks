package gpu

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func readHygonOverviewFixture(t *testing.T) string {
	t.Helper()
	return readHygonTestdata(t, "hy-smi-overview.txt")
}

func readHygonTestdata(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// K100_AI hy-smi rejects the combined --showuse/--showtemp/--showpower query;
// the same host still answers the bare overview table, so utilization,
// temperature and power must be recovered from it instead of staying
// unsampled for the whole telemetry lifetime.
func TestHygonMetricsRecoverOverviewWhenCombinedFlagsAreUnsupported(t *testing.T) {
	var calls []string
	p := &HygonProber{
		seen: cardBaseline{cards: map[int]proto.GPUInfo{0: {UUID: "stable-hygon-0"}}},
		command: func(_ context.Context, args ...string) (string, error) {
			key := strings.Join(args, " ")
			calls = append(calls, key)
			switch key {
			case "--showuse --showmeminfo vram --showtemp --showpower":
				return "unrecognized arguments: --showpower", errors.New("exit status 2")
			case "--showmeminfo vram":
				return readHygonFixture(t), nil
			case "":
				return readHygonOverviewFixture(t), nil
			default:
				t.Fatalf("unexpected hy-smi arguments: %q", args)
				return "", nil
			}
		},
	}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 2 {
		t.Fatalf("Metrics() = %+v, %v (calls %v)", metrics, err, calls)
	}
	if metrics[0].UUID != "stable-hygon-0" {
		t.Fatalf("metric did not retain probe UUID: %+v", metrics[0])
	}
	want := []map[string]float64{
		{
			"gpuworks_gpu_utilization":         0,
			"gpuworks_gpu_temperature_celsius": 64,
			"gpuworks_gpu_power_watts":         132,
			"gpuworks_gpu_memory_used_bytes":   2 * 1024 * 1024,
		},
		{
			"gpuworks_gpu_utilization":         73.5,
			"gpuworks_gpu_temperature_celsius": 63,
			"gpuworks_gpu_power_watts":         130,
			"gpuworks_gpu_memory_used_bytes":   58980 * 1024 * 1024,
		},
	}
	for i, fields := range want {
		for name, value := range fields {
			got, ok := metrics[i].Values[name]
			if !ok || got != value {
				t.Errorf("card %d %s = %v (present %v), want %v", i, name, got, ok, value)
			}
		}
	}
}

// The overview alone still yields telemetry when even the memory query is
// refused: a partial answer beats no samples.
func TestHygonMetricsRecoverOverviewWithoutMemoryQuery(t *testing.T) {
	p := &HygonProber{command: func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "":
			return readHygonOverviewFixture(t), nil
		default:
			return "unrecognized arguments", errors.New("exit status 2")
		}
	}}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 2 {
		t.Fatalf("Metrics() = %+v, %v", metrics, err)
	}
	if metrics[1].Values["gpuworks_gpu_utilization"] != 73.5 {
		t.Fatalf("metric 1 = %+v", metrics[1])
	}
	if _, ok := metrics[1].Values["gpuworks_gpu_memory_used_bytes"]; ok {
		t.Fatalf("invented memory without a memory query: %+v", metrics[1])
	}
}

// Unsupported or unparsable overview cells stay absent: an N/A card must not
// be reported as zero utilization, zero temperature or zero power.
func TestHygonMetricsFallbackKeepsUnknownFieldsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		overview string
	}{
		{"unknown values", "HCU Temp AvgPwr Perf PwrCap VRAM% HCU% Mode\n0 N/A N/A auto 350.0W 0% N/A Normal\n1 N/A N/A auto 350.0W 0% N/A Normal\n"},
		{"unrelated text", "unavailable for device 0 at 37 C and 4 percent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &HygonProber{command: func(_ context.Context, args ...string) (string, error) {
				switch strings.Join(args, " ") {
				case "--showmeminfo vram":
					return readHygonFixture(t), nil
				case "":
					return tc.overview, nil
				default:
					return "unsupported", errors.New("exit status 2")
				}
			}}
			metrics, err := p.Metrics(t.Context())
			if err != nil || len(metrics) != 2 {
				t.Fatalf("Metrics() = %+v, %v", metrics, err)
			}
			for _, m := range metrics {
				for _, name := range []string{"gpuworks_gpu_utilization", "gpuworks_gpu_temperature_celsius", "gpuworks_gpu_power_watts"} {
					if _, ok := m.Values[name]; ok {
						t.Errorf("invented missing %s in %+v", name, m)
					}
				}
			}
		})
	}
}

// Newer DTK-25.x hy-smi builds print extra overview columns (Dec%, Enc%)
// ahead of Mode. Column positions must come from the header row, so the
// wider table still yields utilization, temperature and power.
func TestHygonMetricsRecoverOverviewWithExtraColumns(t *testing.T) {
	raw := readHygonTestdata(t, "hy-smi-overview-wide.txt")
	p := &HygonProber{command: func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "":
			return raw, nil
		default:
			return "unrecognized arguments", errors.New("exit status 2")
		}
	}}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 2 {
		t.Fatalf("Metrics() = %+v, %v", metrics, err)
	}
	want := map[string]float64{
		"gpuworks_gpu_utilization":         73.5,
		"gpuworks_gpu_temperature_celsius": 63,
		"gpuworks_gpu_power_watts":         130,
	}
	for name, value := range want {
		if got := metrics[1].Values[name]; got != value {
			t.Errorf("wide-table %s = %v, want %v (%+v)", name, got, value, metrics[1])
		}
	}
}

// A header without the HCU% column yields no utilization sample, but the
// columns that are present still produce their metrics.
func TestHygonOverviewWithoutUtilizationColumnStillYieldsTempAndPower(t *testing.T) {
	overview := "HCU Temp AvgPwr Perf PwrCap VRAM% Mode\n" +
		"0 64.0C 132.0W auto 350.0W 0% Normal\n" +
		"1 63.0C 130.0W auto 350.0W 90% Normal\n"
	rows := parseHygonOverview(overview)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for index, row := range rows {
		if _, ok := row["gpuworks_gpu_utilization"]; ok {
			t.Errorf("card %d invented utilization from a header without HCU%%: %+v", index, row)
		}
	}
	if rows[0]["gpuworks_gpu_temperature_celsius"] != 64 || rows[0]["gpuworks_gpu_power_watts"] != 132 {
		t.Fatalf("card 0 temp/power = %+v", rows[0])
	}
}

// A row shorter than a needed column skips just that metric instead of
// dropping the whole row or misreading a shifted cell.
func TestHygonOverviewShortRowSkipsOnlyMissingMetrics(t *testing.T) {
	overview := "HCU Temp AvgPwr Perf PwrCap VRAM% HCU% Mode\n" +
		"0 64.0C\n" +
		"1 63.0C 130.0W auto 350.0W 90% 73.5% Normal\n"
	rows := parseHygonOverview(overview)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0]["gpuworks_gpu_temperature_celsius"] != 64 || len(rows[0]) != 1 {
		t.Fatalf("card 0 = %+v, want only temperature", rows[0])
	}
	if rows[1]["gpuworks_gpu_utilization"] != 73.5 || rows[1]["gpuworks_gpu_power_watts"] != 130 {
		t.Fatalf("card 1 = %+v", rows[1])
	}
}

func TestHygonMetricsFallbackFailureAndCancellation(t *testing.T) {
	p := &HygonProber{command: func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "--showmeminfo vram":
			return "", errors.New("meminfo query refused")
		case "":
			return "", errors.New("overview query refused")
		default:
			return "", errors.New("combined query refused")
		}
	}}
	metrics, err := p.Metrics(t.Context())
	if err == nil || len(metrics) != 0 {
		t.Fatalf("Metrics() = %+v, %v", metrics, err)
	}
	for _, want := range []string{"combined query refused", "meminfo query refused", "overview query refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not report %q", err, want)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Metrics(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Metrics() error = %v, want context.Canceled", err)
	}
}
