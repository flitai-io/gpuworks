package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// TestDeviceExistsSeesTheLiveHostDev: a privileged container's own /dev is
// a copy made at container start, so a control device a driver creates
// after the agent started (fallbackProber's evidence of a vendor whose CLI
// does not yet answer) is missing there; deviceExists must check the
// host's live /dev through hostRoot when CLIs run there.
func TestDeviceExistsSeesTheLiveHostDev(t *testing.T) {
	root := withHostRoot(t)
	const path = "/dev/nvidiactl"
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !deviceExists(path) {
		t.Fatal("deviceExists did not see a node that only exists under hostRoot")
	}
	cliOnHost.Store(false)
	if deviceExists(path) {
		t.Fatal("deviceExists off the host found a node that only exists under hostRoot")
	}
}

func TestNVIDIAMetricsAndTopologySkipAvailabilityQuery(t *testing.T) {
	var calls [][]string
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "--query-gpu=index,uuid,utilization.gpu"):
			return "0, GPU-a, 42, 1024, 8192, 66, 300, 48\n", nil
		case len(args) > 0 && args[0] == "dmon":
			return "# gpu rxpci txpci\n0 12 34\n", nil
		case reflect.DeepEqual(args, []string{"topo", "-m"}):
			return "topology", nil
		default:
			return "", errors.New("unexpected call: " + joined)
		}
	}}
	if metrics, err := p.Metrics(t.Context()); err != nil || len(metrics) != 1 {
		t.Fatalf("Metrics = %+v, %v", metrics, err)
	}
	if got := p.Topology(t.Context()); got != "topology" {
		t.Fatalf("Topology = %q", got)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want metric+dmon+topology", calls)
	}
	for _, args := range calls {
		if len(args) > 0 && args[0] == "--query-gpu=index" {
			t.Fatalf("unexpected availability query: %+v", calls)
		}
	}
}

// Per-tick command-count guard: on a driver that supports every field,
// Metrics issues exactly one --query-gpu call per tick, on every tick (not
// just the first) — the winning tier is cached, not re-probed — and one dmon
// call per nvidiaPCIeInterval. The top tier is clocks_event_reasons.active
// (R535+ spelling), tried before the deprecated clocks_throttle_reasons.active.
func TestNVIDIAMetricsIssuesOneQueryGPUPerTickAndOneDmonPerInterval(t *testing.T) {
	var queryGPUCalls, dmonCalls int
	clock := time.Now()
	p := &SMIProber{now: func() time.Time { return clock }, runner: func(_ context.Context, args ...string) (string, error) {
		switch {
		case strings.HasPrefix(args[0], "--query-gpu="):
			queryGPUCalls++
			if args[0] != metricsQueryEventReasons {
				return "", errors.New("unexpected query-gpu call: " + args[0])
			}
			return "0, GPU-a, 42, 1024, 8192, 66, 300, 48, 55, 1410, 1980, 0x0000000000000004\n", nil
		case args[0] == "dmon":
			dmonCalls++
			return "# gpu rxpci txpci\n0 12 34\n", nil
		default:
			return "", errors.New("unexpected call: " + strings.Join(args, " "))
		}
	}}
	// Seven ticks at the collector's 10 s cadence span one interval: dmon is
	// asked on the first and on the seventh. The ticks between carry no
	// PCIe values rather than repeating the first ones.
	const ticks = 7
	var metrics []Metric
	var err error
	for i := 0; i < ticks; i++ {
		metrics, err = p.Metrics(t.Context())
		if err != nil {
			t.Fatalf("Metrics tick %d = %v", i, err)
		}
		_, hasPCIe := metrics[0].Values["gpuworks_gpu_pcie_rx_bytes_per_sec"]
		if want := i == 0 || i == ticks-1; hasPCIe != want {
			t.Fatalf("tick %d carries PCIe = %v, want %v", i, hasPCIe, want)
		}
		clock = clock.Add(10 * time.Second)
	}
	if queryGPUCalls != ticks || dmonCalls != 2 {
		t.Fatalf("queryGPUCalls=%d dmonCalls=%d after %d ticks, want %d and 2", queryGPUCalls, dmonCalls, ticks, ticks)
	}
	if len(metrics) != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
	v := metrics[0].Values
	if v["gpuworks_gpu_memory_bandwidth_utilization"] != 55 {
		t.Fatalf("memory bandwidth utilization = %v", v["gpuworks_gpu_memory_bandwidth_utilization"])
	}
	if v["gpuworks_gpu_sm_clock_mhz"] != 1410 || v["gpuworks_gpu_sm_clock_max_mhz"] != 1980 {
		t.Fatalf("sm clocks = %v / %v", v["gpuworks_gpu_sm_clock_mhz"], v["gpuworks_gpu_sm_clock_max_mhz"])
	}
	if v["gpuworks_gpu_throttle_reasons"] != 4 {
		t.Fatalf("throttle reasons = %v, want 4 (0x4)", v["gpuworks_gpu_throttle_reasons"])
	}
}

// A driver that understands clocks_throttle_reasons.active (the deprecated
// spelling) but rejects clocks_event_reasons.active falls back exactly one
// tier, then caches that tier: only the first tick pays for the failed
// top-tier attempt, every later tick issues exactly one query-gpu call.
func TestNVIDIAMetricsPrefersEventReasonsThenCachesThrottleReasonsTier(t *testing.T) {
	var calls []string
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0])
		switch args[0] {
		case metricsQueryEventReasons:
			return "", errors.New("nvidia-smi: unknown field 'clocks_event_reasons.active'")
		case metricsQueryThrottleReasons:
			return "0, GPU-a, 42, 1024, 8192, 66, 300, 48, 55, 1410, 1980, 0x0000000000000004\n", nil
		case "dmon":
			return "# gpu rxpci txpci\n0 12 34\n", nil
		default:
			return "", errors.New("unexpected call: " + args[0])
		}
	}}
	if _, err := p.Metrics(t.Context()); err != nil {
		t.Fatalf("Metrics (tick 1) = %v", err)
	}
	if len(calls) != 3 || calls[0] != metricsQueryEventReasons || calls[1] != metricsQueryThrottleReasons || calls[2] != "dmon" {
		t.Fatalf("tick 1 calls = %+v, want event-reasons, throttle-reasons, dmon", calls)
	}
	calls = nil
	for i := 0; i < 2; i++ {
		if _, err := p.Metrics(t.Context()); err != nil {
			t.Fatalf("Metrics (cached tick) = %v", err)
		}
	}
	if len(calls) != 2 || calls[0] != metricsQueryThrottleReasons || calls[1] != metricsQueryThrottleReasons {
		t.Fatalf("cached ticks calls = %+v, want throttle-reasons twice (event-reasons not retried, dmon not yet due)", calls)
	}
}

// A driver that rejects both throttle-reason spellings (but still understands
// the original extended query) falls back two tiers on the first call, then
// caches that tier for later ticks.
func TestNVIDIAMetricsFallsBackToExtendedThenCaches(t *testing.T) {
	var calls []string
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0])
		switch args[0] {
		case metricsQueryEventReasons, metricsQueryThrottleReasons:
			return "", errors.New("nvidia-smi: unsupported field")
		case metricsQueryExtended:
			return "0, GPU-a, 42, 1024, 8192, 66, 300, 48\n", nil
		case "dmon":
			return "# gpu rxpci txpci\n0 12 34\n", nil
		default:
			return "", errors.New("unexpected call: " + args[0])
		}
	}}
	metrics, err := p.Metrics(t.Context())
	if err != nil {
		t.Fatalf("Metrics (tick 1) = %v", err)
	}
	if len(calls) != 4 || calls[3] != "dmon" {
		t.Fatalf("tick 1 calls = %+v, want event-reasons, throttle-reasons, extended, dmon", calls)
	}
	if _, ok := metrics[0].Values["gpuworks_gpu_memory_bandwidth_utilization"]; ok {
		t.Fatalf("fallback row must not report the new fields: %+v", metrics[0].Values)
	}
	if metrics[0].Values["gpuworks_gpu_fan_speed_percent"] != 48 {
		t.Fatalf("fallback must keep the pre-existing fan speed field: %+v", metrics[0].Values)
	}

	calls = nil
	const moreTicks = 3
	for i := 0; i < moreTicks; i++ {
		if _, err := p.Metrics(t.Context()); err != nil {
			t.Fatalf("Metrics (cached tick %d) = %v", i, err)
		}
	}
	queryGPUCalls := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "--query-gpu=") {
			queryGPUCalls++
			if c != metricsQueryExtended {
				t.Fatalf("cached tier call = %q, want metricsQueryExtended", c)
			}
		}
	}
	if queryGPUCalls != moreTicks {
		t.Fatalf("query-gpu calls across %d cached ticks = %d, want %d (one per tick)", moreTicks, queryGPUCalls, moreTicks)
	}
}

// A driver old enough to reject every field this stage added falls all the
// way back to the original basic query, then caches that tier too.
func TestNVIDIAMetricsFallsBackToBasicQueryThenCaches(t *testing.T) {
	var calls []string
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0])
		switch args[0] {
		case metricsQueryEventReasons, metricsQueryThrottleReasons, metricsQueryExtended:
			return "", errors.New("nvidia-smi: unsupported field")
		case metricsQueryBasic:
			return "0, GPU-a, 42, 1024, 8192, 66, 300\n", nil
		case "dmon":
			return "# gpu rxpci txpci\n0 12 34\n", nil
		default:
			return "", errors.New("unexpected call: " + args[0])
		}
	}}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 1 {
		t.Fatalf("Metrics (tick 1) = %+v, %v", metrics, err)
	}
	if len(calls) != 5 {
		t.Fatalf("tick 1 calls = %+v, want event-reasons, throttle-reasons, extended, basic, dmon", calls)
	}

	calls = nil
	if _, err := p.Metrics(t.Context()); err != nil {
		t.Fatalf("Metrics (cached tick) = %v", err)
	}
	if len(calls) != 1 || calls[0] != metricsQueryBasic {
		t.Fatalf("cached tick calls = %+v, want exactly basic (dmon not yet due)", calls)
	}
}

// After metricsTierRecheckTTL elapses, Metrics tries the top tier again
// instead of trusting the cached lower tier forever — a driver upgrade
// mid-lifetime can unlock a higher tier, at the cost of one extra probe burst
// per TTL window rather than every tick.
func TestNVIDIAMetricsRechecksTopTierAfterTTL(t *testing.T) {
	var calls []string
	eventReasonsAllowed := false
	clock := time.Now()
	p := &SMIProber{now: func() time.Time { return clock }, runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args[0])
		switch args[0] {
		case metricsQueryEventReasons:
			if eventReasonsAllowed {
				return "0, GPU-a, 42, 1024, 8192, 66, 300, 48, 55, 1410, 1980, 0x0\n", nil
			}
			return "", errors.New("nvidia-smi: unsupported field")
		case metricsQueryThrottleReasons:
			return "0, GPU-a, 42, 1024, 8192, 66, 300, 48, 55, 1410, 1980, 0x0\n", nil
		case "dmon":
			return "# gpu rxpci txpci\n0 12 34\n", nil
		default:
			return "", errors.New("unexpected call: " + args[0])
		}
	}}
	if _, err := p.Metrics(t.Context()); err != nil {
		t.Fatalf("Metrics (tick 1) = %v", err)
	}
	calls = nil
	if _, err := p.Metrics(t.Context()); err != nil {
		t.Fatalf("Metrics (cached tick) = %v", err)
	}
	if len(calls) != 1 || calls[0] != metricsQueryThrottleReasons {
		t.Fatalf("cached tick calls = %+v, want throttle-reasons cached (event-reasons not retried)", calls)
	}

	// Simulate the TTL having elapsed, and the driver having been upgraded to
	// now accept the top tier.
	clock = clock.Add(metricsTierRecheckTTL + time.Second)
	eventReasonsAllowed = true
	calls = nil
	if _, err := p.Metrics(t.Context()); err != nil {
		t.Fatalf("Metrics (post-TTL tick) = %v", err)
	}
	if len(calls) != 2 || calls[0] != metricsQueryEventReasons {
		t.Fatalf("post-TTL tick calls = %+v, want event-reasons retried and to win", calls)
	}
}

// The TTL clock must only ever be reset by a full probe (one that actually
// started at tier 0), never by an ordinary cached-tier success — otherwise a
// driver stuck on a lower tier would keep pushing metricsTierRecheckTTL out
// on every single tick and a full re-probe of the top tier would never
// happen again after the very first one. Simulated over 61 ticks at the
// collector's real 10s cadence (~10 minutes plus one tick): the top tier
// must be attempted exactly twice — once at the very first tick, and once
// more right at the TTL boundary — not once, and not more than twice.
func TestNVIDIAMetricsReprobesTopTierExactlyTwiceAcross61TicksSpanningTTL(t *testing.T) {
	var eventReasonsAttempts int
	clock := time.Now()
	p := &SMIProber{now: func() time.Time { return clock }, runner: func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case metricsQueryEventReasons:
			eventReasonsAttempts++
			return "", errors.New("nvidia-smi: unsupported field") // this driver never accepts the top tier
		case metricsQueryThrottleReasons:
			return "0, GPU-a, 42, 1024, 8192, 66, 300, 48, 55, 1410, 1980, 0x0\n", nil
		case "dmon":
			return "# gpu rxpci txpci\n0 12 34\n", nil
		default:
			return "", errors.New("unexpected call: " + args[0])
		}
	}}
	const ticks = 61
	const tickInterval = 10 * time.Second // the real collector's cadence
	for i := 0; i < ticks; i++ {
		if i > 0 {
			clock = clock.Add(tickInterval)
		}
		if _, err := p.Metrics(t.Context()); err != nil {
			t.Fatalf("Metrics tick %d = %v", i, err)
		}
	}
	// 60 * 10s = 600s = metricsTierRecheckTTL exactly, reached on tick index
	// 60 (the 61st call): if every cached-tier success incorrectly reset the
	// TTL clock (the bug this test catches), this would still be 1.
	if eventReasonsAttempts != 2 {
		t.Fatalf("event-reasons (top tier) attempts across %d ticks spanning the TTL = %d, want exactly 2", ticks, eventReasonsAttempts)
	}
}

// nvidia-smi prints "[N/A]" for a queried field a particular GPU does not
// support even when the query itself succeeds; such fields must be silently
// absent from Values rather than zero or an error.
func TestParseNVIDIAMetricsHandlesNAForNewFields(t *testing.T) {
	metrics := parseNVIDIAMetrics("0, GPU-a, 42, 1024, 8192, 66, 300, 48, [N/A], [N/A], [N/A], [N/A]\n")
	if len(metrics) != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
	for _, key := range []string{"gpuworks_gpu_memory_bandwidth_utilization", "gpuworks_gpu_sm_clock_mhz", "gpuworks_gpu_sm_clock_max_mhz", "gpuworks_gpu_throttle_reasons"} {
		if _, ok := metrics[0].Values[key]; ok {
			t.Fatalf("%s must be absent for an [N/A] field: %+v", key, metrics[0].Values)
		}
	}
	// The pre-existing fields must still be present.
	if metrics[0].Values["gpuworks_gpu_utilization"] != 42 {
		t.Fatalf("utilization = %v", metrics[0].Values["gpuworks_gpu_utilization"])
	}
}

func TestParseThrottleReasonsHex(t *testing.T) {
	cases := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"0x0000000000000000", 0, true},
		{"0x0000000000000004", 4, true},
		{"0X20", 32, true},
		{"[N/A]", 0, false},
		{"[Not Supported]", 0, false},
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, c := range cases {
		got, ok := parseThrottleReasonsHex(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("parseThrottleReasonsHex(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestNVIDIAProbeCachesDriverAndFramework(t *testing.T) {
	var calls [][]string
	p := &SMIProber{hostRoot: "/nonexistent-nvidia-test-root", frameworkTTL: time.Hour, runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 0 {
			return "Driver Version: 550.90\nCUDA Version: 12.8\n", nil
		}
		if reflect.DeepEqual(args, []string{"-L"}) {
			return "GPU 0: NVIDIA H100 (UUID: GPU-a)\n", nil
		}
		if strings.Contains(args[0], "--query-gpu=index,uuid,name") {
			return "0, GPU-a, NVIDIA H100, 81920, 9.0, 00000000:65:00.0, 550.90, 700, 0, 0\n", nil
		}
		return "", errors.New("unexpected call: " + strings.Join(args, " "))
	}}
	if gpus, err := p.Probe(t.Context()); err != nil || len(gpus) != 1 {
		t.Fatalf("Probe = %+v, %v", gpus, err)
	}
	if got := p.DriverVersion(t.Context()); got != "550.90" {
		t.Fatalf("DriverVersion = %q", got)
	}
	if family, version := p.Framework(t.Context()); family != "cuda" || version != "12.8" {
		t.Fatalf("Framework = %q/%q", family, version)
	}
	if _, version := p.Framework(t.Context()); version != "12.8" {
		t.Fatalf("cached Framework version = %q", version)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want inventory + MIG listing + one framework query", calls)
	}
}

func TestNvidiaArchID(t *testing.T) {
	cases := map[string]string{
		"9.0":  "sm_90",
		"8.9":  "sm_89",
		"12.0": "sm_120",
		"7":    "sm_70",
		"":     "",
	}
	for cc, want := range cases {
		if got := nvidiaArchID(cc); got != want {
			t.Errorf("nvidiaArchID(%q) = %q, want %q", cc, got, want)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	line := "0, GPU-abc, NVIDIA H100, 81559, 9.0, 00000000:65:00.0, 550.90.07"
	f := splitCSV(line)
	if len(f) != 7 {
		t.Fatalf("expected 7 fields, got %d: %v", len(f), f)
	}
	if f[2] != "NVIDIA H100" {
		t.Errorf("field trim failed: %q", f[2])
	}
	if f[6] != "550.90.07" {
		t.Errorf("last field: %q", f[6])
	}
}

func TestParseNVIDIAMetrics(t *testing.T) {
	metrics := parseNVIDIAMetrics("0, GPU-a, 42, 1024, 8192, 66, 300, 48\n")
	if len(metrics) != 1 {
		t.Fatalf("metrics = %d, want 1: %+v", len(metrics), metrics)
	}
	m := metrics[0]
	if m.Index != 0 || m.UUID != "GPU-a" || m.Vendor != "nvidia" {
		t.Fatalf("identity = %+v", m)
	}
	if m.Values["gpuworks_gpu_utilization"] != 42 {
		t.Fatalf("utilization = %v", m.Values["gpuworks_gpu_utilization"])
	}
	if m.Values["gpuworks_gpu_memory_used_bytes"] != 1024*1024*1024 {
		t.Fatalf("memory used = %v", m.Values["gpuworks_gpu_memory_used_bytes"])
	}
	if m.Values["gpuworks_gpu_memory_total_bytes"] != 8192*1024*1024 {
		t.Fatalf("memory total = %v", m.Values["gpuworks_gpu_memory_total_bytes"])
	}
	if m.Values["gpuworks_gpu_fan_speed_percent"] != 48 {
		t.Fatalf("fan speed = %v", m.Values["gpuworks_gpu_fan_speed_percent"])
	}
}

func TestParseNVIDIADmon(t *testing.T) {
	got := parseNVIDIADmon("# gpu rxpci txpci\n# Idx  MB/s  MB/s\n0 12 34\n1 - -\n")
	if got[0].RXBytesPerSec != 12*1024*1024 || got[0].TXBytesPerSec != 34*1024*1024 {
		t.Fatalf("gpu 0 = %+v", got[0])
	}
	if _, ok := got[1]; ok {
		t.Fatalf("unsupported GPU should be absent: %+v", got[1])
	}
}

func TestParseNVIDIAInfoHealthLevels(t *testing.T) {
	const prefix = "0, GPU-a, NVIDIA H100, 81920, 9.0, 00000000:65:00.0, 550.90, 700, "
	cases := []struct {
		name, health        string
		healthy             bool
		status, code, proof string
	}{
		{"clean", "0, 0, No, No, No", true, "OK", "", ""},
		{"fields the card does not support are not judged", "[N/A], [N/A], [N/A], [N/A], [N/A]", true, "OK", "", ""},
		{"volatile errors stop scheduling", "3, 8, No, No, No", false, "Error", "memory_uncorrectable_errors",
			"ecc.errors.uncorrected.volatile.total=3\necc.errors.uncorrected.aggregate.total=8"},
		// The aggregate counter is lifetime history a reset never clears.
		{"aggregate history stays healthy", "0, 1, No, [N/A], [N/A]", true, "OK", "memory_error_history",
			"ecc.errors.uncorrected.aggregate.total=1"},
		{"pending row replacement stays schedulable", "0, 0, [N/A], Yes, No", true, "Warning", "memory_repair_pending",
			"remapped_rows.pending=Yes"},
		{"pending page retirement stays schedulable", "0, 0, Yes, [N/A], [N/A]", true, "Warning", "memory_repair_pending",
			"retired_pages.pending=Yes"},
		{"failed replacement stops scheduling", "0, 0, No, Yes, Yes", false, "Error", "memory_remap_failed",
			"remapped_rows.pending=Yes\nremapped_rows.failure=Yes"},
		{"older driver answers only the ECC pair", "3, 8", false, "Error", "memory_uncorrectable_errors",
			"ecc.errors.uncorrected.volatile.total=3\necc.errors.uncorrected.aggregate.total=8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gpus := parseNVIDIAInfo(prefix + tc.health + "\n")
			if len(gpus) != 1 || gpus[0].PowerLimitWatts != 700 {
				t.Fatalf("gpus = %+v", gpus)
			}
			g := gpus[0]
			if g.Healthy != tc.healthy || g.HealthStatus != tc.status || g.HealthCode != tc.code || g.HealthMessage != tc.proof {
				t.Fatalf("health = %v/%q/%q/%q, want %v/%q/%q/%q", g.Healthy, g.HealthStatus, g.HealthCode, g.HealthMessage,
					tc.healthy, tc.status, tc.code, tc.proof)
			}
		})
	}
}

// A driver that rejects a field name fails the whole query; the probe must
// fall back tier by tier instead of losing the inventory.
func TestNVIDIAProbeFallsBackWhenDriverRejectsHealthFields(t *testing.T) {
	var queries []string
	p := &SMIProber{hostRoot: "/nonexistent-nvidia-test-root", runner: func(_ context.Context, args ...string) (string, error) {
		if len(args) > 0 && strings.HasPrefix(args[0], "--query-gpu=") {
			queries = append(queries, args[0])
			if strings.Contains(args[0], "remapped_rows.pending") {
				return `Field "remapped_rows.pending" is not a valid field to query.`, errors.New("exit status 2")
			}
			return "0, GPU-a, NVIDIA V100, 32768, 7.0, 00000000:65:00.0, 470.10, 300, 2, 2\n", nil
		}
		return "", errors.New("unexpected call")
	}}
	gpus, err := p.Probe(t.Context())
	if err != nil || len(gpus) != 1 {
		t.Fatalf("Probe = %+v, %v", gpus, err)
	}
	if len(queries) != 3 || strings.Contains(queries[2], "remapped_rows") || !strings.Contains(queries[2], "ecc.errors.uncorrected.aggregate.total") {
		t.Fatalf("queries = %q", queries)
	}
	if gpus[0].Healthy || gpus[0].HealthCode != "memory_uncorrectable_errors" {
		t.Fatalf("gpu = %+v", gpus[0])
	}
	// The next probe starts where this driver answered: the fields it does
	// not know are not paid for every minute.
	queries = nil
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || strings.Contains(queries[0], "remapped_rows") {
		t.Fatalf("second probe queries = %q, want only the one the driver answers", queries)
	}
}

// The recovery the driver asks for (gpu_recovery_action, R550 and later)
// follows the memory fields; "None" as read on gpu3 (driver 580) is no
// finding.
func TestNVIDIARecoveryAction(t *testing.T) {
	for value, want := range map[string]string{
		"None": "", "[N/A]": "", "": "",
		"Reset": accelerator.HealthResetRequired, "Drain and Reset": accelerator.HealthResetRequired,
		"Drain P2P": accelerator.HealthResetRequired, "Reboot": accelerator.HealthRebootRequired,
		"Node Reboot": accelerator.HealthRebootRequired, "Something New": accelerator.HealthResetRequired,
	} {
		if got := nvidiaRecoveryCode(value); got != want {
			t.Errorf("nvidiaRecoveryCode(%q) = %q, want %q", value, got, want)
		}
	}
	gpus := parseNVIDIAInfo("0, GPU-a, NVIDIA H100, 81559, 9.0, 00000000:17:00.0, 580.126.09, 700, 0, 0, No, No, No, Reboot\n")
	if len(gpus) != 1 || gpus[0].HealthCode != accelerator.HealthRebootRequired || gpus[0].Healthy ||
		gpus[0].HealthMessage != "gpu_recovery_action=Reboot" {
		t.Fatalf("gpu = %+v", gpus)
	}
	gpus = parseNVIDIAInfo("0, GPU-a, Quadro RTX 8000, 46080, 7.5, 00000000:17:00.0, 580.126.09, 260, 0, 0, No, No, No, None\n")
	if len(gpus) != 1 || gpus[0].HealthCode != "" || !gpus[0].Healthy {
		t.Fatalf("gpu = %+v", gpus)
	}
}

func TestParseNVIDIAInfoKeepsLegacySevenFieldInventory(t *testing.T) {
	gpus := parseNVIDIAInfo("0, GPU-a, NVIDIA T4, 16384, 7.5, 00000000:65:00.0, 535.54\n")
	if len(gpus) != 1 || gpus[0].UUID != "GPU-a" || !gpus[0].Healthy || gpus[0].PowerLimitWatts != 0 {
		t.Fatalf("legacy inventory = %+v", gpus)
	}
}

func TestEnrichSingleVisibleNVIDIAMIGUsesProfileMemory(t *testing.T) {
	gpus := parseNVIDIAInfo("0, GPU-parent, NVIDIA RTX PRO 6000 Blackwell Server Edition, N/A, 12.0, 00000000:84:00.0, 580.95\n")
	enrichSingleVisibleNVIDIAMIG(gpus, `GPU 0: NVIDIA RTX PRO 6000 Blackwell Server Edition (UUID: GPU-parent)
  MIG 1g.24gb     Device  0: (UUID: MIG-visible)
`)
	if len(gpus) != 1 || gpus[0].MemoryTotalBytes != 24<<30 || !gpus[0].MIGEnabled {
		t.Fatalf("single visible MIG inventory = %+v", gpus)
	}
	if gpus[0].UUID != "GPU-parent" || gpus[0].Index != 0 {
		t.Fatalf("MIG enrichment changed stable parent identity: %+v", gpus[0])
	}
}

func TestEnrichSingleVisibleNVIDIAMIGDoesNotAggregateMultipleSlices(t *testing.T) {
	gpus := parseNVIDIAInfo("0, GPU-parent, NVIDIA A100, N/A, 8.0, 00000000:84:00.0, 570.00\n")
	enrichSingleVisibleNVIDIAMIG(gpus, `GPU 0: NVIDIA A100 (UUID: GPU-parent)
  MIG 1g.10gb Device 0: (UUID: MIG-first)
  MIG 1g.10gb Device 1: (UUID: MIG-second)
`)
	if len(gpus) != 1 || gpus[0].MemoryTotalBytes != 0 || gpus[0].MIGEnabled {
		t.Fatalf("multiple MIG slices were incorrectly collapsed: %+v", gpus)
	}
}

func TestApplyUnifiedMemoryFallbackFillsGB10FromHost(t *testing.T) {
	gpus := parseNVIDIAInfo("0, GPU-gb10, NVIDIA GB10, N/A, 12.1, 0000000F:01:00.0, 580.178.04, N/A\n")
	applyUnifiedMemoryFallback(gpus, func() int64 { return 127598832 * 1024 })
	if len(gpus) != 1 || gpus[0].MemoryTotalBytes != 127598832*1024 {
		t.Fatalf("GB10 unified memory not filled from host: %+v", gpus)
	}
	if gpus[0].MIGEnabled {
		t.Fatalf("unified fallback must not mark the GPU as MIG: %+v", gpus[0])
	}
}

func TestApplyUnifiedMemoryFallbackKeepsParsedMemoryAndDiscreteZeros(t *testing.T) {
	gpus := parseNVIDIAInfo(`0, GPU-a, NVIDIA GB10, 119444, 12.1, 0000000F:01:00.0, 580.178.04
1, GPU-b, NVIDIA RTX PRO 6000 Blackwell Server Edition, N/A, 12.0, 00000000:84:00.0, 580.95
`)
	applyUnifiedMemoryFallback(gpus, func() int64 { return 127598832 * 1024 })
	if gpus[0].MemoryTotalBytes != 119444*1024*1024 {
		t.Fatalf("parsed GB10 memory was overwritten: %+v", gpus[0])
	}
	if gpus[1].MemoryTotalBytes != 0 {
		t.Fatalf("discrete GPU with unparsable memory got a host fallback: %+v", gpus[1])
	}
}

func TestApplyUnifiedMemoryFallbackSkipsWhenHostUnknown(t *testing.T) {
	gpus := parseNVIDIAInfo("0, GPU-gb10, NVIDIA GB10, N/A, 12.1, 0000000F:01:00.0, 580.178.04\n")
	applyUnifiedMemoryFallback(gpus, func() int64 { return 0 })
	if gpus[0].MemoryTotalBytes != 0 {
		t.Fatalf("fallback invented memory without host facts: %+v", gpus[0])
	}
}

// When nvidia-smi does not answer (on zstone-164 every call waits on the
// driver lock behind a card that failed to initialize), the driver version
// comes from the loaded kernel module, and is not remembered in place of
// nvidia-smi's own answer.
func TestNVIDIADriverVersionFallsBackToTheKernelModule(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys/module/nvidia"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys/module/nvidia/version"), []byte("580.126.09\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	answer := false
	p := &SMIProber{hostRoot: root, runner: func(_ context.Context, args ...string) (string, error) {
		if answer {
			return "580.126.09\n", nil
		}
		return "", fmt.Errorf("nvidia-smi: no answer within 10s: %w", ErrCLIUnresponsive)
	}}
	if got := p.DriverVersion(t.Context()); got != "580.126.09" {
		t.Fatalf("DriverVersion = %q, want the module's version", got)
	}
	if p.driver != "" {
		t.Fatalf("module version remembered as %q", p.driver)
	}
	answer = true
	if got := p.DriverVersion(t.Context()); got != "580.126.09" || p.driver != "580.126.09" {
		t.Fatalf("DriverVersion = %q, remembered %q", got, p.driver)
	}

	if got := (&SMIProber{hostRoot: t.TempDir(), runner: func(context.Context, ...string) (string, error) {
		return "", errors.New("NVIDIA-SMI has failed")
	}}).DriverVersion(t.Context()); got != "" {
		t.Fatalf("DriverVersion without nvidia-smi or module = %q", got)
	}
}
