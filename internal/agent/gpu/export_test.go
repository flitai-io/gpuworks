package gpu

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// Only compiled into the gpu test package. Integration tests can exercise the
// public Engine with a real prober without adding test setters to production.
func CapturedKunlunForTest(t *testing.T, missingXDR bool, cancelAtXDR func()) *KunlunProber {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	xml := read("kunlun-p800-node12-idle-20261028.xml")
	var fs struct {
		PCI map[string]string `json:"pci"`
	}
	if err := json.Unmarshal(read("kunlun-p800-node12-idle-20261028-sysfs.json"), &fs); err != nil {
		t.Fatal(err)
	}
	var dev struct {
		Devices map[string]struct {
			FileType string `json:"file_type"`
			Rdev     uint64 `json:"rdev"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(read("kunlun-p800-node12-device-lstat-followup.json"), &dev); err != nil {
		t.Fatal(err)
	}
	p := &KunlunProber{hostRoot: t.TempDir(), platform: func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} }, lookPath: func(string) (string, error) { return "/fixture/xpu-smi", nil }, command: func(context.Context, ...string) (string, error) { return string(xml), nil }, readFile: func(path string) ([]byte, error) {
		if value, ok := fs.PCI[path]; ok {
			return []byte(value), nil
		}
		return nil, os.ErrNotExist
	}, stat: func(path string) (os.FileInfo, error) {
		if path == "/dev/xdrdrv" {
			if missingXDR {
				return nil, os.ErrNotExist
			}
			if cancelAtXDR != nil {
				cancelAtXDR()
			}
		}
		d, ok := dev.Devices[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		if d.FileType != "char" {
			t.Fatalf("capture is not a character device: %s", path)
		}
		return kunlunDeviceInfo{minor: int(d.Rdev & 255), major: int(d.Rdev >> 8), mode: os.ModeDevice | os.ModeCharDevice}, nil
	}}
	cards, err := p.Probe(t.Context())
	if err != nil || len(cards) != 8 {
		t.Fatalf("capture=%+v %v", cards, err)
	}
	return p
}

func CapturedHygonForTest(t *testing.T) Prober { return testHygonProber(t, readHygonFixture(t)) }
func NativeAMDForTest() Prober {
	p := runtimeAMDFixture()
	_, _ = p.Probe(context.Background())
	return p
}

// PPU's existing injection validator has explicit inventory/device seams.
// Exercise that real validator, not a fake returning its input as output.
type ppuSelectionForTest struct{ PPUProber }

func (p *ppuSelectionForTest) RuntimeInjectionForDriver(indexes []int, driver string) RuntimeInjection {
	return p.runtimeInjection(indexes, "8, GPU-00000000-0000-0000-0000-000000000008", func(index int, id string) (string, error) {
		if index != 8 || !strings.HasSuffix(id, "0008") {
			return "", os.ErrNotExist
		}
		return "/dev/alixpu_ppu9", nil
	}, driver)
}
func NativePPUForTest() Prober { return &ppuSelectionForTest{} }
