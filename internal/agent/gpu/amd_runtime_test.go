package gpu

import (
	"context"
	"os"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

type amdDeviceInfo struct {
	mode os.FileMode
	rdev uint64
}

func (i amdDeviceInfo) Name() string       { return "device" }
func (i amdDeviceInfo) Size() int64        { return 0 }
func (i amdDeviceInfo) Mode() os.FileMode  { return i.mode }
func (i amdDeviceInfo) ModTime() time.Time { return time.Time{} }
func (i amdDeviceInfo) IsDir() bool        { return false }
func (i amdDeviceInfo) Sys() any           { return amdFakeStat(i.rdev) }

func runtimeAMDFixture() *AMDProber {
	return &AMDProber{
		hostRoot:    "/nonexistent-amd-runtime-test-root",
		hostRuntime: func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} },
		readFile: func(path string) ([]byte, error) {
			values := map[string]string{
				"/sys/class/drm/card7/device/vendor":                   "0x1002",
				"/sys/class/drm/card7/device/mem_info_vram_total":      "17179869184",
				"/sys/class/drm/card7/device/uevent":                   "PCI_SLOT_NAME=0000:43:00.0",
				"/sys/class/drm/card7/device/unique_id":                "1234",
				"/sys/class/drm/card7/device/product_name":             "AMD MI210",
				"/sys/class/drm/card7/device/device":                   "0x740f",
				"/sys/class/drm/card7/device/drm/renderD135/dev":       "226:135",
				"/sys/class/drm/card8/device/vendor":                   "0x1002",
				"/sys/class/drm/card8/device/mem_info_vram_total":      "17179869184",
				"/sys/class/drm/card8/device/uevent":                   "PCI_SLOT_NAME=0000:44:00.0",
				"/sys/class/drm/card8/device/unique_id":                "5678",
				"/sys/class/drm/card8/device/product_name":             "AMD MI210",
				"/sys/class/drm/card8/device/device":                   "0x740f",
				"/sys/class/drm/card8/device/drm/renderD136/dev":       "226:136",
				"/sys/class/kfd/kfd/topology/nodes/1/properties":       "vendor_id 4098\ndevice_id 29711\ndomain 0\nlocation_id 17152\ndrm_render_minor 135\ngfx_target_version 90010\n",
				"/sys/class/kfd/kfd/topology/nodes/2/properties":       "vendor_id 4098\ndevice_id 29711\ndomain 0\nlocation_id 17408\ndrm_render_minor 136\ngfx_target_version 90010\n",
				"/sys/class/kfd/kfd/dev":                               "235:0",
				"/sys/bus/pci/devices/0000:43:00.0/drm/renderD135/dev": "226:135",
				"/sys/bus/pci/devices/0000:44:00.0/drm/renderD136/dev": "226:136",
			}
			if value, ok := values[path]; ok {
				return []byte(value), nil
			}
			if strings.HasPrefix(path, "/sys/class/drm/card") && strings.Contains(path, "/device/") {
				switch {
				case strings.HasSuffix(path, "/vendor"):
					return []byte("0x1002"), nil
				case strings.HasSuffix(path, "/mem_info_vram_total"):
					return []byte("17179869184"), nil
				case strings.HasSuffix(path, "/product_name"):
					return []byte("AMD MI210"), nil
				case strings.HasSuffix(path, "/uevent") && strings.Contains(path, "/card12/"):
					return []byte("PCI_SLOT_NAME=0000:45:00.0"), nil
				case strings.HasSuffix(path, "/unique_id") && strings.Contains(path, "/card12/"):
					return []byte("9012"), nil
				}
			}
			return nil, os.ErrNotExist
		},
		glob: func(pattern string) ([]string, error) {
			switch pattern {
			case "/sys/class/drm/card[0-9]*/device":
				return []string{"/sys/class/drm/card7/device", "/sys/class/drm/card8/device"}, nil
			case "/sys/class/drm/card7/device/drm/renderD*":
				return []string{"/sys/class/drm/card7/device/drm/renderD135"}, nil
			case "/sys/class/drm/card8/device/drm/renderD*":
				return []string{"/sys/class/drm/card8/device/drm/renderD136"}, nil
			case "/sys/class/kfd/kfd/topology/nodes/*/properties":
				return []string{"/sys/class/kfd/kfd/topology/nodes/1/properties", "/sys/class/kfd/kfd/topology/nodes/2/properties"}, nil
			case "/sys/bus/pci/devices/0000:43:00.0/drm/renderD*":
				return []string{"/sys/bus/pci/devices/0000:43:00.0/drm/renderD135"}, nil
			case "/sys/bus/pci/devices/0000:44:00.0/drm/renderD*":
				return []string{"/sys/bus/pci/devices/0000:44:00.0/drm/renderD136"}, nil
			}
			return nil, nil
		},
		stat: func(path string) (os.FileInfo, error) {
			switch path {
			case "/dev/kfd":
				return amdDeviceInfo{mode: os.ModeDevice | os.ModeCharDevice, rdev: uint64(60160)}, nil
			case "/dev/dri/renderD135":
				return amdDeviceInfo{mode: os.ModeDevice | os.ModeCharDevice, rdev: uint64(57991)}, nil
			case "/dev/dri/renderD136":
				return amdDeviceInfo{mode: os.ModeDevice | os.ModeCharDevice, rdev: uint64(57992)}, nil
			}
			return nil, os.ErrNotExist
		},
	}
}

func TestAMDRuntimeFactsDefaultToCurrentHost(t *testing.T) {
	facts := (&AMDProber{}).runtimeFacts()
	if facts.OS != goruntime.GOOS || facts.Arch != goruntime.GOARCH {
		t.Fatalf("default host facts = %s/%s, want %s/%s", facts.OS, facts.Arch, goruntime.GOOS, goruntime.GOARCH)
	}
}

func TestAMDRuntimeEmptySelectionFailsClosed(t *testing.T) {
	inj := (&AMDProber{}).RuntimeInjectionForDriver(nil, runtime.TargetNamed("docker"))
	if inj.UnsupportedReason == "" {
		t.Fatalf("empty selection injection was accepted: %+v", inj)
	}
}

func TestAMDRuntimeUsesSelectedRenderWithoutGuessingUserspaceOrdinals(t *testing.T) {
	p := runtimeAMDFixture()
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	inj := p.RuntimeInjectionForDriver([]int{7}, runtime.TargetNamed("docker"))
	if inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	if strings.Join(inj.ClearEnv, ",") != "NVIDIA_VISIBLE_DEVICES,CUDA_VISIBLE_DEVICES,HIP_VISIBLE_DEVICES,ROCR_VISIBLE_DEVICES,GPU_DEVICE_ORDINAL" ||
		!inj.Grant.Replaces.ToolkitRequest || !inj.Grant.Replaces.RuntimeClass {
		t.Fatalf("AMD selector cleanup = env %v replaces %+v", inj.ClearEnv, inj.Grant.Replaces)
	}
	wantPaths := []string{"/dev/dri", "/dev/dri/by-path", "/dev/dri/card7", "/dev/dri/card8", "/dev/dri/renderD135", "/dev/dri/renderD136", "/dev/kfd"}
	if !reflect.DeepEqual(inj.Grant.Replaces.Devices, wantPaths) {
		t.Fatalf("AMD device path cleanup = %v, want %v", inj.Grant.Replaces.Devices, wantPaths)
	}
	joined := strings.Join(inj.Grant.DockerDeviceArgs(), " ")
	if joined != "--device /dev/kfd:/dev/kfd:rwm --device /dev/dri/renderD135:/dev/dri/renderD135:rwm" {
		t.Fatal(joined)
	}
	if len(inj.Env) != 0 {
		t.Fatal(inj.Env)
	}
}

// runveil binds all of /dev and /sys, so the selection is pinned by ROCm's
// GPU UUID (GPU-<unique_id>, zero-padded to 16 hex digits as rocminfo
// prints it); docker keeps relying on the injected render node alone.
func TestAMDRuntimePinsRunveilSelectionByGPUUUID(t *testing.T) {
	p := runtimeAMDFixture()
	gs, err := p.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var uuid string
	for _, g := range gs {
		if g.Index == 7 {
			uuid = g.UUID
		}
	}
	id := normalizeAMDUniqueID(strings.TrimPrefix(uuid, "amd-"))
	if id == "" {
		t.Fatalf("fixture card 7 has no unique_id-backed UUID: %q", uuid)
	}
	inj := p.RuntimeInjectionForDriver([]int{7}, runtime.TargetNamed("udocker"))
	if inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	want := "GPU-" + strings.Repeat("0", 16-len(id)) + id
	if inj.Env["ROCR_VISIBLE_DEVICES"] != want {
		t.Fatalf("ROCR_VISIBLE_DEVICES = %q, want %q", inj.Env["ROCR_VISIBLE_DEVICES"], want)
	}
	if !strings.Contains(strings.Join(inj.Grant.DockerDeviceArgs(), " "), "/dev/kfd") {
		t.Fatalf("runveil injection lost the device nodes: %v", inj.Grant.DockerDeviceArgs())
	}
}

func TestAMDRuntimeCleanupIncludesByPathAliases(t *testing.T) {
	p := runtimeAMDFixture()
	originalGlob := p.glob
	const alias = "/dev/dri/by-path/pci-0000:43:00.0-render"
	p.glob = func(pattern string) ([]string, error) {
		if pattern == "/dev/dri/by-path/*" {
			return []string{alias}, nil
		}
		return originalGlob(pattern)
	}
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	injection := p.RuntimeInjectionForDriver([]int{7}, runtime.TargetNamed("docker"))
	if injection.UnsupportedReason != "" {
		t.Fatal(injection.UnsupportedReason)
	}
	want := []string{"/dev/dri", "/dev/dri/by-path", "/dev/dri/by-path/pci-0000:43:00.0-render", "/dev/dri/card7", "/dev/dri/card8", "/dev/dri/renderD135", "/dev/dri/renderD136", "/dev/kfd"}
	if !reflect.DeepEqual(injection.Grant.Replaces.Devices, want) {
		t.Fatalf("AMD by-path cleanup = %v, want %v", injection.Grant.Replaces.Devices, want)
	}
	if strings.Contains(strings.Join(injection.Grant.DockerDeviceArgs(), " "), "by-path") {
		t.Fatalf("injection must expose only the verified render node: %v", injection.Grant.DockerDeviceArgs())
	}
}

func TestAMDRuntimeFailsClosed(t *testing.T) {
	want := map[string]string{
		"driver": "host runtime is unsupported", "platform": "host runtime is unsupported",
		"missing": "device 9 absent from complete inventory", "duplicate": "duplicate device 7",
		"regular": "/dev/kfd unavailable: not a character device", "partition": "device 7 inventory is degraded",
		"drift": "device 7 PCI or GPU identity changed", "replacement": "device 7 PCI or GPU identity changed",
		"unpublished": "device 7 has no published healthy inventory identity", "rdev": "device 7 inventory is degraded",
		"kfd": "KFD sysfs identity unavailable",
	}
	for _, test := range []string{"driver", "platform", "missing", "duplicate", "regular", "partition", "drift", "replacement", "unpublished", "rdev", "kfd"} {
		t.Run(test, func(t *testing.T) {
			p := runtimeAMDFixture()
			// Every negative case starts from a demonstrably usable fixture;
			// otherwise a broken common setup could make all refusals pass.
			if _, err := p.Probe(context.Background()); err != nil {
				t.Fatal(err)
			}
			if baseline := p.RuntimeInjectionForDriver([]int{7}, runtime.TargetNamed("nerdctl")); baseline.UnsupportedReason != "" {
				t.Fatalf("invalid baseline fixture: %s", baseline.UnsupportedReason)
			}
			if test == "unpublished" {
				p = runtimeAMDFixture()
			}
			if test != "unpublished" {
				if _, err := p.Probe(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			indexes := []int{7}
			driver := "nerdctl"
			switch test {
			case "driver":
				driver = "host"
			case "platform":
				p.hostRuntime = func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "arm64"} }
			case "missing":
				indexes = []int{9}
			case "duplicate":
				indexes = []int{7, 7}
			case "regular":
				p.stat = func(string) (os.FileInfo, error) { return amdDeviceInfo{}, nil }
			case "rdev":
				p.stat = func(path string) (os.FileInfo, error) {
					if path == "/dev/dri/renderD135" {
						return amdDeviceInfo{mode: os.ModeDevice | os.ModeCharDevice, rdev: uint64(57992)}, nil
					}
					return runtimeAMDFixture().stat(path)
				}
			case "kfd":
				original := p.readFile
				p.readFile = func(path string) ([]byte, error) {
					if strings.HasSuffix(path, "/kfd/dev") {
						return nil, os.ErrNotExist
					}
					return original(path)
				}
			case "partition":
				original := p.glob
				p.glob = func(path string) ([]string, error) {
					if strings.Contains(path, "renderD") {
						return []string{"renderD135", "renderD136"}, nil
					}
					return original(path)
				}
			case "drift", "replacement":
				gs, e := p.Probe(context.Background())
				if e != nil {
					t.Fatal(e)
				}
				g := gs[0]
				if test == "drift" {
					g.PCIeBDF = "0000:44:00.0"
				} else {
					g.UUID = "amd-replaced"
				}
				p.lastSeen[7] = g
			}
			inj := p.RuntimeInjectionForDriver(indexes, runtime.TargetNamed(driver))
			if !strings.Contains(inj.UnsupportedReason, want[test]) {
				t.Fatalf("refusal = %q, want %q", inj.UnsupportedReason, want[test])
			}
			if len(inj.Env) != 0 || len(inj.Grant.DockerDeviceArgs()) != 0 || len(inj.Mounts) != 0 {
				t.Fatalf("failed injection returned additions: %+v", inj)
			}
		})
	}
}

func TestAMDRuntimeMultipleNonContiguousCards(t *testing.T) {
	p := runtimeAMDFixture()
	originalRead, originalGlob, originalStat := p.readFile, p.glob, p.stat
	p.readFile = func(path string) ([]byte, error) {
		if path == "/sys/class/kfd/kfd/topology/nodes/3/properties" {
			return []byte("vendor_id 4098\ndevice_id 29711\ndomain 0\nlocation_id 17664\ndrm_render_minor 141\ngfx_target_version 90010\n"), nil
		}
		if path == "/sys/class/drm/card12/device/device" {
			return []byte("0x740f"), nil
		}
		if strings.HasSuffix(path, "/renderD141/dev") {
			return []byte("226:141"), nil
		}
		if strings.Contains(path, "card12/") {
			if strings.HasSuffix(path, "/uevent") {
				return []byte("PCI_SLOT_NAME=0000:45:00.0"), nil
			}
			if strings.HasSuffix(path, "/unique_id") {
				return []byte("9012"), nil
			}
		}
		return originalRead(path)
	}
	p.glob = func(pattern string) ([]string, error) {
		if pattern == "/sys/class/kfd/kfd/topology/nodes/*/properties" {
			return []string{"/sys/class/kfd/kfd/topology/nodes/1/properties", "/sys/class/kfd/kfd/topology/nodes/2/properties", "/sys/class/kfd/kfd/topology/nodes/3/properties"}, nil
		}
		if pattern == "/sys/class/drm/card12/device/drm/renderD*" {
			return []string{"/sys/class/drm/card12/device/drm/renderD141"}, nil
		}
		if pattern == "/sys/class/drm/card[0-9]*/device" {
			return []string{"/sys/class/drm/card7/device", "/sys/class/drm/card12/device"}, nil
		}
		if pattern == "/sys/bus/pci/devices/0000:45:00.0/drm/renderD*" {
			return []string{"/sys/bus/pci/devices/0000:45:00.0/drm/renderD141"}, nil
		}
		return originalGlob(pattern)
	}
	p.stat = func(path string) (os.FileInfo, error) {
		if path == "/dev/dri/renderD141" {
			return amdDeviceInfo{mode: os.ModeDevice | os.ModeCharDevice, rdev: uint64(57997)}, nil
		}
		return originalStat(path)
	}
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	inj := p.RuntimeInjectionForDriver([]int{12, 7}, runtime.TargetNamed("nerdctl"))
	if inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
	if len(inj.Env) != 0 {
		t.Fatal(inj.Env)
	}
	joined := strings.Join(inj.Grant.DockerDeviceArgs(), " ")
	if !strings.Contains(joined, "renderD135") || !strings.Contains(joined, "renderD141") || strings.Contains(joined, "--gpus") {
		t.Fatal(joined)
	}
}

func TestAMDRuntimeIdentityFailurePersistsAcrossRetriesAndTelemetry(t *testing.T) {
	p := runtimeAMDFixture()
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := p.readFile
	p.readFile = func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "/unique_id") {
			return []byte("replacement"), nil
		}
		return original(path)
	}
	for attempt := 0; attempt < 2; attempt++ {
		p.Available()
		p.Metrics(context.Background())
		p.Topology(context.Background())
		if inj := p.RuntimeInjectionForDriver([]int{7}, runtime.TargetNamed("docker")); inj.UnsupportedReason == "" {
			t.Fatalf("attempt %d accepted replacement before publishing new inventory", attempt)
		}
	}
	// Publishing the new inventory explicitly establishes the new baseline.
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inj := p.RuntimeInjectionForDriver([]int{7}, runtime.TargetNamed("docker")); inj.UnsupportedReason != "" {
		t.Fatal(inj.UnsupportedReason)
	}
}

func TestAMDRuntimeRejectsDuplicatePhysicalIdentity(t *testing.T) {
	p := runtimeAMDFixture()
	originalRead, originalGlob := p.readFile, p.glob
	p.readFile = func(path string) ([]byte, error) {
		if strings.Contains(path, "card12/") && strings.HasSuffix(path, "/unique_id") {
			return []byte("1234"), nil
		}
		if strings.Contains(path, "card12/") && strings.HasSuffix(path, "/mem_info_vram_total") {
			return []byte("17179869184"), nil
		}
		return originalRead(path)
	}
	p.glob = func(pattern string) ([]string, error) {
		if pattern == "/sys/class/drm/card[0-9]*/device" {
			return []string{"/sys/class/drm/card7/device", "/sys/class/drm/card12/device"}, nil
		}
		return originalGlob(pattern)
	}
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inj := p.RuntimeInjectionForDriver([]int{7}, runtime.TargetNamed("docker")); inj.UnsupportedReason == "" {
		t.Fatalf("duplicate physical identity authorized injection: %+v", inj)
	}
}
