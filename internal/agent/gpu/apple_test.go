package gpu

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// appleHost answers the commands AppleProber runs with what one Mac printed.
// The fixtures are verbatim captures from an M1 Max (64 GiB, macOS 15.4.1),
// except the platform UUID, which is replaced.
type appleHost struct {
	sysctl, accel, platform string
	sysctlErr, ioregErr     error
	calls                   []string
}

func newM1MaxHost(t *testing.T) *appleHost {
	t.Helper()
	read := func(name string) string {
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	return &appleHost{
		sysctl:   read("apple-m1max-sysctl.txt"),
		accel:    read("apple-m1max-ioreg-accelerator.txt"),
		platform: read("apple-m1max-ioreg-platform.txt"),
	}
}

func (h *appleHost) run(_ context.Context, name string, args ...string) (string, error) {
	h.calls = append(h.calls, name+" "+strings.Join(args, " "))
	switch {
	case name == "sysctl":
		return h.sysctl, h.sysctlErr
	case name == "ioreg" && strings.Contains(strings.Join(args, " "), "IOAccelerator"):
		return h.accel, h.ioregErr
	case name == "ioreg" && strings.Contains(strings.Join(args, " "), "IOPlatformExpertDevice"):
		return h.platform, h.ioregErr
	}
	return "", errors.New("unexpected command " + name)
}

func (h *appleHost) prober(goos string) *AppleProber {
	return &AppleProber{run: h.run, goos: goos}
}

func TestAppleProberReportsTheSoCAsOneUnifiedMemoryCard(t *testing.T) {
	host := newM1MaxHost(t)
	p := host.prober("darwin")
	if !p.Available() {
		t.Fatal("Available() = false on an Apple silicon Mac")
	}
	gpus, err := p.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 {
		t.Fatalf("Probe() = %d GPUs, want 1", len(gpus))
	}
	g := gpus[0]
	if g.Index != 0 || g.Name != "Apple M1 Max" || g.Vendor != accelerator.VendorApple || g.ArchID != "M1 Max" {
		t.Fatalf("identity = %+v", g)
	}
	// Metal reports 48 GiB as the recommended working set of this 64 GiB Mac;
	// vllm-metal logged "Set Metal wired_limit to 48.0 GB" on it.
	if g.MemoryTotalBytes != 48<<30 {
		t.Fatalf("MemoryTotalBytes = %d, want %d", g.MemoryTotalBytes, int64(48<<30))
	}
	if g.UUID != "APPLE-00000000-1111-2222-3333-444455556666" || g.DriverVersion != "15.4.1" || !g.Healthy {
		t.Fatalf("UUID/driver/health = %q %q %v", g.UUID, g.DriverVersion, g.Healthy)
	}
	if !accelerator.UnifiedMemory(g.Name) {
		t.Fatalf("UnifiedMemory(%q) = false", g.Name)
	}
	if arch := accelerator.GPUArch(g.Vendor, g.ArchID, g.ComputeCapability); arch.Family != "m1" {
		t.Fatalf("GPUArch family = %q, want m1", arch.Family)
	}
	if framework, version := p.Framework(context.Background()); framework != "metal" || version != "15.4.1" {
		t.Fatalf("Framework() = %q %q", framework, version)
	}
	if accelerator.FrameworkVendor("metal") != p.Vendor() {
		t.Fatal("the reported framework must derive the prober's own vendor")
	}
}

func TestAppleProberIgnoresHostsThatAreNotAppleSilicon(t *testing.T) {
	host := newM1MaxHost(t)
	// Linux: the probe never runs a command at all, so every Linux agent's
	// start-up pays nothing for it.
	if host.prober("linux").Available() {
		t.Fatal("Available() = true on linux")
	}
	if gpus, err := host.prober("linux").Probe(context.Background()); err != nil || gpus != nil {
		t.Fatalf("Probe() on linux = %v, %v", gpus, err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("commands run on linux: %v", host.calls)
	}

	// An Intel Mac has sysctl but no iogpu keys and an Intel brand string:
	// sysctl prints what it knows and exits 1.
	host.sysctl = "hw.memsize: 17179869184\nsysctl: unknown oid 'iogpu.wired_limit_mb'\nkern.osproductversion: 13.6\nmachdep.cpu.brand_string: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz\n"
	host.sysctlErr = errors.New("exit status 1")
	p := host.prober("darwin")
	if p.Available() {
		t.Fatal("Available() = true on an Intel Mac")
	}
	if gpus, err := p.Probe(context.Background()); err != nil || gpus != nil {
		t.Fatalf("Probe() on an Intel Mac = %v, %v", gpus, err)
	}
}

func TestAppleProberKeepsTheFactsSysctlPrintsBeforeAnUnknownKey(t *testing.T) {
	host := newM1MaxHost(t)
	host.sysctl = "hw.memsize: 68719476736\nsysctl: unknown oid 'iogpu.wired_limit_mb'\nkern.osproductversion: 15.4.1\nmachdep.cpu.brand_string: Apple M1 Max\n"
	host.sysctlErr = errors.New("exit status 1")
	gpus, err := host.prober("darwin").Probe(context.Background())
	if err != nil || len(gpus) != 1 || gpus[0].MemoryTotalBytes != 48<<30 {
		t.Fatalf("Probe() = %+v, %v", gpus, err)
	}
}

func TestAppleProberPropagatesCommandFailures(t *testing.T) {
	host := newM1MaxHost(t)
	host.sysctl = ""
	host.sysctlErr = ErrCLIUnresponsive
	if _, err := host.prober("darwin").Probe(context.Background()); !errors.Is(err, ErrCLIUnresponsive) {
		t.Fatalf("Probe() with sysctl unresponsive = %v", err)
	}
	if _, err := host.prober("darwin").Metrics(context.Background()); !errors.Is(err, ErrCLIUnresponsive) {
		t.Fatalf("Metrics() with sysctl unresponsive = %v", err)
	}

	host = newM1MaxHost(t)
	host.ioregErr = errors.New("ioreg: killed")
	if _, err := host.prober("darwin").Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "ioreg") {
		t.Fatalf("Probe() with ioreg failing = %v", err)
	}
}

func TestAppleProberFallsBackToTheBrandWhenIoregNamesNoModel(t *testing.T) {
	host := newM1MaxHost(t)
	host.accel = "+-o AGXAcceleratorG13X\n    {\n    }\n"
	host.platform = ""
	gpus, err := host.prober("darwin").Probe(context.Background())
	if err != nil || len(gpus) != 1 {
		t.Fatalf("Probe() = %+v, %v", gpus, err)
	}
	if gpus[0].Name != "Apple M1 Max" || gpus[0].UUID != "" {
		t.Fatalf("name/uuid = %q %q", gpus[0].Name, gpus[0].UUID)
	}
}

func TestAppleProberMetrics(t *testing.T) {
	host := newM1MaxHost(t)
	host.accel = regexp.MustCompile(`"Device Utilization %"=\d+`).ReplaceAllString(host.accel, `"Device Utilization %"=87`)
	host.accel = regexp.MustCompile(`"In use system memory"=\d+`).ReplaceAllString(host.accel, `"In use system memory"=319356928`)
	metrics, err := host.prober("darwin").Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || metrics[0].Index != 0 || metrics[0].Vendor != accelerator.VendorApple || metrics[0].UUID == "" {
		t.Fatalf("Metrics() = %+v", metrics)
	}
	want := map[string]float64{
		"gpuworks_gpu_utilization":        87,
		"gpuworks_gpu_memory_used_bytes":  319356928,
		"gpuworks_gpu_memory_total_bytes": 48 << 30,
	}
	for key, value := range want {
		if got, ok := metrics[0].Values[key]; !ok || got != value {
			t.Errorf("%s = %v (present %v), want %v", key, got, ok, value)
		}
	}

	// A registry entry without statistics reports what it has and invents
	// no zeros.
	host.accel = "    \"model\" = \"Apple M1 Max\"\n"
	metrics, err = host.prober("darwin").Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := metrics[0].Values["gpuworks_gpu_utilization"]; ok {
		t.Fatalf("utilization invented: %v", metrics[0].Values)
	}
	if _, ok := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; ok {
		t.Fatalf("used memory invented: %v", metrics[0].Values)
	}
}

// The platform UUID never changes, so the samples after the first do not run
// ioreg for it again; a failed read is retried.
func TestAppleProberReadsThePlatformUUIDOnce(t *testing.T) {
	host := newM1MaxHost(t)
	p := host.prober("darwin")
	reads := func() int {
		n := 0
		for _, call := range host.calls {
			if strings.Contains(call, "IOPlatformExpertDevice") {
				n++
			}
		}
		return n
	}
	host.platform = ""
	if m, err := p.Metrics(context.Background()); err != nil || m[0].UUID != "" {
		t.Fatalf("Metrics without a UUID = %+v, %v", m, err)
	}
	host.platform = newM1MaxHost(t).platform
	var first string
	for i := 0; i < 3; i++ {
		m, err := p.Metrics(context.Background())
		if err != nil || m[0].UUID == "" || (first != "" && m[0].UUID != first) {
			t.Fatalf("sample %d = %+v, %v", i, m, err)
		}
		first = m[0].UUID
	}
	if n := reads(); n != 2 {
		t.Fatalf("ioreg read the platform %d times, want once after the failed read", n)
	}
}

func TestAppleProberRuntimeInjection(t *testing.T) {
	p := &AppleProber{}
	if got := p.RuntimeInjection(nil); got.UnsupportedReason != "" || len(got.Env) != 0 || len(got.Grant.DockerDeviceArgs()) != 0 {
		t.Fatalf("RuntimeInjection(nil) = %+v", got)
	}
	if got := p.RuntimeInjection([]int{0}); got.UnsupportedReason != "" {
		t.Fatalf("RuntimeInjection([0]) = %+v", got)
	}
	if got := p.RuntimeInjection([]int{0, 1}); got.UnsupportedReason == "" {
		t.Fatal("RuntimeInjection([0 1]) must refuse a second GPU")
	}
}

// The rule must agree with the recommendedMaxWorkingSetSize Metal reports;
// the values are those llama.cpp logs on each memory size.
func TestAppleGPUWorkingSet(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []struct {
		mem, wiredMB, want int64
	}{
		{8 * gib, 0, 5726623061},     // 5726.63 MB
		{16 * gib, 0, 11453246122},   // 11453.25 MB
		{24 * gib, 0, 16 * gib},      // 17179.89 MB
		{32 * gib, 0, 22906492245},   // 22906.50 MB
		{36 * gib, 0, 27 * gib},      // 28991.03 MB
		{64 * gib, 0, 48 * gib},      // measured on the M1 Max
		{128 * gib, 0, 96 * gib},     // 103079.22 MB
		{192 * gib, 0, 144 * gib},    // 154618.82 MB
		{64 * gib, 57344, 56 * gib},  // an administrator's iogpu.wired_limit_mb
		{64 * gib, 999999, 64 * gib}, // never more than the machine has
		{0, 0, 0},
	} {
		if got := appleGPUWorkingSet(tc.mem, tc.wiredMB); got != tc.want {
			t.Errorf("appleGPUWorkingSet(%d, %d) = %d, want %d", tc.mem, tc.wiredMB, got, tc.want)
		}
	}
}

func TestIoregDictionarySkipsNestedValues(t *testing.T) {
	got := ioregDictionary(`{"a"=1,"nested"={"x"=2,"y"=(3,4)},"list"=(5,6),"b"=7}`)
	if got["a"] != "1" || got["b"] != "7" || got["nested"] != `{"x"=2,"y"=(3,4)}` {
		t.Fatalf("ioregDictionary = %v", got)
	}
	if _, ok := got["x"]; ok {
		t.Fatalf("nested key leaked: %v", got)
	}
}
