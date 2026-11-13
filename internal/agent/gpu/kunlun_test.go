package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

type kunlunDeviceInfo struct {
	os.FileInfo
	minor int
	major int
	mode  os.FileMode
}

func (f kunlunDeviceInfo) Mode() os.FileMode { return f.mode }
func (f kunlunDeviceInfo) Sys() any {
	st := &syscall.Stat_t{}
	value := reflect.ValueOf(st).Elem().FieldByName("Rdev")
	if value.CanUint() {
		value.SetUint(uint64(f.major<<8 | f.minor))
	} else {
		value.SetInt(int64(f.major<<8 | f.minor))
	}
	return st
}

func kunlunFixture(t *testing.T) (*KunlunProber, *string) {
	t.Helper()
	b, e := os.ReadFile("testdata/kunlun-p800.xml")
	if e != nil {
		t.Fatal(e)
	}
	raw := string(b)
	p := &KunlunProber{hostRoot: t.TempDir(), platform: func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} }, lookPath: func(s string) (string, error) {
		if s != "xpu-smi" {
			t.Fatal(s)
		}
		return "/bin/xpu-smi", nil
	}, command: func(ctx context.Context, args ...string) (string, error) {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded CLI")
		}
		switch strings.Join(args, " ") {
		case "-q -x":
			return raw, nil
		case "topo -m":
			return "XPU0 XPU1\nXPU0 X PHB\n", nil
		default:
			return "", fmt.Errorf("unexpected xpu-smi arguments: %v", args)
		}
	}}
	p.readFile = func(path string) ([]byte, error) {
		switch path {
		case "/sys/bus/pci/devices/0000:07:00.0/numa_node", "/sys/bus/pci/devices/0000:09:00.0/numa_node":
			return []byte("-1"), nil
		case "/sys/bus/pci/devices/0000:07:00.0/vendor", "/sys/bus/pci/devices/0000:09:00.0/vendor":
			return []byte("0x2057"), nil
		case "/sys/bus/pci/devices/0000:07:00.0/kunlun/xpu0/dev":
			return []byte("195:0"), nil
		case "/sys/bus/pci/devices/0000:09:00.0/kunlun/xpu1/dev":
			return []byte("195:1"), nil
		case "/sys/class/kunlun/xpuctrl/dev":
			return []byte("195:255"), nil
		default:
			return nil, os.ErrNotExist
		}
	}
	p.stat = func(path string) (os.FileInfo, error) {
		minor := -1
		switch path {
		case "/dev/xpu0":
			minor = 0
		case "/dev/xpu1":
			minor = 1
		case "/dev/xpuctrl":
			minor = 255
		}
		if minor < 0 {
			return nil, os.ErrNotExist
		}
		return kunlunDeviceInfo{minor: minor, major: 195, mode: os.ModeDevice | os.ModeCharDevice}, nil
	}
	return p, &raw
}
func TestKunlunRealXMLInventoryAndMetrics(t *testing.T) {
	p, _ := kunlunFixture(t)
	if !p.Available() {
		t.Fatal("real XML not detected")
	}
	g, e := p.Probe(t.Context())
	if e != nil || len(g) != 2 {
		t.Fatal(g, e)
	}
	for i, c := range g {
		if c.Index != i || c.Vendor != accelerator.VendorKunlunxin || c.Name != "P800 PCIe" || c.ArchID != "kl3" || c.MemoryTotalBytes != 96<<30 || c.DriverVersion != "5.0.21.26" || !c.Healthy || c.NUMANode != -1 {
			t.Fatalf("wrong GPU %+v", c)
		}
	}
	if g[0].PCIeBDF != "0000:07:00.0" || g[1].PCIeBDF != "0000:09:00.0" {
		t.Fatal(g)
	}
	if p.DriverVersion(t.Context()) != "5.0.21.26" {
		t.Fatal("driver")
	}
	if name, v := p.Framework(t.Context()); name != "xre" || v != "" {
		t.Fatal("CUDA compatibility facade became SDK", name, v)
	}
	if !strings.Contains(p.Topology(t.Context()), "PHB") {
		t.Fatal("topology")
	}
	metrics, e := p.Metrics(t.Context())
	if e != nil || len(metrics) != 2 || metrics[0].Values["gpuworks_gpu_memory_used_bytes"] != 88632*(1<<20) || metrics[0].Values["gpuworks_gpu_temperature_celsius"] != 49 || metrics[0].Values["gpuworks_gpu_power_watts"] != 77 {
		t.Fatal(metrics, e)
	}
}
func TestKunlunRunningProcessXMLRepairsOnlyKnownTagTypos(t *testing.T) {
	fixture, err := os.ReadFile("testdata/kunlun-p800-running-process.xml")
	if err != nil {
		t.Fatal(err)
	}
	p, raw := kunlunFixture(t)
	*raw = string(fixture)

	gpus, err := p.Probe(t.Context())
	if err != nil || len(gpus) != 8 {
		t.Fatalf("busy inventory: got %d GPUs, err %v", len(gpus), err)
	}
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 8 || metrics[0].Values["gpuworks_gpu_memory_used_bytes"] != 95754*(1<<20) {
		t.Fatalf("busy GPU memory was lost: metrics=%+v err=%v", metrics, err)
	}
	processes, err := p.ComputeProcesses(t.Context())
	if err != nil || len(processes) != 15 || processes[0].PID != 52100 || processes[0].Name != "VLLM::Worker_TP4" || processes[0].PCIeBDF != "0000:15:00.0" || processes[0].MemoryBytes != 95754*(1<<20) {
		t.Fatalf("busy GPU process was lost: processes=%+v err=%v", processes, err)
	}
	inv, err := p.inventory(t.Context())
	if err != nil || len(inv.Cards[0].Processes) != 1 || inv.Cards[0].Processes[0].UsedMemory != "95754 MiB" {
		t.Fatalf("busy process memory was lost: inventory=%+v err=%v", inv, err)
	}

	// The compatibility repair is limited to the observed process tag typo.
	// An unrelated mismatched element must still fail strict XML decoding.
	*raw = strings.Replace(string(fixture), "<product_name>", "<unrelated><broken></unrelated><product_name>", 1)
	if _, err := p.Probe(t.Context()); err == nil {
		t.Fatal("unrelated malformed XML was accepted")
	}
}

func TestKunlunProbeFailureAndEmpty(t *testing.T) {
	for _, kind := range []string{"empty", "malformed", "zero", "partial", "duplicate", "duplicate-bdf", "bad-minor", "wrong-brand", "failed", "missing", "ECC", "missing-ECC", "invalid-ECC"} {
		t.Run(kind, func(t *testing.T) {
			p, raw := kunlunFixture(t)
			switch kind {
			case "empty":
				*raw = ""
			case "malformed":
				*raw = "not XML"
			case "zero":
				*raw = "<xpu_smi_log><attached_xpus>0</attached_xpus></xpu_smi_log>"
			case "partial":
				*raw = strings.Replace(*raw, "<attached_xpus>2", "<attached_xpus>3", 1)
			case "duplicate":
				*raw = strings.Replace(*raw, "<minor_number>1", "<minor_number>0", 1)
			case "duplicate-bdf":
				*raw = strings.Replace(*raw, "<pci_bus_id>00000000:09:00.0</pci_bus_id>", "<pci_bus_id>0000:07:00.0</pci_bus_id>", 1)
			case "bad-minor":
				*raw = strings.Replace(*raw, "<minor_number>0", "<minor_number>-1", 1)
			case "wrong-brand":
				*raw = strings.ReplaceAll(*raw, "KUNLUNXIN", "INTEL")
			case "failed":
				p.command = func(context.Context, ...string) (string, error) { return "", errors.New("driver failure") }
			case "missing":
				p.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
			case "ECC":
				*raw = strings.Replace(*raw, "<dram_uncorrectable>0", "<dram_uncorrectable>3", 1)
			case "missing-ECC":
				*raw = strings.Replace(*raw, "<dram_uncorrectable>0</dram_uncorrectable>", "", 1)
			case "invalid-ECC":
				*raw = strings.Replace(*raw, "<dram_uncorrectable>0", "<dram_uncorrectable>unknown", 1)
			}
			g, e := p.Probe(t.Context())
			switch kind {
			case "missing", "zero":
				if e != nil || len(g) != 0 || p.Available() {
					t.Fatal(g, e)
				}
			case "ECC":
				if e != nil || len(g) != 2 || g[0].Healthy || g[0].HealthCode != accelerator.HealthMemoryUncorrectable || g[0].HealthStatus != accelerator.HealthError {
					t.Fatal(g, e)
				}
			case "missing-ECC", "invalid-ECC":
				if e != nil || len(g) != 2 || !g[0].Healthy || g[0].HealthCode != accelerator.HealthTelemetryUnavailable || !g[1].Healthy || !p.Available() {
					t.Fatal("ECC warning should stay schedulable", g, e)
				}
			case "bad-minor":
				if e != nil || len(g) != 2 || g[0].Healthy || !g[1].Healthy || !p.Available() {
					t.Fatal("bad card erased healthy sibling", g, e)
				}
			case "partial":
				if e != nil || len(g) != 2 || !g[0].Healthy || !g[1].Healthy {
					t.Fatal("count mismatch erased returned cards", g, e)
				}
			case "duplicate", "duplicate-bdf", "wrong-brand":
				if e != nil || len(g) != 2 || g[0].Healthy || g[1].Healthy {
					t.Fatal("ambiguous identities were schedulable", g, e)
				}
			default:
				if e == nil || p.Available() {
					t.Fatal("bad inventory accepted", g, e)
				}
			}
		})
	}
}
func TestKunlunSelectedDevicesIsolation(t *testing.T) {
	for _, kind := range []string{"single", "multi", "empty", "duplicate", "absent", "renumbered", "other-model", "other-arch", "missing-device", "regular-file", "wrong-rdev", "wrong-sysfs", "wrong-vendor", "missing-control", "control-rdev", "udocker"} {
		t.Run(kind, func(t *testing.T) {
			p, raw := kunlunFixture(t)
			if _, e := p.Probe(t.Context()); e != nil {
				t.Fatal(e)
			}
			indexes := []int{1}
			driver := "docker"
			switch kind {
			case "multi":
				indexes = []int{1, 0}
			case "empty":
				indexes = nil
			case "duplicate":
				indexes = []int{1, 1}
			case "absent":
				indexes = []int{9}
			case "renumbered":
				*raw = strings.Replace(*raw, "GPU-fixture-2", "GPU-other", 1)
			case "other-model":
				*raw = strings.Replace(*raw, "<product_name>P800 PCIe</product_name>", "<product_name>Other Kunlun</product_name>", 1)
				indexes = []int{0}
			case "other-arch":
				*raw = strings.Replace(*raw, "<product_architecture>KL3</product_architecture>", "<product_architecture>KL2</product_architecture>", 1)
				indexes = []int{0}
			case "udocker":
				driver = "udocker"
			}
			originalStat := p.stat
			p.stat = func(path string) (os.FileInfo, error) {
				if kind == "missing-device" && path == "/dev/xpu1" || kind == "missing-control" && path == "/dev/xpuctrl" {
					return nil, os.ErrNotExist
				}
				s, e := originalStat(path)
				if e != nil {
					return s, e
				}
				d := s.(kunlunDeviceInfo)
				if kind == "regular-file" {
					d.mode = 0600
				}
				if kind == "wrong-rdev" || kind == "control-rdev" && path == "/dev/xpuctrl" {
					d.minor = 4
				}
				return d, nil
			}
			originalRead := p.readFile
			p.readFile = func(path string) ([]byte, error) {
				if kind == "wrong-sysfs" && strings.HasSuffix(path, "/xpu1/dev") {
					return []byte("195:0"), nil
				}
				if kind == "wrong-vendor" && strings.HasSuffix(path, "/vendor") {
					return []byte("0x8086"), nil
				}
				return originalRead(path)
			}
			got := p.RuntimeInjectionForDriver(indexes, driver)
			pass := kind == "single" || kind == "multi" || kind == "empty"
			if !pass {
				wantCause := map[string]string{
					"duplicate":       "invalid or duplicate Kunlun device selection",
					"absent":          "invalid or duplicate Kunlun device selection",
					"renumbered":      "Kunlun device identity changed since probe",
					"other-model":     "Kunlun device is not a supported P800 PCIe/KL3",
					"other-arch":      "Kunlun device is not a supported P800 PCIe/KL3",
					"missing-device":  "Kunlun character device unavailable: /dev/xpu1",
					"regular-file":    "Kunlun character device unavailable: /dev/xpu1",
					"wrong-rdev":      "Kunlun character device major/minor mismatch",
					"wrong-sysfs":     "Kunlun sysfs minor mismatch",
					"wrong-vendor":    "Kunlun PCI vendor cannot be verified",
					"missing-control": "Kunlun control device unavailable",
					"control-rdev":    "Kunlun character device major/minor mismatch",
					"udocker":         "runtime_unsupported:",
				}[kind]
				if !strings.Contains(got.UnsupportedReason, wantCause) {
					t.Fatalf("%s cause = %q, want %q", kind, got.UnsupportedReason, wantCause)
				}
				if got.UnsupportedReason == "" || len(got.DockerArgs) != 0 || len(got.Env) != 0 {
					t.Fatalf("unsafe injection %+v", got)
				}
				return
			}
			if got.UnsupportedReason != "" {
				t.Fatal(got)
			}
			if kind == "empty" {
				if len(got.Env) != 0 || len(got.DockerArgs) != 0 {
					t.Fatal(got)
				}
				return
			}

			want := []string{"--runtime", "runc", "--device", "/dev/xpu1:/dev/xpu1:rwm"}
			wantEnv := map[string]string{"XPU_VISIBLE_DEVICES": "0"}
			if kind == "multi" {
				want = append(want, "--device", "/dev/xpu0:/dev/xpu0:rwm")
				wantEnv["XPU_VISIBLE_DEVICES"] = "0,1"
			}
			want = append(want, "--device", "/dev/xpuctrl:/dev/xpuctrl:rwm")
			if !reflect.DeepEqual(got.DockerArgs, want) || !reflect.DeepEqual(got.Env, wantEnv) {
				t.Fatal(got, want, wantEnv)
			}
		})
	}
}
func TestKunlunCommandTimeout(t *testing.T) {
	// Read-only queries outlive a caller cancellation. Bound and reap the
	// background fixture before another test reuses the xpu-smi binary key.
	queryTimings(t, time.Second, 200*time.Millisecond, time.Minute)
	script := filepath.Join(t.TempDir(), "xpu-smi")
	if e := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); e != nil {
		t.Fatal(e)
	}
	p := &KunlunProber{hostRoot: t.TempDir(), platform: func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} }, lookPath: func(string) (string, error) { return script, nil }}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, e := p.Probe(ctx)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}

func TestKunlunInjectionChecksFreshCLIWhenQueryCacheIsStale(t *testing.T) {
	queryTimings(t, time.Second, 2*time.Second, time.Minute)
	p, raw := kunlunFixture(t)
	script := filepath.Join(t.TempDir(), "xpu-smi")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$1\" = -q ] && [ \"$2\" = -x ] || exit 3\ncat \"$0.xml\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeInventory := func(inventory string) {
		t.Helper()
		if err := os.WriteFile(script+".xml", []byte(inventory), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeInventory(*raw)
	p.command = nil // Exercise the real QueryVendorCLI and RunVendorCLI paths.
	p.lookPath = func(string) (string, error) { return script, nil }
	if cards, err := p.Probe(t.Context()); err != nil || len(cards) != 2 {
		t.Fatalf("initial CLI probe: %v, %v", cards, err)
	}
	writeInventory(strings.Replace(*raw, "GPU-fixture-2", "GPU-replaced", 1))

	// Simulate a slow driver: read-only queries may serve the previous answer.
	// Injection must bypass this cache and observe the replaced device identity.
	vendorQueries.Lock()
	vendorQueries.binaries[binaryKey(script)].lastTook = vendorCLITimeout + time.Second
	vendorQueries.Unlock()
	if cached, err := p.inventory(t.Context()); err != nil || cached.Cards[1].UUID != "GPU-fixture-2" {
		t.Fatalf("query cache was not stale: %+v, %v", cached, err)
	}
	if got := p.RuntimeInjectionForDriver([]int{1}, "docker"); !strings.Contains(got.UnsupportedReason, "identity changed") || len(got.DockerArgs) != 0 {
		t.Fatalf("stale query authorized injection: %+v", got)
	}
}

func TestKunlunInvalidUsedMemoryIsOmitted(t *testing.T) {
	p, raw := kunlunFixture(t)
	*raw = strings.Replace(*raw, "<used>88632 MiB</used>", "<used>999999 MiB</used>", 1)
	metrics, err := p.Metrics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; exists {
		t.Fatal("used memory above total was reported")
	}
}

func TestKunlunXMLIgnoresStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	xml, err := os.ReadFile("testdata/kunlun-p800.xml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "xpu-smi")
	script := "#!/bin/sh\nprintf '%s\\n' 'error & <not xml>' >&2\ncat " + filepath.Join(dir, "output.xml") + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "output.xml"), xml, 0600); err != nil {
		t.Fatal(err)
	}
	p := &KunlunProber{lookPath: func(string) (string, error) { return path, nil }}
	for _, fresh := range []bool{false, true} {
		inv, err := p.inventoryWithFreshness(t.Context(), fresh)
		if err != nil || len(inv.Cards) != 2 {
			t.Fatalf("fresh=%v: %d cards, %v", fresh, len(inv.Cards), err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "output.xml"), []byte("<broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.inventoryWithFreshness(t.Context(), true); err == nil {
		t.Fatal("broken XML accepted")
	}
}

func TestKunlunValueUnits(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
	}{
		{"1024MiB", 1024}, {"1 GiB", 1024}, {"1024MB", 1024 * 1000000.0 / (1 << 20)}, {"1048576 KiB", 1024}, {"1 gib", 1024},
	} {
		got, ok := kunlunValue(tc.raw, "MiB")
		if !ok || got != tc.want {
			t.Errorf("%q: got %v, %v; want %v", tc.raw, got, ok, tc.want)
		}
	}
	for _, raw := range []string{"-1 MiB", "NaN MiB", "1e1000 MiB", "1 TiB", "1 MiB junk"} {
		if _, ok := kunlunValue(raw, "MiB"); ok {
			t.Errorf("accepted %q", raw)
		}
	}
}
