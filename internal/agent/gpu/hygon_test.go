package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHygonInspectAndRequireDeviceSeeTheLiveHostDev: a privileged
// container's own /dev is a copy made at container start, so a device node
// the driver creates afterward (/opt/hyhal, /dev/kfd, /dev/mkfd) is missing
// there; the default inspect (no injected p.stat) must check the host's
// live file system through hostRoot when CLIs run there.
func TestHygonInspectAndRequireDeviceSeeTheLiveHostDev(t *testing.T) {
	withHostRoot(t)
	root := hostRoot
	if err := os.MkdirAll(filepath.Join(root, "opt/hyhal"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &HygonProber{}
	if info, err := p.inspect("/opt/hyhal"); err != nil || !info.IsDir() {
		t.Fatalf("inspect(/opt/hyhal) = %+v, %v", info, err)
	}
	if _, err := p.inspect("/opt/hyhal-missing"); err == nil {
		t.Fatal("inspect found a directory that does not exist under hostRoot")
	}
	// requireDevice wants a device node, not a plain directory.
	if err := p.requireDevice("/opt/hyhal"); err == nil || !strings.Contains(err.Error(), "not a device node") {
		t.Fatalf("requireDevice(a directory) = %v", err)
	}
	// Off the host the same tree, visible only under hostRoot, is not found.
	cliOnHost.Store(false)
	if _, err := p.inspect("/opt/hyhal"); err == nil {
		t.Fatal("inspect off the host found a directory that only exists under hostRoot")
	}
}

func TestHygonProbeLibrariesResolveHostDTKClosure(t *testing.T) {
	dir := t.TempDir()
	names := []string{"libz.so.1", "libtinfo.so.5", "libtinfo.so.6", "libelf.so.1"}
	var listing strings.Builder
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&listing, "%s (libc6,x86-64) => %s\n", name, path)
	}
	ldconfig := filepath.Join(dir, "ldconfig")
	if err := os.WriteFile(ldconfig, []byte("#!/bin/sh\n/bin/cat <<'LIBS'\n"+listing.String()+"LIBS\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := (&HygonProber{}).PartitionProbeLibraries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(names) {
		t.Fatalf("probe libraries = %+v", got)
	}
	for i, name := range names {
		if got[i] != (ProbeLibrary{Source: filepath.Join(dir, name), Name: name}) {
			t.Fatalf("probe library %d = %+v", i, got[i])
		}
	}
}

func TestParseHygonInventoryFromRealK100Capture(t *testing.T) {
	raw := readHygonFixture(t)
	gpus := parseHygonInventory(raw, func(path string) ([]byte, error) {
		switch path {
		case "/sys/bus/pci/devices/0000:19:00.0/unique_id":
			return []byte("0x7100f95cee823041\n"), nil
		case "/sys/bus/pci/devices/0000:19:00.0/numa_node":
			return []byte("0\n"), nil
		case "/sys/bus/pci/devices/0000:67:00.0/unique_id":
			return []byte("0x715322fd7cac4121\n"), nil
		case "/sys/bus/pci/devices/0000:67:00.0/numa_node":
			return []byte("1\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	})
	if len(gpus) != 2 {
		t.Fatalf("gpus = %d, want 2: %+v", len(gpus), gpus)
	}
	first := gpus[0]
	if first.Index != 0 || first.Name != "K100_AI" || first.Vendor != "hygon" {
		t.Fatalf("first identity = %+v", first)
	}
	if first.UUID != "GPU-7100f95cee823041" || first.PCIeBDF != "0000:19:00.0" || first.NUMANode != 0 {
		t.Fatalf("first host identity = %+v", first)
	}
	if first.ArchID != "K100AI" {
		t.Fatalf("first normalized SKU = %q", first.ArchID)
	}
	if first.MemoryTotalBytes != 65520*1024*1024 || !first.Healthy || first.HealthStatus != "OK" {
		t.Fatalf("first capacity/health = %+v", first)
	}
	if gpus[1].PCIeBDF != "0000:67:00.0" || gpus[1].NUMANode != 1 {
		t.Fatalf("second host identity = %+v", gpus[1])
	}
}

func TestParseHygonInventoryDoesNotGuessMissingSKU(t *testing.T) {
	raw := "HCU[0] : Card Series:\nHCU[0] : PCI Bus: 0000:19:00.0\nHCU[0] : vram Total Memory (MiB): 65520\n"
	gpus := parseHygonInventory(raw, nil)
	if len(gpus) != 1 {
		t.Fatalf("gpus = %+v", gpus)
	}
	if gpus[0].Name != "Hygon DCU" || gpus[0].ArchID != "" {
		t.Fatalf("missing product name was guessed: %+v", gpus[0])
	}
}

// A card that names only its series is stored as the series, not as no
// identity: K100 is still a K100 even if not which one.
func TestParseHygonInventoryKeepsTheSeriesOfAnAmbiguousSKU(t *testing.T) {
	raw := "HCU[0] : Card Series: K100\nHCU[0] : PCI Bus: 0000:19:00.0\nHCU[0] : vram Total Memory (MiB): 65520\n"
	gpus := parseHygonInventory(raw, nil)
	if len(gpus) != 1 || gpus[0].ArchID != "K100" || gpus[0].Name != "K100" {
		t.Fatalf("gpus = %+v", gpus)
	}
}

func TestNormalizeHygonSKUUsesExactAliasesOnly(t *testing.T) {
	tests := map[string]string{
		"K100_AI": "K100AI",
		"K100 AI": "K100AI",
		"Z100":    "Z100",
		"Z-100":   "Z100",
		"BW1000":  "BW1000",
		"BW":      "BW",
		"BW1101":  "BW1101",
		"K100":    "K100_FAMILY_AMBIGUOUS",
		"Hygon":   "UNKNOWN_HYGON_SKU",
		"gfx928":  "UNKNOWN_HYGON_SKU",
	}
	for raw, want := range tests {
		if got := normalizeHygonSKU(raw); got != want {
			t.Errorf("normalizeHygonSKU(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseHygonZ100Inventory(t *testing.T) {
	raw := "DCU[0]: Card Series: Z100\nDCU[0]: PCI Bus: 0000:1a:00.0\nDCU[0]: vram Total Memory (MiB): 32752\n"
	gpus := parseHygonInventory(raw, nil)
	if len(gpus) != 1 || gpus[0].ArchID != "Z100" || gpus[0].MemoryTotalBytes != 32752*1024*1024 {
		t.Fatalf("Z100 inventory = %+v", gpus)
	}
}

func TestParseHygonBWInventory(t *testing.T) {
	raw := "HCU[0]: Card Series: BW\nHCU[0]: PCI Bus: 0000:85:00.0 --> SN: fixture-serial\nHCU[0]: vram Total Memory (MiB): 65520\n"
	gpus := parseHygonInventory(raw, nil)
	if len(gpus) != 1 || gpus[0].Name != "BW" || gpus[0].ArchID != "BW" || gpus[0].PCIeBDF != "0000:85:00.0" || gpus[0].MemoryTotalBytes != 65520<<20 {
		t.Fatalf("BW inventory = %+v", gpus)
	}
}

func TestHygonMetricsUseRealMemoryCapture(t *testing.T) {
	p := &HygonProber{lastSeen: map[int]hygonIdentity{0: {uuid: "GPU-stable-zero"}}}
	metrics := p.parseMetrics(readHygonFixture(t), "")
	if len(metrics) != 2 {
		t.Fatalf("metrics = %+v", metrics)
	}
	if metrics[0].Vendor != "hygon" || metrics[0].Values["gpuworks_gpu_memory_total_bytes"] != 65520*1024*1024 {
		t.Fatalf("metric 0 = %+v", metrics[0])
	}
	if metrics[0].UUID != "GPU-stable-zero" {
		t.Fatalf("metric did not retain probe UUID: %+v", metrics[0])
	}
	if metrics[1].Values["gpuworks_gpu_memory_used_bytes"] != 58980*1024*1024 {
		t.Fatalf("metric 1 = %+v", metrics[1])
	}
}

func TestHygonMetricsFallBackToProvenMemoryQuery(t *testing.T) {
	var calls [][]string
	p := &HygonProber{command: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch strings.Join(args, " ") {
		case "--showuse --showmeminfo vram --showtemp --showpower":
			return "unsupported option", errors.New("exit status 2")
		case "--showmeminfo vram":
			return readHygonFixture(t), nil
		case "":
			return readHygonOverviewFixture(t), nil
		default:
			t.Fatalf("unexpected hy-smi arguments: %q", args)
			return "", nil
		}
	}}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 2 {
		t.Fatalf("Metrics() = %+v, %v", metrics, err)
	}
	wantFallback := [][]string{{"--showmeminfo", "vram"}, nil}
	if len(calls) != 3 || !reflect.DeepEqual(calls[1:], wantFallback) {
		t.Fatalf("calls = %+v, fallback want %+v", calls, wantFallback)
	}
}

func TestHygonRuntimeInjectionResolvesDRMFromPCI(t *testing.T) {
	p := testHygonProber(t, readHygonFixture(t))
	p.lastSeen = map[int]hygonIdentity{
		0: {uuid: "GPU-a", bdf: "0000:19:00.0"},
		1: {uuid: "GPU-b", bdf: "0000:67:00.0"},
	}
	inj := p.RuntimeInjection([]int{1})
	if inj.UnsupportedReason != "" {
		t.Fatalf("RuntimeInjection() failed: %s", inj.UnsupportedReason)
	}
	wantEnv := map[string]string{
		"HYGON_VISIBLE_DEVICES": "1",
		"HIP_VISIBLE_DEVICES":   "1",
	}
	if !reflect.DeepEqual(inj.Env, wantEnv) {
		t.Fatalf("env = %+v, want %+v", inj.Env, wantEnv)
	}
	wantArgs := []string{
		"--device", "/dev/kfd:/dev/kfd:rwm",
		"--device", "/dev/mkfd:/dev/mkfd:rwm",
		"--device", "/dev/dri/card2:/dev/dri/card2:rwm",
		"--device", "/dev/dri/renderD129:/dev/dri/renderD129:rwm",
		"--group-add", "39",
		"--group-add", "998",
	}
	if !reflect.DeepEqual(inj.DockerArgs, wantArgs) {
		t.Fatalf("docker args = %#v, want %#v", inj.DockerArgs, wantArgs)
	}
	wantMounts := []RuntimeMount{{Source: "/opt/hyhal", Target: "/opt/hyhal", ReadOnly: true}}
	if !reflect.DeepEqual(inj.Mounts, wantMounts) {
		t.Fatalf("driver library mounts = %#v, want %#v", inj.Mounts, wantMounts)
	}
}

// Docker, nerdctl and a Kubernetes pod hand the container only the selected
// cards' DRM nodes, so HIP numbers them 0..N-1 there; a host index past the first card would
// hide every card. runveil binds the whole host /dev and keeps host indexes.
func TestHygonRuntimeInjectionForDriverNumbersCardsAsHIPSeesThem(t *testing.T) {
	for _, tc := range []struct {
		driver  string
		indexes []int
		visible string
	}{
		{"docker", []int{1}, "0"},
		{"nerdctl", []int{1}, "0"},
		{"kube", []int{1}, "0"},
		{"udocker", []int{1}, "1"},
		{"docker", []int{1, 0}, "0,1"},
		{"kube", []int{1, 0}, "0,1"},
		{"nerdctl", []int{1, 0}, "0,1"},
		{"udocker", []int{1, 0}, "1,0"},
	} {
		p := testHygonProber(t, readHygonFixture(t))
		p.lastSeen = map[int]hygonIdentity{
			0: {uuid: "GPU-a", bdf: "0000:19:00.0"},
			1: {uuid: "GPU-b", bdf: "0000:67:00.0"},
		}
		inj := ResolveInjection(t.Context(), p, InjectionRequest{Indexes: tc.indexes, DriverName: tc.driver})
		if inj.UnsupportedReason != "" {
			t.Fatalf("%s %v: injection failed: %s", tc.driver, tc.indexes, inj.UnsupportedReason)
		}
		wantEnv := map[string]string{"HYGON_VISIBLE_DEVICES": tc.visible, "HIP_VISIBLE_DEVICES": tc.visible}
		if !reflect.DeepEqual(inj.Env, wantEnv) {
			t.Fatalf("%s %v: env = %+v, want %+v", tc.driver, tc.indexes, inj.Env, wantEnv)
		}
		// The selected cards' nodes are still the host's own, whatever HIP calls them.
		if !slices.Contains(inj.DockerArgs, "/dev/dri/renderD129:/dev/dri/renderD129:rwm") {
			t.Fatalf("%s %v: selected render node missing: %#v", tc.driver, tc.indexes, inj.DockerArgs)
		}
	}
}

func TestHygonRuntimeInjectionForDriverKeepsRefusals(t *testing.T) {
	p := testHygonProber(t, readHygonFixture(t))
	p.lastSeen = map[int]hygonIdentity{1: {uuid: "GPU-b", bdf: "0000:19:00.0"}}
	inj := p.RuntimeInjectionForDriver([]int{1}, "docker")
	if !strings.Contains(inj.UnsupportedReason, "changed from PCI") || inj.Env != nil || inj.DockerArgs != nil {
		t.Fatalf("refused injection = %+v", inj)
	}
}

func TestHygonRuntimeInjectionFailsClosedOnIndexDrift(t *testing.T) {
	p := testHygonProber(t, readHygonFixture(t))
	p.lastSeen = map[int]hygonIdentity{1: {uuid: "GPU-b", bdf: "0000:19:00.0"}}
	inj := p.RuntimeInjection([]int{1})
	if !strings.Contains(inj.UnsupportedReason, "changed from PCI") || len(inj.Env) != 0 || len(inj.DockerArgs) != 0 {
		t.Fatalf("injection = %+v", inj)
	}
}

func TestHygonRuntimeInjectionRequiresBothDRMNodes(t *testing.T) {
	p := testHygonProber(t, readHygonFixture(t))
	p.glob = func(pattern string) ([]string, error) {
		return []string{filepath.Join(filepath.Dir(pattern), "card2")}, nil
	}
	inj := p.RuntimeInjection([]int{1})
	if !strings.Contains(inj.UnsupportedReason, "both card and render DRM nodes") {
		t.Fatalf("injection = %+v", inj)
	}
}

func TestHygonTopologyFallsBackToPCIAndNUMA(t *testing.T) {
	p := testHygonProber(t, readHygonFixture(t))
	p.command = func(_ context.Context, args ...string) (string, error) {
		if len(args) == 1 && args[0] == "--showtopo" {
			return "", errors.New("unsupported")
		}
		return readHygonFixture(t), nil
	}
	got := p.Topology(t.Context())
	want := "HCU\tBDF\tNUMA\n0\t0000:19:00.0\t0\n1\t0000:67:00.0\t1\n"
	if got != want {
		t.Fatalf("Topology() = %q, want %q", got, want)
	}
}

func TestHygonDriverAndFrameworkVersions(t *testing.T) {
	p := &HygonProber{
		readFile: func(path string) ([]byte, error) {
			if path == "/sys/module/hycu/version" {
				return []byte("6.3.15-V1.0.1\n"), nil
			}
			return nil, os.ErrNotExist
		},
		command: func(_ context.Context, args ...string) (string, error) {
			return "DTK Version: 26.04.1\n", nil
		},
	}
	if got := p.DriverVersion(t.Context()); got != "6.3.15-V1.0.1" {
		t.Fatalf("driver = %q", got)
	}
	framework, version := p.Framework(t.Context())
	if framework != "dtk" || version != "26.04.1" {
		t.Fatalf("framework = %q/%q", framework, version)
	}
}

func TestHygonAvailableRequiresAParsedDevice(t *testing.T) {
	p := &HygonProber{
		lookPath: func(string) (string, error) { return "/opt/hyhal/bin/hy-smi", nil },
		command:  func(context.Context, ...string) (string, error) { return readHygonFixture(t), nil },
	}
	if !p.Available() {
		t.Fatal("real K100 capture should make Hygon prober available")
	}
	p.command = func(context.Context, ...string) (string, error) { return "End of SMI Log", nil }
	if p.Available() {
		t.Fatal("successful command without devices must not claim availability")
	}
}

// NVIDIA is tried after every other vendor CLI: a PPU host's CUDA-compat
// nvidia-smi answers too (see TestNewPrefersPPUOverCompatNVIDIASMI).
func TestDefaultProbersPreserveExistingVendorOrder(t *testing.T) {
	probers := defaultProbers()
	vendors := make([]string, len(probers))
	for i, prober := range probers {
		vendors[i] = prober.Vendor()
	}
	want := []string{"hygon", "ascend", "ppu", "kunlunxin", "nvidia", "apple", "amd"}
	if !reflect.DeepEqual(vendors, want) {
		t.Fatalf("default prober vendors = %v, want %v", vendors, want)
	}
}

func testHygonProber(t *testing.T, inventory string) *HygonProber {
	t.Helper()
	return &HygonProber{
		command: func(context.Context, ...string) (string, error) { return inventory, nil },
		readFile: func(path string) ([]byte, error) {
			switch {
			case strings.HasSuffix(path, "/numa_node") && strings.Contains(path, "19:00.0"):
				return []byte("0"), nil
			case strings.HasSuffix(path, "/numa_node") && strings.Contains(path, "67:00.0"):
				return []byte("1"), nil
			default:
				return nil, os.ErrNotExist
			}
		},
		glob: func(pattern string) ([]string, error) {
			root := strings.TrimSuffix(pattern, "*")
			if strings.Contains(pattern, "19:00.0") {
				return []string{filepath.Join(root, "card1"), filepath.Join(root, "renderD128")}, nil
			}
			if strings.Contains(pattern, "67:00.0") {
				return []string{filepath.Join(root, "renderD129"), filepath.Join(root, "card2")}, nil
			}
			return nil, nil
		},
		stat: func(path string) (os.FileInfo, error) {
			if path == "/opt/hyhal" {
				return hygonFakeFileInfo{name: "hyhal", dir: true}, nil
			}
			if path == "/dev/kfd" || path == "/dev/mkfd" || strings.HasPrefix(path, "/dev/dri/") {
				gid := uint32(998)
				if strings.HasPrefix(filepath.Base(path), "card") {
					gid = 39
				}
				return hygonFakeFileInfo{name: filepath.Base(path), mode: os.ModeDevice, gid: gid}, nil
			}
			return nil, os.ErrNotExist
		},
	}
}

func readHygonFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/hy-smi-k100ai.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type hygonFakeFileInfo struct {
	name string
	dir  bool
	mode os.FileMode
	gid  uint32
}

func (f hygonFakeFileInfo) Name() string       { return f.name }
func (f hygonFakeFileInfo) Size() int64        { return 0 }
func (f hygonFakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f hygonFakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f hygonFakeFileInfo) IsDir() bool        { return f.dir }
func (f hygonFakeFileInfo) Sys() any           { return &syscall.Stat_t{Gid: f.gid} }

func TestHygonQueriesResolvedCLIWhenAbsentFromPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hy-smi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'DCU[0]: Card Series: Z100\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	p := &HygonProber{lookPath: func(name string) (string, error) { return path, nil }}
	out, err := p.run(t.Context(), hygonInventoryArgs()...)
	if err != nil || !strings.Contains(out, "Card Series: Z100") {
		t.Fatalf("resolved query = %q, %v", out, err)
	}
}
