package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestAscendInfoSnapshotIsSharedAcrossConsumers(t *testing.T) {
	var calls [][]string
	p := &AscendProber{infoTTL: time.Hour, runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return strings.ReplaceAll(ascendInfo26Sample, "Warning", "OK"), nil
	}}
	if !p.Available() {
		t.Fatal("expected injected npu-smi to be available")
	}
	if gpus, err := p.Probe(t.Context()); err != nil || len(gpus) != 2 {
		t.Fatalf("Probe = %d, %v", len(gpus), err)
	}
	if got := p.DriverVersion(t.Context()); !strings.Contains(got, "26.0.rc1") {
		t.Fatalf("DriverVersion = %q", got)
	}
	if metrics, err := p.Metrics(t.Context()); err != nil || len(metrics) != 2 {
		t.Fatalf("Metrics = %d, %v", len(metrics), err)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], []string{"info"}) {
		t.Fatalf("npu-smi calls = %+v, want one info call", calls)
	}
}

func TestAscendInfoSnapshotCoalescesFailureAndRetriesAfterTTL(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	p := &AscendProber{infoTTL: time.Hour, infoFailureTTL: 20 * time.Millisecond}
	p.runner = func(_ context.Context, _ ...string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return "", errors.New("driver busy")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = p.Metrics(t.Context())
		}()
	}
	close(start)
	wg.Wait()
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("concurrent failure calls = %d, want 1", gotCalls)
	}
	time.Sleep(25 * time.Millisecond)
	_, _ = p.Metrics(t.Context())
	mu.Lock()
	gotCalls = calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("calls after failure TTL = %d, want 2", gotCalls)
	}
}

func TestAscendTopologyDoesNotRunAvailabilityProbe(t *testing.T) {
	var calls [][]string
	p := &AscendProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "topology", nil
	}}
	if got := p.Topology(t.Context()); got != "topology" {
		t.Fatalf("Topology = %q", got)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], []string{"info", "-t", "topo"}) {
		t.Fatalf("npu-smi calls = %+v", calls)
	}
}

const ascendInfoSample = `
| npu-smi 26.0.rc1 |
| NPU  Name        Health   Memory      Bus-Id        |
| 2    910B4       OK       32768MiB    0000:c1:00.0  |
| 4    Ascend910B4 Warning  32GiB       0000:c2:00.0  |
`

const ascendInfo26Sample = `
+------------------------------------------------------------------------------------------------------------------+
| npu-smi 26.0.rc1                            Version: 26.0.rc1                                                    |
+---------------------------+---------------+----------------------------------------------------------------------+
| NPU   Name                | Health        | Power(W)             Temp(C)                 Hugepages-Usage(page)   |
| Chip                      | Bus-Id        | AICore(%)            Memory-Usage(MB)        HBM-Usage(MB)           |
+===========================+===============+======================================================================+
| 2     910B4               | OK            | 82.4                 32                      0    / 0                |
| 0                         | 0000:81:00.0  | 17                   0    / 0                2857 / 32768            |
+===========================+===============+======================================================================+
| 4     910B4               | Warning       | 77.2                 36                      0    / 0                |
| 0                         | 0000:01:00.0  | 0                    0    / 0                4096 / 32768            |
+===========================+===============+======================================================================+
`

func TestParseAscendInfo(t *testing.T) {
	gpus := parseAscendInfo(ascendInfoSample)
	if len(gpus) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(gpus), gpus)
	}
	if gpus[0].Index != 2 || gpus[0].Vendor != "ascend" || gpus[0].ArchID != "910B4" {
		t.Fatalf("gpu0 identity = %+v", gpus[0])
	}
	if gpus[0].MemoryTotalBytes != 32768<<20 {
		t.Fatalf("gpu0 memory = %d", gpus[0].MemoryTotalBytes)
	}
	if !gpus[0].Healthy {
		t.Fatalf("gpu0 should be healthy: %+v", gpus[0])
	}
	if gpus[0].HealthStatus != "OK" {
		t.Fatalf("gpu0 health status = %q", gpus[0].HealthStatus)
	}
	// The "Ascend" some releases glue to the model is not part of it.
	if gpus[1].Index != 4 || gpus[1].ArchID != "910B4" || gpus[1].Name != "Ascend 910B4" {
		t.Fatalf("gpu1 identity = %+v", gpus[1])
	}
	if !gpus[1].Healthy || gpus[1].HealthStatus != "Warning" || gpus[1].HealthCode != "vendor_health_warning" {
		t.Fatalf("a Warning GPU stays schedulable at Warning: %+v", gpus[1])
	}
	if gpus[1].PCIeBDF != "0000:c2:00.0" {
		t.Fatalf("gpu1 bdf = %q", gpus[1].PCIeBDF)
	}
}

func TestAscendFrameworkUsesASCENDHomePath(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "version.info"), []byte("CANN Version = 9.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASCEND_HOME_PATH", home)
	framework, version := (&AscendProber{}).Framework(context.Background())
	if framework != "cann" || version != "9.0.1" {
		t.Fatalf("framework/version = %q/%q", framework, version)
	}
}

// TestAscendFrameworkReadsHostCANNFilesOnKubernetes: the manifest mounts no
// vendor paths into the agent container, so Framework's glob and read of
// /usr/local/Ascend/... must go through the host root when CLIs run there,
// exactly as the version file a bare ASCEND_HOME_PATH names does.
func TestAscendFrameworkReadsHostCANNFilesOnKubernetes(t *testing.T) {
	withHostRoot(t)
	root := hostRoot
	installInfo := filepath.Join(root, "usr/local/Ascend/cann-8.0.0/version.info")
	if err := os.MkdirAll(filepath.Dir(installInfo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installInfo, []byte("CANN Version = 8.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	framework, version := (&AscendProber{}).Framework(context.Background())
	if framework != "cann" || version != "8.0.0" {
		t.Fatalf("framework/version = %q/%q, want the host's cann-8.0.0", framework, version)
	}
	// Off the host the same tree, visible only under hostRoot, is not found:
	// Framework still names the framework ("cann") but reports no version.
	cliOnHost.Store(false)
	if _, version := (&AscendProber{}).Framework(context.Background()); version != "" {
		t.Fatalf("version off the host = %q, want none (the file is only under hostRoot)", version)
	}
}

func TestRunNPUQuietConfirmsSetPrompt(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "npu-smi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nread answer\n[ \"$answer\" = Y ] || exit 200\nprintf 'confirmed=%s args=%s\\n' \"$answer\" \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := runNPUQuiet(context.Background(), "set", "-t", "device-share", "-i", "4", "-c", "0", "-d", "1")
	if err != nil || !strings.Contains(out, "confirmed=Y") || !strings.Contains(out, "device-share") {
		t.Fatalf("runNPUQuiet = %q, %v", out, err)
	}
}

func TestEnsureDeviceShareReportsWhetherItChangedHostState(t *testing.T) {
	bin := t.TempDir()
	state := filepath.Join(bin, "device-share-enabled")
	script := filepath.Join(bin, "npu-smi")
	content := `#!/bin/sh
set -eu
case "$1" in
  info)
    if test -f "$GPUWORKS_TEST_DEVICE_SHARE_STATE"; then echo 'Device Share Status : true'; else echo 'Device Share Status : false'; fi
    ;;
  set)
    read answer
    test "$answer" = Y
    : > "$GPUWORKS_TEST_DEVICE_SHARE_STATE"
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GPUWORKS_TEST_DEVICE_SHARE_STATE", state)
	prober := &AscendProber{}
	changed, err := prober.EnsureDeviceShare(context.Background(), 4)
	if err != nil || !changed {
		t.Fatalf("first ensure = changed %v, err %v", changed, err)
	}
	changed, err = prober.EnsureDeviceShare(context.Background(), 4)
	if err != nil || changed {
		t.Fatalf("idempotent ensure = changed %v, err %v", changed, err)
	}
}

func TestNPUOutputDetailKeepsActionableTail(t *testing.T) {
	got := npuOutputDetail(strings.Repeat("x", 410) + " invalid card id\n")
	if !strings.HasPrefix(got, ": ...") || !strings.HasSuffix(got, "invalid card id") || len(got) > 405 {
		t.Fatalf("detail = %q", got)
	}
}

func TestParseAscendHealthDetail(t *testing.T) {
	const sample = `
Health Status     : Warning
Error Code        : 81078603
Error Information : node type=PORT, sensor type=Safety State,
                    event state: port link status change(up to down)
`
	status, code, message := parseAscendHealthDetail(sample)
	if status != "Warning" || code != "81078603" {
		t.Fatalf("status/code = %q/%q", status, code)
	}
	wantMessage := "node type=PORT, sensor type=Safety State, event state: port link status change(up to down)"
	if message != wantMessage {
		t.Fatalf("message = %q, want %q", message, wantMessage)
	}
}

func TestEnrichAscendHealthDetailsOnlyQueriesFlaggedGPUs(t *testing.T) {
	flagged := func(index, npu, chip int) ascendChip {
		return ascendChip{gpu: proto.GPUInfo{Index: index, Healthy: true, HealthStatus: "Warning", HealthCode: "vendor_health_warning",
			HealthMessage: accelerator.FormatHealthEvidence(ascendEvidence(npu, chip, "Warning"))}, npu: npu, chip: chip}
	}
	chips := []ascendChip{
		{gpu: proto.GPUInfo{Index: 2, Healthy: true, HealthStatus: "OK"}, npu: 2},
		flagged(4, 4, 0),
		// The second chip of a 910C card is asked about as that chip.
		flagged(7, 3, 1),
	}
	var calls [][]string
	enrichAscendHealthDetails(context.Background(), chips, func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "Health Status: Warning\nError Code: 81078603\nError Information: port link status change(up to down)\n", nil
	})
	want := [][]string{
		{"info", "-t", "health", "-i", "4", "-c", "0"},
		{"info", "-t", "health", "-i", "3", "-c", "1"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %+v, want %v", calls, want)
	}
	wantEvidence := "NPU=3\nChip=1\nHealth=Warning\nHealth Status=Warning\nError Code=81078603\nError Information=port link status change(up to down)"
	if chips[2].gpu.HealthMessage != wantEvidence || chips[1].gpu.HealthCode != "vendor_health_warning" || !chips[1].gpu.Healthy {
		t.Fatalf("flagged detail = %+v", chips)
	}
	if chips[0].gpu.HealthCode != "" || chips[0].gpu.HealthMessage != "" {
		t.Fatalf("healthy chip was enriched: %+v", chips[0].gpu)
	}
}

// The detail view can be more severe than the summary row; the chip takes the
// more severe verdict.
func TestEnrichAscendHealthDetailsEscalatesToDetailVerdict(t *testing.T) {
	chips := []ascendChip{{gpu: proto.GPUInfo{Index: 4, Healthy: true, HealthStatus: "Warning", HealthCode: "vendor_health_warning",
		HealthMessage: accelerator.FormatHealthEvidence(ascendEvidence(4, 0, "Warning"))}, npu: 4}}
	enrichAscendHealthDetails(context.Background(), chips, func(context.Context, ...string) (string, error) {
		return "Health Status: Critical\nError Code: 80CB8009\nError Information: HBM multi-bit ECC\n", nil
	})
	if g := chips[0].gpu; g.Healthy || g.HealthStatus != "Error" || g.HealthCode != "vendor_health_fault" {
		t.Fatalf("detail verdict ignored: %+v", g)
	}
}

// Once npu-smi stops answering, the remaining chips are not asked: each
// would wait out the same stall, holding up the whole status report.
func TestEnrichAscendHealthDetailsStopsAtUnresponsiveCLI(t *testing.T) {
	var chips []ascendChip
	for i := 0; i < 4; i++ {
		chips = append(chips, ascendChip{gpu: proto.GPUInfo{Index: i, Healthy: true, HealthStatus: "Warning", HealthCode: "vendor_health_warning"}, npu: i})
	}
	calls := 0
	enrichAscendHealthDetails(context.Background(), chips, func(context.Context, ...string) (string, error) {
		calls++
		return "", fmt.Errorf("npu-smi: no answer within 10s: %w", ErrCLIUnresponsive)
	})
	if calls != 1 {
		t.Fatalf("npu-smi health calls after a stall = %d, want 1", calls)
	}
	for _, c := range chips {
		if c.gpu.HealthCode != "vendor_health_warning" || !c.gpu.Healthy {
			t.Fatalf("summary verdict changed: %+v", c.gpu)
		}
	}
}

func TestAscendHealthLevels(t *testing.T) {
	for raw, code := range map[string]string{"OK": "", "Warning": "vendor_health_warning", "Alarm": "vendor_health_fault", "Critical": "vendor_health_fault", "": ""} {
		if got := ascendHealthCode(raw); got != code {
			t.Errorf("ascendHealthCode(%q) = %q, want %q", raw, got, code)
		}
	}
	if got := ascendHealthStatus([]string{"1", "910B4", "Alarm", "78.5"}); got != "Alarm" {
		t.Errorf("Alarm row health = %q", got)
	}
}

func TestEnrichAscendHealthDetailsKeepsSummaryWhenQueryFails(t *testing.T) {
	summary := accelerator.FormatHealthEvidence(ascendEvidence(4, 0, "Warning"))
	chips := []ascendChip{{gpu: proto.GPUInfo{Index: 4, Healthy: true, HealthStatus: "Warning", HealthCode: "vendor_health_warning", HealthMessage: summary}, npu: 4}}
	enrichAscendHealthDetails(context.Background(), chips, func(context.Context, ...string) (string, error) {
		return "", errors.New("npu-smi detail unavailable")
	})
	if g := chips[0].gpu; !g.Healthy || g.HealthStatus != "Warning" || g.HealthCode != "vendor_health_warning" || g.HealthMessage != summary {
		t.Fatalf("summary was not preserved: %+v", g)
	}
}

func TestRuntimeInjection(t *testing.T) {
	nv := (&SMIProber{}).RuntimeInjection([]int{2, 4})
	if got := nv.Env["CUDA_VISIBLE_DEVICES"]; got != "2,4" {
		t.Fatalf("CUDA_VISIBLE_DEVICES = %q", got)
	}
	if len(nv.DockerArgs) != 2 || nv.DockerArgs[0] != "--gpus" || nv.DockerArgs[1] != `"device=2,4"` {
		t.Fatalf("nvidia docker args = %+v", nv.DockerArgs)
	}

	asc := (&AscendProber{}).RuntimeInjection([]int{2, 4})
	if got := asc.Env["ASCEND_VISIBLE_DEVICES"]; got != "2,4" {
		t.Fatalf("ASCEND_VISIBLE_DEVICES = %q", got)
	}
	if got := asc.Env["ASCEND_RT_VISIBLE_DEVICES"]; got != "0,1" {
		t.Fatalf("ASCEND_RT_VISIBLE_DEVICES = %q", got)
	}
	if got := asc.Env["NPU_VISIBLE_DEVICES"]; got != "0,1" {
		t.Fatalf("NPU_VISIBLE_DEVICES = %q", got)
	}
	if len(asc.DockerArgs) != 2 || asc.DockerArgs[0] != "--runtime" || asc.DockerArgs[1] != "ascend" {
		t.Fatalf("ascend docker args = %+v", asc.DockerArgs)
	}
}

// withDeviceNodes makes the per-host device nodes a fact of the test rather
// than of the build host's /dev.
func withDeviceNodes(t *testing.T, present ...string) {
	t.Helper()
	previous := deviceNodeExists
	t.Cleanup(func() { deviceNodeExists = previous })
	set := make(map[string]bool, len(present))
	for _, path := range present {
		set[path] = true
	}
	deviceNodeExists = func(path string) bool { return set[path] }
}

func TestRuntimeInjectionForDriverNerdctl(t *testing.T) {
	withDeviceNodes(t, "/dev/davinci_manager", "/dev/devmm_svm", "/dev/hisi_hdc")
	asc := (&AscendProber{}).RuntimeInjectionForDriver([]int{2, 4}, "nerdctl")
	if _, ok := asc.Env[accelerator.AscendEnvVisibleDevices]; ok {
		t.Fatalf("nerdctl injection must not set %s (no plugin reads it): %+v", accelerator.AscendEnvVisibleDevices, asc.Env)
	}
	if got := asc.Env[accelerator.AscendEnvRTVisibleDevices]; got != "0,1" {
		t.Fatalf("ASCEND_RT_VISIBLE_DEVICES = %q", got)
	}
	if got := asc.Env[accelerator.AscendEnvNPUVisible]; got != "0,1" {
		t.Fatalf("NPU_VISIBLE_DEVICES = %q", got)
	}
	wantArgs := []string{
		"--device", "/dev/davinci2",
		"--device", "/dev/davinci4",
		"--device", "/dev/davinci_manager",
		"--device", "/dev/devmm_svm",
		"--device", "/dev/hisi_hdc",
	}
	if strings.Join(asc.DockerArgs, ",") != strings.Join(wantArgs, ",") {
		t.Fatalf("nerdctl device args = %+v, want %+v", asc.DockerArgs, wantArgs)
	}
	for _, arg := range asc.DockerArgs {
		if strings.Contains(arg, ":") {
			t.Fatalf("nerdctl device args must not rename host paths (fails on real hardware): %q", arg)
		}
	}
	if len(asc.Mounts) == 0 {
		t.Fatal("nerdctl injection must bind-mount the host driver/DCMI libraries")
	}
	for _, m := range asc.Mounts {
		if !m.ReadOnly {
			t.Fatalf("nerdctl driver mount %q must be read-only", m.Source)
		}
	}

	// Every other driver keeps the Ascend Docker Runtime plugin path.
	dockerInj := (&AscendProber{}).RuntimeInjectionForDriver([]int{2, 4}, "docker")
	if len(dockerInj.DockerArgs) != 2 || dockerInj.DockerArgs[0] != "--runtime" || dockerInj.DockerArgs[1] != "ascend" {
		t.Fatalf("docker driver must be unaffected by the nerdctl branch: %+v", dockerInj.DockerArgs)
	}
	if len(dockerInj.Mounts) != 0 {
		t.Fatalf("docker driver injection must not carry nerdctl-only mounts: %+v", dockerInj.Mounts)
	}
}

func TestParseAscendMetrics(t *testing.T) {
	const sample = `
| NPU  Name        Health  AICore(%)  Memory-Usage(MB)  Temp(C)  Power(W) |
| 2    910B4       OK      77%        1024/32768MB      43C      88W      |
| 4    Ascend910B4 Warning 12%        2048/32768MB      45C      92W      |
`
	metrics := parseAscendMetrics(sample)
	if len(metrics) != 2 {
		t.Fatalf("metrics = %d, want 2: %+v", len(metrics), metrics)
	}
	m := metrics[0]
	if m.Index != 2 || m.UUID != "ascend-2" || m.Vendor != "ascend" {
		t.Fatalf("identity = %+v", m)
	}
	if m.Values["gpuworks_gpu_utilization"] != 77 {
		t.Fatalf("utilization = %v", m.Values["gpuworks_gpu_utilization"])
	}
	if m.Values["gpuworks_gpu_memory_used_bytes"] != 1024<<20 {
		t.Fatalf("memory used = %v", m.Values["gpuworks_gpu_memory_used_bytes"])
	}
	if m.Values["gpuworks_gpu_memory_total_bytes"] != 32768<<20 {
		t.Fatalf("memory total = %v", m.Values["gpuworks_gpu_memory_total_bytes"])
	}
	if m.Values["gpuworks_gpu_temperature_celsius"] != 43 {
		t.Fatalf("temperature = %v", m.Values["gpuworks_gpu_temperature_celsius"])
	}
	if m.Values["gpuworks_gpu_power_watts"] != 88 {
		t.Fatalf("power = %v", m.Values["gpuworks_gpu_power_watts"])
	}
}

func TestParseAscendInfo26TwoLineRows(t *testing.T) {
	gpus := parseAscendInfo(ascendInfo26Sample)
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2: %+v", len(gpus), gpus)
	}
	if gpus[0].Index != 2 || gpus[0].MemoryTotalBytes != 32768<<20 || gpus[0].PCIeBDF != "0000:81:00.0" || !gpus[0].Healthy {
		t.Fatalf("gpu 2 = %+v", gpus[0])
	}
	if gpus[1].Index != 4 || gpus[1].MemoryTotalBytes != 32768<<20 || gpus[1].PCIeBDF != "0000:01:00.0" || gpus[1].HealthStatus != "Warning" {
		t.Fatalf("gpu 4 = %+v", gpus[1])
	}
}

func TestParseAscendMetrics26TwoLineRows(t *testing.T) {
	metrics := parseAscendMetrics(ascendInfo26Sample)
	if len(metrics) != 2 {
		t.Fatalf("metrics = %d, want 2: %+v", len(metrics), metrics)
	}
	first := metrics[0].Values
	if first["gpuworks_gpu_memory_used_bytes"] != 2857<<20 || first["gpuworks_gpu_memory_total_bytes"] != 32768<<20 {
		t.Fatalf("memory metrics = %+v", first)
	}
	if first["gpuworks_gpu_utilization"] != 17 || first["gpuworks_gpu_power_watts"] != 82.4 || first["gpuworks_gpu_temperature_celsius"] != 32 {
		t.Fatalf("telemetry metrics = %+v", first)
	}
	second := metrics[1].Values
	if second["gpuworks_gpu_utilization"] != 0 || second["gpuworks_gpu_memory_used_bytes"] != 4096<<20 {
		t.Fatalf("second metrics = %+v", second)
	}
}

func TestNerdctlInjectionPassesOnlyTheDeviceNodesTheHostHas(t *testing.T) {
	withDeviceNodes(t, "/dev/davinci_manager", "/dev/ummu", "/dev/uburma")
	asc := (&AscendProber{}).RuntimeInjectionForDriver([]int{3}, "nerdctl")
	want := []string{
		"--device", "/dev/davinci3",
		"--device", "/dev/davinci_manager",
		"--device", "/dev/ummu",
		"--device", "/dev/uburma",
	}
	if strings.Join(asc.DockerArgs, ",") != strings.Join(want, ",") {
		t.Fatalf("device args = %+v, want %+v", asc.DockerArgs, want)
	}

	withDeviceNodes(t)
	asc = (&AscendProber{}).RuntimeInjectionForDriver([]int{3}, "nerdctl")
	if strings.Join(asc.DockerArgs, ",") != "--device,/dev/davinci3" {
		t.Fatalf("device args without any shared node = %+v", asc.DockerArgs)
	}
}
