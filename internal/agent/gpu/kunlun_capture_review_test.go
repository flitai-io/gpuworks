package gpu

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

func TestKunlunCapturedEightCardInjection(t *testing.T) {
	for _, name := range []string{"kunlun-p800-node11-busy-20261028.xml", "kunlun-p800-node12-idle-20261028.xml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			var inv kunlunInventory
			if err := xml.Unmarshal(repairKunlunProcessInfoTagTypos(data), &inv); err != nil {
				t.Fatal(err)
			}
			if inv.Attached != 8 || len(inv.Cards) != 8 {
				t.Fatalf("attached=%d rows=%d", inv.Attached, len(inv.Cards))
			}
			sysfsName := strings.TrimSuffix(name, ".xml") + "-sysfs.json"
			sysfsRaw, err := os.ReadFile(filepath.Join("testdata", sysfsName))
			if err != nil {
				t.Fatal(err)
			}
			var sysfs struct {
				PCI map[string]string `json:"pci"`
				Dev map[string]uint64 `json:"dev"`
			}
			if err := json.Unmarshal(sysfsRaw, &sysfs); err != nil {
				t.Fatal(err)
			}
			mappings := sysfs.PCI
			modeRaw, err := os.ReadFile(filepath.Join("testdata", "kunlun-p800-"+strings.Split(name, "-")[2]+"-device-lstat-followup.json"))
			if err != nil {
				t.Fatal(err)
			}
			var modeCapture struct {
				Method  string `json:"method"`
				Devices map[string]struct {
					ModeOctal string `json:"mode_octal"`
					FileType  string `json:"file_type"`
					Rdev      uint64 `json:"rdev"`
				} `json:"devices"`
				Sysfs map[string]string `json:"sysfs"`
			}
			if err := json.Unmarshal(modeRaw, &modeCapture); err != nil {
				t.Fatal(err)
			}
			if modeCapture.Method != "os.lstat" {
				t.Fatalf("device fixture method = %q", modeCapture.Method)
			}
			capturedMode := func(path string) os.FileMode {
				entry, ok := modeCapture.Devices[path]
				if !ok || entry.FileType != "char" || entry.Rdev != sysfs.Dev[path] {
					t.Fatalf("device follow-up mismatch for %s: %+v", path, entry)
				}
				rawMode, err := strconv.ParseUint(strings.TrimPrefix(entry.ModeOctal, "0o"), 8, 32)
				if err != nil || rawMode&0170000 != 0020000 {
					t.Fatalf("captured mode is not char device: %s %s", path, entry.ModeOctal)
				}
				return os.ModeDevice | os.ModeCharDevice | os.FileMode(rawMode&0777)
			}
			if modeCapture.Sysfs["/sys/class/kunlun/xpuctrl/dev"] != mappings["/sys/class/kunlun/xpuctrl/dev"] {
				t.Fatal("control follow-up changed")
			}
			majors := map[int]int{}
			for _, c := range inv.Cards {
				n, err := strconv.Atoi(strings.TrimSpace(c.Minor))
				if err != nil || n < 0 || n >= 8 {
					t.Fatalf("minor %q", c.Minor)
				}
				bdf := accelerator.NormalizeBDF(strings.TrimSpace(c.BDF))
				base := "/sys/bus/pci/devices/" + bdf
				devMapping := mappings[base+"/kunlun/xpu"+strconv.Itoa(n)+"/dev"]
				parts := strings.Split(devMapping, ":")
				if len(parts) != 2 {
					t.Fatalf("captured card %d sysfs mapping missing", n)
				}
				major, majorErr := strconv.Atoi(parts[0])
				minor, minorErr := strconv.Atoi(parts[1])
				if majorErr != nil || minorErr != nil || minor != n || mappings[base+"/vendor"] != "0x2057" || mappings[base+"/device"] != "0x3686" {
					t.Fatalf("captured sysfs does not map card %d at %s: %q", n, bdf, devMapping)
				}
				majors[n] = major
				if sysfs.Dev[fmt.Sprintf("/dev/xpu%d", n)] != uint64(major<<8|n) {
					t.Fatalf("captured /dev/xpu%d rdev mismatch", n)
				}
			}
			control := mappings["/sys/class/kunlun/xpuctrl/dev"]
			controlParts := strings.Split(control, ":")
			if len(controlParts) != 2 {
				t.Fatalf("captured control sysfs missing: %q", control)
			}
			controlMajor, majorErr := strconv.Atoi(controlParts[0])
			controlMinor, minorErr := strconv.Atoi(controlParts[1])
			if majorErr != nil || minorErr != nil || controlMinor != 255 || sysfs.Dev["/dev/xpuctrl"] != uint64(controlMajor<<8|controlMinor) {
				t.Fatalf("captured control mapping inconsistent: %q rdev=%d", control, sysfs.Dev["/dev/xpuctrl"])
			}
			p := &KunlunProber{
				hostRoot: t.TempDir(),
				platform: func() accelerator.HostRuntime { return accelerator.HostRuntime{OS: "linux", Arch: "amd64"} },
				lookPath: func(string) (string, error) { return "/bin/xpu-smi", nil },
				command:  func(context.Context, ...string) (string, error) { return string(data), nil },
				readFile: func(path string) ([]byte, error) {
					if v, ok := mappings[path]; ok {
						return []byte(v), nil
					}
					return nil, os.ErrNotExist
				},
				stat: func(path string) (os.FileInfo, error) {
					if path == "/dev/xpuctrl" {
						return kunlunDeviceInfo{minor: controlMinor, major: controlMajor, mode: capturedMode(path)}, nil
					}
					if strings.HasPrefix(path, "/dev/xpu") {
						n, err := strconv.Atoi(strings.TrimPrefix(path, "/dev/xpu"))
						if err == nil && n >= 0 && n < 8 {
							return kunlunDeviceInfo{minor: n, major: majors[n], mode: capturedMode(path)}, nil
						}
					}
					return nil, os.ErrNotExist
				},
			}
			cards, err := p.Probe(t.Context())
			if err != nil || len(cards) != 8 {
				t.Fatalf("probe: %v %v", cards, err)
			}
			indexes := []int{0, 1, 2, 3, 4, 5, 6, 7}
			got := p.RuntimeInjectionForDriver(indexes, "docker")
			if got.UnsupportedReason != "" {
				t.Fatal(got.UnsupportedReason)
			}
			want := []string{"--runtime", "runc"}
			for _, n := range indexes {
				node := fmt.Sprintf("/dev/xpu%d", n)
				want = append(want, "--device", node+":"+node+":rwm")
			}
			want = append(want, "--device", "/dev/xpuctrl:/dev/xpuctrl:rwm")
			if !reflect.DeepEqual(got.DockerArgs, want) {
				t.Fatalf("args %v want %v", got.DockerArgs, want)
			}
		})
	}
}
