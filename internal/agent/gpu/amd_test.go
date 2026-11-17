package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type amdTestDeviceInfo struct{ rdev uint64 }

func (amdTestDeviceInfo) Name() string       { return "kfd" }
func (amdTestDeviceInfo) Size() int64        { return 0 }
func (amdTestDeviceInfo) Mode() os.FileMode  { return os.ModeDevice | os.ModeCharDevice }
func (amdTestDeviceInfo) ModTime() time.Time { return time.Time{} }
func (amdTestDeviceInfo) IsDir() bool        { return false }
func (d amdTestDeviceInfo) Sys() any         { return amdFakeStat(d.rdev) }

func amdFakeStat(rdev uint64) any {
	stat := &syscall.Stat_t{}
	field := reflect.ValueOf(stat).Elem().FieldByName("Rdev")
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		field.SetInt(int64(rdev))
	default:
		field.SetUint(rdev)
	}
	return stat
}

func testAMD(files map[string]string) *AMDProber {
	return &AMDProber{stat: func(path string) (os.FileInfo, error) {
		if path == "/dev/kfd" {
			return amdTestDeviceInfo{rdev: 60160}, nil
		}
		if path == "/dev/dri/renderD129" {
			return amdTestDeviceInfo{rdev: 57985}, nil
		}
		if path == "/dev/dri/renderD128" {
			return amdTestDeviceInfo{rdev: 57984}, nil
		}
		if path == "/dev/dri/renderD130" {
			return amdTestDeviceInfo{rdev: 57986}, nil
		}
		return nil, os.ErrNotExist
	}, readFile: func(path string) ([]byte, error) {
		if s, ok := files[path]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}, hostRoot: "/nonexistent-amd-test-root", glob: func(pattern string) ([]string, error) {
		if pattern == "/sys/class/drm/card[0-9]*/device" {
			return []string{"/sys/class/drm/card2/device", "/sys/class/drm/card4/device"}, nil
		}
		if pattern == "/sys/class/drm/card2/device/drm/renderD*" {
			return []string{"/sys/class/drm/card2/device/drm/renderD129"}, nil
		}
		if pattern == "/sys/class/drm/card4/device/drm/renderD*" {
			return []string{"/sys/class/drm/card4/device/drm/renderD130"}, nil
		}
		if pattern == "/sys/class/kfd/kfd/topology/nodes/*/properties" {
			paths := []string{"/sys/class/kfd/kfd/topology/nodes/1/properties"}
			if _, ok := files["/sys/class/kfd/kfd/topology/nodes/2/properties"]; ok {
				paths = append(paths, "/sys/class/kfd/kfd/topology/nodes/2/properties")
			}
			return paths, nil
		}
		if strings.Contains(pattern, "hwmon") {
			return []string{"/hw"}, nil
		}
		return nil, nil
	}}
}

func TestAMDProbeCancellationBoundsBlockedSysfsReaders(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	p := &AMDProber{glob: func(pattern string) ([]string, error) {
		if pattern == "/sys/class/drm/card[0-9]*/device" {
			entered <- struct{}{}
			<-release
		}
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := p.Probe(ctx); finished <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("sysfs fixture did not block")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Probe error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Probe did not honor cancellation")
	}
	second, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	_, err := p.Probe(second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Probe error = %v", err)
	}
	select {
	case <-entered:
		t.Fatal("second sysfs reader started while first is blocked")
	default:
	}
	close(release)
}
func amdFiles() map[string]string {
	p := "/sys/class/drm/card2/device/"
	return map[string]string{p + "vendor": "0x1002\n", p + "device": "0x740f", p + "drm/renderD129/dev": "226:129", p + "mem_info_vram_total": "68719476736\n", p + "uevent": "DRIVER=amdgpu\nPCI_SLOT_NAME=0000:43:00.0\n", p + "product_name": "AMD Instinct MI210", p + "unique_id": "1234", p + "numa_node": "1", p + "mem_info_vram_used": "1024", p + "gpu_busy_percent": "73", "/sys/class/drm/card4/device/vendor": "0x1d94", "/hw/temp1_input": "45000", "/hw/power1_average": "120500000", "/hw/freq1_input": "1500000000", "/opt/rocm/.info/version": "6.4.3", "/sys/class/kfd/kfd/dev": "235:0", "/sys/class/kfd/kfd/topology/nodes/1/properties": "vendor_id 4098\ndevice_id 29711\ndomain 0\nlocation_id 17152\ndrm_render_minor 129\ngfx_target_version 90010\n"}
}
func addAMDTestCard4Facts(files map[string]string) {
	files["/sys/class/drm/card4/device/device"] = "0x740f"
	files["/sys/class/drm/card4/device/drm/renderD130/dev"] = "226:130"
	files["/sys/class/kfd/kfd/topology/nodes/2/properties"] = "vendor_id 4098\ndevice_id 29711\ndomain 0\nlocation_id 17408\ndrm_render_minor 130\ngfx_target_version 90010\n"
}
func TestAMDInventoryAndMetrics(t *testing.T) {
	p := testAMD(amdFiles())
	gs, e := p.Probe(context.Background())
	if e != nil || len(gs) != 1 {
		t.Fatalf("inventory %v %v", gs, e)
	}
	g := gs[0]
	if g.Index != 2 || g.Vendor != "amd" || g.PCIeBDF != "0000:43:00.0" || g.MemoryTotalBytes != 68719476736 || g.NUMANode != 1 || g.UUID != "amd-1234" {
		t.Fatalf("GPU %+v", g)
	}
	ms, e := p.Metrics(context.Background())
	if e != nil || len(ms) != 1 {
		t.Fatalf("metrics %v %v", ms, e)
	}
	for key, want := range map[string]float64{"gpuworks_gpu_utilization": 73, "gpuworks_gpu_memory_used_bytes": 1024, "gpuworks_gpu_temperature_celsius": 45, "gpuworks_gpu_power_watts": 120.5, "gpuworks_gpu_sm_clock_mhz": 1500} {
		if ms[0].Values[key] != want {
			t.Fatalf("%s=%v", key, ms[0].Values[key])
		}
	}
	name, v := p.Framework(context.Background())
	if name != "rocm" || v != "6.4.3" {
		t.Fatal(name, v)
	}
}

func TestAMDMetricUnknownCapacityAndClockAreOmittedWhileZeroIsPreserved(t *testing.T) {
	const device = "/sys/class/drm/card2/device/"
	t.Run("unknown capacity and clock", func(t *testing.T) {
		files := amdFiles()
		files[device+"mem_info_vram_total"] = "0"
		files[device+"mem_info_vram_used"] = "0"
		files[device+"gpu_busy_percent"] = "0"
		files["/hw/freq1_input"] = "N/A"

		metrics, err := testAMD(files).Metrics(context.Background())
		if err != nil || len(metrics) != 1 {
			t.Fatalf("metrics = %v, err = %v", metrics, err)
		}
		for _, key := range []string{"gpuworks_gpu_memory_total_bytes", "gpuworks_gpu_memory_used_bytes", "gpuworks_gpu_sm_clock_mhz"} {
			if _, ok := metrics[0].Values[key]; ok {
				t.Fatalf("unknown %s was reported: %v", key, metrics[0].Values[key])
			}
		}
		if v, ok := metrics[0].Values["gpuworks_gpu_utilization"]; !ok || v != 0 {
			t.Fatalf("actual zero utilization was not preserved: %v, present=%t", v, ok)
		}
	})

	t.Run("valid capacity and zero readings", func(t *testing.T) {
		files := amdFiles()
		files[device+"mem_info_vram_used"] = "0"
		files[device+"gpu_busy_percent"] = "0"
		files["/hw/freq1_input"] = "0"
		p := testAMD(files)
		if _, err := p.Probe(context.Background()); err != nil {
			t.Fatal(err)
		}
		metrics, err := p.Metrics(context.Background())
		if err != nil || len(metrics) != 1 {
			t.Fatalf("metrics = %v, err = %v", metrics, err)
		}
		for key, want := range map[string]float64{
			"gpuworks_gpu_memory_total_bytes": 68719476736,
			"gpuworks_gpu_memory_used_bytes":  0,
			"gpuworks_gpu_utilization":        0,
			"gpuworks_gpu_sm_clock_mhz":       0,
		} {
			if got, ok := metrics[0].Values[key]; !ok || got != want {
				t.Fatalf("%s = %v, present=%t; want %v", key, got, ok, want)
			}
		}
	})
}

func TestNormalizeAMDUniqueIDRequiresNonzeroUint64Hex(t *testing.T) {
	for input, want := range map[string]string{
		"0x00001234":        "00001234",
		"ABCDEF":            "abcdef",
		"0":                 "",
		"000000":            "",
		"serial-123":        "",
		"10000000000000000": "",
		"+123":              "",
		"0x":                "",
		"":                  "",
	} {
		if got := normalizeAMDUniqueID(input); got != want {
			t.Errorf("normalizeAMDUniqueID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAMD395KeepsDriverCapacityAndSerialUUIDWithGFX1151Mapping(t *testing.T) {
	files := amdFiles()
	device := "/sys/class/drm/card2/device/"
	delete(files, device+"product_name")
	files[device+"device"] = "0x1586"
	files[device+"unique_id"] = "0x00001234"
	files[device+"uevent"] = "PCI_SLOT_NAME=0000:43:00.0"
	files[device+"mem_info_vram_total"] = "4294967296"
	p := testAMD(files)
	p.glob = func(pattern string) ([]string, error) {
		switch {
		case pattern == "/sys/class/drm/card[0-9]*/device":
			return []string{"/sys/class/drm/card2/device", "/sys/class/drm/card4/device"}, nil
		case strings.HasSuffix(pattern, "/drm/renderD*"):
			return []string{strings.TrimSuffix(pattern, "*") + "129"}, nil
		case pattern == "/sys/class/kfd/kfd/topology/nodes/*/properties":
			return []string{"/sys/class/kfd/kfd/topology/nodes/1/properties"}, nil
		default:
			return nil, nil
		}
	}
	files["/sys/class/kfd/kfd/topology/nodes/1/properties"] = "vendor_id 4098\ndevice_id 5510\ndomain 0\nlocation_id 17152\ndrm_render_minor 129\ngfx_target_version 110501\n"
	// readFile closes over the original map, which is intentionally mutable here.
	gs, err := p.Probe(context.Background())
	if err != nil || len(gs) != 1 {
		t.Fatalf("inventory = %v, %v", gs, err)
	}
	g := gs[0]
	if g.ArchID != "gfx1151" || g.Name != "AMD GPU 0x1586 (gfx1151)" || g.MemoryTotalBytes != 0 || g.Healthy || g.UUID != "amd-00001234" {
		t.Fatalf("gfx1151 identity/driver capacity = %+v", g)
	}
	files[device+"uevent"] = "PCI_SLOT_NAME=0000:44:00.0"
	gs, err = p.Probe(context.Background())
	if err != nil || len(gs) != 1 || gs[0].UUID != "amd-00001234" {
		t.Fatalf("UUID changed after PCI relocation = %v, %v", gs, err)
	}
}
func TestAMDDegradesIncompleteCardAndKeepsHealthyPeer(t *testing.T) {
	f := amdFiles()
	pfx := "/sys/class/drm/card2/device/"
	delete(f, pfx+"mem_info_vram_total")
	f["/sys/class/drm/card4/device/vendor"] = "0x1002"
	f["/sys/class/drm/card4/device/uevent"] = "PCI_SLOT_NAME=0000:44:00.0"
	f["/sys/class/drm/card4/device/mem_info_vram_total"] = "8589934592"
	addAMDTestCard4Facts(f)
	gs, err := testAMD(f).Probe(context.Background())
	if err != nil || len(gs) != 2 || gs[0].Healthy || !gs[1].Healthy {
		t.Fatalf("partial inventory = %v, %v", gs, err)
	}
	f = amdFiles()
	delete(f, pfx+"uevent")
	gs, err = testAMD(f).Probe(context.Background())
	if err != nil || len(gs) != 1 || gs[0].Healthy {
		t.Fatalf("missing BDF card should remain visible but degraded: %v %v", gs, err)
	}
	foreign := amdFiles()
	foreign["/sys/class/drm/card2/device/vendor"] = "0x1d94"
	gs, e := testAMD(foreign).Probe(context.Background())
	if e != nil || len(gs) != 0 {
		t.Fatalf("Hygon became AMD: %v %v", gs, e)
	}
}

func TestAMDInventoryRejectsMultipleRenderNodes(t *testing.T) {
	f := amdFiles()
	p := testAMD(f)
	original := p.glob
	p.glob = func(pattern string) ([]string, error) {
		if pattern == "/sys/class/drm/card2/device/drm/renderD*" {
			return []string{"/sys/class/drm/card2/device/drm/renderD129", "/sys/class/drm/card2/device/drm/renderD130"}, nil
		}
		return original(pattern)
	}
	gs, err := p.Probe(context.Background())
	if err != nil || len(gs) != 1 || gs[0].Healthy || gs[0].HealthCode != "DRIVER_INIT_FAILED" || !strings.Contains(gs[0].HealthMessage, "requires exactly one render node") {
		t.Fatalf("ambiguous render inventory = %+v, %v", gs, err)
	}
	if p.Available() {
		t.Fatal("ambiguous render card auto-selected")
	}
}

func TestAMDDuplicateIdentityDegradesEveryMatchingCard(t *testing.T) {
	f := amdFiles()
	f["/sys/class/drm/card4/device/vendor"] = "0x1002"
	f["/sys/class/drm/card4/device/uevent"] = "PCI_SLOT_NAME=0000:43:00.0"
	f["/sys/class/drm/card4/device/unique_id"] = "1234"
	f["/sys/class/drm/card4/device/mem_info_vram_total"] = "8589934592"
	gs, err := testAMD(f).Probe(context.Background())
	if err != nil || len(gs) != 2 || gs[0].Healthy || gs[1].Healthy ||
		gs[0].HealthCode != "RUNTIME_UNUSABLE" || gs[1].HealthCode != "RUNTIME_UNUSABLE" {
		t.Fatalf("duplicate identity was not rejected for all rows: %v %v", gs, err)
	}
}

func TestAMDHostRootUsesRealFilesystemOperations(t *testing.T) {
	root := t.TempDir()
	write := func(path, value string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	base := "/sys/class/drm/card2/device"
	write(base+"/vendor", "0x1002")
	write(base+"/uevent", "PCI_SLOT_NAME=0000:43:00.0")
	write(base+"/product_name", "AMD test device")
	write(base+"/device", "0x1586")
	write(base+"/unique_id", "0x1234")
	write(base+"/mem_info_vram_total", "4294967296")
	write(base+"/numa_node", "0")
	write("/sys/class/kfd/kfd/dev", "235:0")
	write("/sys/module/amdgpu/version", "test")
	// A regular file deliberately fails the character-device check while
	// proving Probe used the real os.Stat path rooted in this fixture.
	write("/dev/kfd", "not a device")
	p := &AMDProber{hostRoot: root}
	if p.Available() {
		t.Fatal("AMD compute backend must not be auto-selected without a usable KFD device")
	}
	gs, err := p.Probe(context.Background())
	if err != nil || len(gs) != 1 || gs[0].UUID != "amd-1234" || gs[0].MemoryTotalBytes != 4294967296 || gs[0].Healthy || gs[0].HealthCode != "DRIVER_INIT_FAILED" {
		t.Fatalf("real host-root probe = %v, %v", gs, err)
	}
}

func TestAMDMetricHwmonFailureKeepsPeerAndSkipsConflicts(t *testing.T) {
	f := amdFiles()
	f["/sys/class/drm/card4/device/vendor"] = "0x1002"
	f["/sys/class/drm/card4/device/uevent"] = "PCI_SLOT_NAME=0000:44:00.0"
	f["/sys/class/drm/card4/device/unique_id"] = "5678"
	f["/sys/class/drm/card4/device/mem_info_vram_total"] = "8589934592"
	p := testAMD(f)
	p.glob = func(pattern string) ([]string, error) {
		if strings.Contains(pattern, "hwmon") && strings.Contains(pattern, "card2") {
			return nil, os.ErrPermission
		}
		if strings.Contains(pattern, "hwmon") && strings.Contains(pattern, "card4") {
			return []string{"/hw4"}, nil
		}
		return []string{"/sys/class/drm/card2/device", "/sys/class/drm/card4/device"}, nil
	}
	f["/hw4/temp1_input"] = "51000"
	ms, err := p.Metrics(context.Background())
	if err != nil || len(ms) != 2 || ms[0].Values["gpuworks_gpu_memory_total_bytes"] == 0 ||
		ms[1].Values["gpuworks_gpu_temperature_celsius"] != 51 {
		t.Fatalf("one card's hwmon error discarded peer metrics: %v, %v", ms, err)
	}
	f["/sys/class/drm/card4/device/uevent"] = "PCI_SLOT_NAME=0000:43:00.0"
	f["/sys/class/drm/card4/device/unique_id"] = "1234"
	ms, err = p.Metrics(context.Background())
	if err != nil || len(ms) != 0 {
		t.Fatalf("ambiguous identities published metrics: %v, %v", ms, err)
	}
}

func TestAMDOptionalMetricsAndCancellation(t *testing.T) {
	f := amdFiles()
	delete(f, "/hw/temp1_input")
	f["/sys/class/drm/card2/device/gpu_busy_percent"] = "N/A"
	ms, e := testAMD(f).Metrics(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := ms[0].Values["gpuworks_gpu_utilization"]; ok {
		t.Fatal("invalid utilization reported as zero")
	}
	if _, ok := ms[0].Values["gpuworks_gpu_temperature_celsius"]; ok {
		t.Fatal("missing temperature reported")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := testAMD(f).Probe(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestAMDPermissionFailure(t *testing.T) {
	p := testAMD(nil)
	p.readFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
	if p.Available() {
		t.Fatal("permission denied available")
	}
	if _, e := p.Probe(context.Background()); !errors.Is(e, os.ErrPermission) {
		t.Fatal(e)
	}
}

func TestAMDDeviceInspectionRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "dev")
	if err := os.MkdirAll(dev, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dev, "target")
	if err := os.WriteFile(target, []byte("regular target"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dev, "kfd")); err != nil {
		t.Fatal(err)
	}
	p := &AMDProber{hostRoot: root}
	if err := p.requireCharacterDevice("/dev/kfd", amdDeviceNumber{major: 235, minor: 0}); err == nil {
		t.Fatal("device symlink was followed as a character device")
	}
}

func TestAMDGFXTargetEncoding(t *testing.T) {
	for v, want := range map[int64]string{90010: "gfx90a", 90402: "gfx942", 100300: "gfx1030", 110000: "gfx1100", 120001: "gfx1201", 0: "", 90016: ""} {
		if got := amdGFXTarget(v); got != want {
			t.Fatalf("%d = %s, want %s", v, got, want)
		}
	}
}
func TestAMDArchMatchesKFDRenderMinor(t *testing.T) {
	f := amdFiles()
	f["/sys/class/kfd/kfd/topology/nodes/0/properties"] = "vendor_id 4098\ndevice_id 29711\ndomain 0\nlocation_id 17152\ndrm_render_minor 130\ngfx_target_version 90010\n"
	f["/sys/class/kfd/kfd/topology/nodes/1/properties"] = "vendor_id 4098\ndevice_id 29711\ndomain 0\nlocation_id 17152\ndrm_render_minor 129\ngfx_target_version 90402\n"
	p := testAMD(f)
	orig := p.glob
	p.glob = func(pattern string) ([]string, error) {
		if strings.Contains(pattern, "drm/renderD") {
			return []string{"/sys/class/drm/card2/device/drm/renderD129"}, nil
		}
		if strings.Contains(pattern, "topology") {
			return []string{"/sys/class/kfd/kfd/topology/nodes/0/properties", "/sys/class/kfd/kfd/topology/nodes/1/properties"}, nil
		}
		return orig(pattern)
	}
	gs, e := p.Probe(context.Background())
	if e != nil || len(gs) != 1 || gs[0].ArchID != "gfx942" {
		t.Fatalf("architecture %v %v", gs, e)
	}
	f["/sys/class/kfd/kfd/topology/nodes/1/properties"] = "vendor_id 7572\ndevice_id 29711\ndomain 0\nlocation_id 17152\ndrm_render_minor 129\ngfx_target_version 90402\n"
	gs, e = p.Probe(context.Background())
	if e != nil || gs[0].ArchID != "" {
		t.Fatalf("foreign KFD node matched: %v %v", gs, e)
	}
}

func TestAMDFailedInventoryRetainsVendorWithoutSelectingUnusableCard(t *testing.T) {
	files := amdFiles()
	delete(files, "/sys/class/drm/card2/device/mem_info_vram_total")
	p := testAMD(files)
	gpus, err := p.Probe(context.Background())
	if err != nil || len(gpus) != 1 || gpus[0].Vendor != "amd" || gpus[0].Healthy || p.Available() {
		t.Fatalf("failed AMD inventory=%v %v", gpus, err)
	}
	files["/sys/class/drm/card2/device/vendor"] = "0x1d94"
	gpus, err = p.Probe(context.Background())
	if err != nil || len(gpus) != 0 || p.Available() {
		t.Fatalf("ROCm-compatible Hygon reported as AMD=%v %v", gpus, err)
	}
}
