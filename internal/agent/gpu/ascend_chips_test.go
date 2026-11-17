package gpu

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The npu-smi fixtures are real output: the 910C tables were captured on the
// two 910C hosts behind the live demo server (artifacts/work/
// 2026-10-02-gpu-identity/evidence), the 910C table with Warning chips and
// the 910B4 table are the ones zstack-utility records in
// docs/hardware-tools/npu-smi-output.md.
func readNPUFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type chipWant struct {
	index    int
	npu      int
	chip     int
	bdf      string
	healthy  bool
	memMiB   int64
	archID   string
	healthSt string
}

func assertChips(t *testing.T, chips []ascendChip, want []chipWant) {
	t.Helper()
	if len(chips) != len(want) {
		t.Fatalf("chips = %d, want %d: %+v", len(chips), len(want), chips)
	}
	for i, w := range want {
		c := chips[i]
		g := c.gpu
		if g.Index != w.index || c.npu != w.npu || c.chip != w.chip {
			t.Errorf("chip %d address = index %d npu %d chip %d, want %d/%d/%d", i, g.Index, c.npu, c.chip, w.index, w.npu, w.chip)
		}
		if g.PCIeBDF != w.bdf || g.Healthy != w.healthy || g.HealthStatus != w.healthSt || g.MemoryTotalBytes != w.memMiB<<20 {
			t.Errorf("chip %d = bdf %q healthy %v status %q mem %d, want %q %v %q %d", i, g.PCIeBDF, g.Healthy, g.HealthStatus, g.MemoryTotalBytes, w.bdf, w.healthy, w.healthSt, w.memMiB<<20)
		}
		wantUUID := "ascend-" + strconv.Itoa(w.index)
		if g.ArchID != w.archID || g.Name != "Ascend "+w.archID || g.Vendor != "ascend" || g.UUID != wantUUID || g.NUMANode != -1 {
			t.Errorf("chip %d identity = %+v, want arch %s uuid %s", i, g, w.archID, wantUUID)
		}
	}
}

func TestParseAscendChips910CReportsEveryChip(t *testing.T) {
	chips := parseAscendChips(readNPUFixture(t, "npu-smi-info-910c-five-cards.txt"))
	bdfs := []string{"0000:9D:00.0", "0000:9F:00.0", "0000:99:00.0", "0000:9B:00.0", "0000:95:00.0", "0000:97:00.0", "0000:91:00.0", "0000:93:00.0", "0000:8D:00.0", "0000:8F:00.0"}
	var want []chipWant
	for phy, bdf := range bdfs {
		want = append(want, chipWant{index: phy, npu: phy / 2, chip: phy % 2, bdf: bdf, healthy: true, healthSt: "OK", memMiB: 65536, archID: "910C"})
	}
	assertChips(t, chips, want)
}

// NPUs 3 to 6 of this host are not listed; the Phy-IDs of NPU 7 are 14 and
// 15, which are the device numbers, not positions in the table.
func TestParseAscendChips910CUsesPhysicalIDsOnASparseHost(t *testing.T) {
	out := readNPUFixture(t, "npu-smi-info-910c-sparse.txt")
	chips := parseAscendChips(out)
	var indexes []int
	for _, c := range chips {
		indexes = append(indexes, c.gpu.Index)
	}
	if want := []int{0, 1, 2, 3, 4, 5, 14, 15}; !reflect.DeepEqual(indexes, want) {
		t.Fatalf("indexes = %v, want %v", indexes, want)
	}
	last := chips[len(chips)-1]
	if last.npu != 7 || last.chip != 1 || last.gpu.PCIeBDF != "0000:83:00.0" || last.gpu.ArchID != "910C" {
		t.Fatalf("last chip = %+v", last)
	}
	for index, want := range map[int][2]int{14: {7, 0}, 15: {7, 1}, 3: {1, 1}} {
		if npu, chip := ascendChipAddress(out, index); npu != want[0] || chip != want[1] {
			t.Errorf("address of %d = -i %d -c %d, want %v", index, npu, chip, want)
		}
	}
	// An index the table does not list is a one-chip card's NPU id.
	if npu, chip := ascendChipAddress(out, 9); npu != 9 || chip != 0 {
		t.Fatalf("address of unlisted 9 = %d/%d", npu, chip)
	}
}

func TestParseAscendChips910CKeepsEachChipsHealth(t *testing.T) {
	chips := parseAscendChips(readNPUFixture(t, "npu-smi-info-910c-warning.txt"))
	if len(chips) != 12 {
		t.Fatalf("chips = %d, want 12", len(chips))
	}
	byIndex := map[int]ascendChip{}
	for _, c := range chips {
		byIndex[c.gpu.Index] = c
	}
	// NPU 3 chip 1 and NPU 4 chip 0 are Warning; their siblings are not. A
	// Warning chip stays schedulable at a lower preference.
	for index, status := range map[int]string{6: "OK", 7: "Warning", 8: "Warning", 9: "OK", 2: "OK", 13: "OK"} {
		c, ok := byIndex[index]
		if !ok || c.gpu.HealthStatus != status || !c.gpu.Healthy {
			t.Errorf("chip %d = %v/%q (present %v), want schedulable %q", index, c.gpu.Healthy, c.gpu.HealthStatus, ok, status)
		}
	}
	if c := byIndex[7]; c.npu != 3 || c.chip != 1 || c.gpu.HealthStatus != "Warning" || c.gpu.PCIeBDF != "0000:93:00.0" {
		t.Fatalf("chip 7 = %+v", c)
	}
	if _, ok := byIndex[0]; ok {
		t.Fatal("NPU 0 is not on this host, yet a chip 0 was reported")
	}
}

func TestParseAscendChips910BOneChipPerCard(t *testing.T) {
	chips := parseAscendChips(readNPUFixture(t, "npu-smi-info-910b4.txt"))
	assertChips(t, chips, []chipWant{
		{index: 1, npu: 1, bdf: "0000:C2:00.0", healthy: true, healthSt: "OK", memMiB: 32768, archID: "910B4"},
		{index: 2, npu: 2, bdf: "0000:81:00.0", healthy: true, healthSt: "OK", memMiB: 32768, archID: "910B4"},
		{index: 5, npu: 5, bdf: "0000:02:00.0", healthy: true, healthSt: "OK", memMiB: 32768, archID: "910B4"},
		{index: 6, npu: 6, bdf: "0000:41:00.0", healthy: true, healthSt: "OK", memMiB: 32768, archID: "910B4"},
		{index: 7, npu: 7, bdf: "0000:42:00.0", healthy: true, healthSt: "OK", memMiB: 32768, archID: "910B4"},
	})
}

// The first-generation 910 is a one-chip card that names itself Ascend910
// too; without the multi-chip layout it is not a 910C.
func TestParseAscendChipsFirstGeneration910IsNot910C(t *testing.T) {
	const table = `
| NPU   Name                | Health        | Power(W)     Temp(C)          Hugepages-Usage(page)   |
| Chip                      | Bus-Id        | AICore(%)    Memory-Usage(MB) HBM-Usage(MB)           |
+===========================+===============+=========================================================+
| 0     Ascend910           | OK            | 70.1         40               0    / 0                |
| 0                         | 0000:C1:00.0  | 0            0    / 0         3105 / 32768            |
+===========================+===============+=========================================================+
| 1     Ascend910           | OK            | 71.2         41               0    / 0                |
| 0                         | 0000:81:00.0  | 0            0    / 0         2869 / 32768            |
+===========================+===============+=========================================================+
`
	chips := parseAscendChips(table)
	assertChips(t, chips, []chipWant{
		{index: 0, npu: 0, bdf: "0000:C1:00.0", healthy: true, healthSt: "OK", memMiB: 32768, archID: "910"},
		{index: 1, npu: 1, bdf: "0000:81:00.0", healthy: true, healthSt: "OK", memMiB: 32768, archID: "910"},
	})
}

// A card with two chips but no Phy-ID column cannot be mapped to devices;
// its chips are left out rather than given the NPU id twice.
func TestParseAscendChipsLeavesOutUnaddressableChips(t *testing.T) {
	const table = `
| NPU   Name                | Health        | Power(W)     Temp(C)          Hugepages-Usage(page)   |
| Chip                      | Bus-Id        | AICore(%)    Memory-Usage(MB) HBM-Usage(MB)           |
| 0     Ascend910           | OK            | 156.7        34               0    / 0                |
| 0                         | 0000:9D:00.0  | 0            0    / 0         2909 / 65536            |
| 0     Ascend910           | OK            | -            33               0    / 0                |
| 1                         | 0000:9F:00.0  | 0            0    / 0         2870 / 65536            |
| 2     910B4               | OK            | 82.2         31               0    / 0                |
| 0                         | 0000:81:00.0  | 0            0    / 0         2869 / 32768            |
`
	chips := parseAscendChips(table)
	if len(chips) != 1 || chips[0].gpu.Index != 2 || chips[0].gpu.ArchID != "910B4" {
		t.Fatalf("chips = %+v, want only the 910B4", chips)
	}
}

func TestParseAscendMetrics910CPerChip(t *testing.T) {
	metrics := parseAscendMetrics(readNPUFixture(t, "npu-smi-info-910c-five-cards.txt"))
	if len(metrics) != 10 {
		t.Fatalf("metrics = %d, want 10", len(metrics))
	}
	chip0, chip1 := metrics[0], metrics[1]
	if chip0.Index != 0 || chip0.UUID != "ascend-0" || chip1.Index != 1 || chip1.UUID != "ascend-1" {
		t.Fatalf("identities = %+v / %+v", chip0, chip1)
	}
	if chip0.Values["gpuworks_gpu_memory_used_bytes"] != 2909<<20 || chip1.Values["gpuworks_gpu_memory_used_bytes"] != 2870<<20 {
		t.Fatalf("memory used = %v / %v", chip0.Values, chip1.Values)
	}
	if chip0.Values["gpuworks_gpu_power_watts"] != 156.7 || chip0.Values["gpuworks_gpu_temperature_celsius"] != 34 {
		t.Fatalf("chip 0 telemetry = %+v", chip0.Values)
	}
	// npu-smi prints the card's power on chip 0 only.
	if _, ok := chip1.Values["gpuworks_gpu_power_watts"]; ok || chip1.Values["gpuworks_gpu_temperature_celsius"] != 33 {
		t.Fatalf("chip 1 telemetry = %+v", chip1.Values)
	}
}

func TestAscendProbeAsksAboutTheUnhealthyChip(t *testing.T) {
	table := readNPUFixture(t, "npu-smi-info-910c-warning.txt")
	var calls [][]string
	p := &AscendProber{infoTTL: time.Hour, runner: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 1 && args[0] == "info" {
			return table, nil
		}
		return "Health Status : Warning\nError Code : 80E18005\nError Information : link down\n", nil
	}}
	gpus, err := p.Probe(t.Context())
	if err != nil || len(gpus) != 12 {
		t.Fatalf("Probe = %d, %v", len(gpus), err)
	}
	want := [][]string{
		{"info"},
		{"info", "-t", "health", "-i", "3", "-c", "1"},
		{"info", "-t", "health", "-i", "4", "-c", "0"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("npu-smi calls = %v, want %v", calls, want)
	}
	for _, g := range gpus {
		flagged := g.Index == 7 || g.Index == 8
		if flagged != (g.HealthCode == "VENDOR_HEALTH_WARNING") {
			t.Errorf("gpu %d health code = %q", g.Index, g.HealthCode)
		}
	}
	if g := gpus[indexOfGPU(gpus, 7)]; g.HealthMessage != "NPU=3\nChip=1\nHealth=Warning\nHealth Status=Warning\nError Code=80E18005\nError Information=link down" {
		t.Errorf("gpu 7 evidence = %q", g.HealthMessage)
	}
}

// fakeNPUSMI puts an npu-smi on PATH that prints table for `npu-smi info`,
// answers board and device-share queries per chip, and logs every argv.
func fakeNPUSMI(t *testing.T, table string) (logPath string) {
	t.Helper()
	bin := t.TempDir()
	tablePath := filepath.Join(bin, "table.txt")
	logPath = filepath.Join(bin, "calls.log")
	state := filepath.Join(bin, "shared")
	if err := os.WriteFile(tablePath, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
if [ "$*" = "info" ]; then cat "` + tablePath + `"; exit 0; fi
case "$*" in
  "info -t board -i "*)
    printf '\tNPU ID : %s\n\tChip ID : %s\n\tVDie ID : 7D16F66C 00C0C9DF %s%s\n' "$5" "$7" "$5" "$7" ;;
  "info -t device-share -i "*)
    if [ -f "` + state + `-$5-$7" ]; then echo 'Device-share Status : True'; else echo 'Device-share Status : False'; fi ;;
  "set -t device-share -i "*)
    read answer; [ "$answer" = Y ] || exit 200; : > "` + state + `-$5-$7" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "npu-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func npuCalls(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestAscendChipQueriesAddressTheSecondChipOf910C(t *testing.T) {
	logPath := fakeNPUSMI(t, readNPUFixture(t, "npu-smi-info-910c-sparse.txt"))
	p := &AscendProber{}
	// Device 15 is chip 1 of NPU 7.
	if got := p.PartitionHardwareID(t.Context(), 15); got != "7D16F66C-00C0C9DF-71" {
		t.Fatalf("PartitionHardwareID(15) = %q", got)
	}
	changed, err := p.EnsureDeviceShare(t.Context(), 15)
	if err != nil || !changed {
		t.Fatalf("EnsureDeviceShare(15) = %v, %v", changed, err)
	}
	if changed, err := p.EnsureDeviceShare(t.Context(), 15); err != nil || changed {
		t.Fatalf("second EnsureDeviceShare(15) = %v, %v", changed, err)
	}
	want := []string{
		"info",
		"info -t board -i 7 -c 1",
		"info -t device-share -i 7 -c 1",
		"set -t device-share -i 7 -c 1 -d 1",
		"info -t device-share -i 7 -c 1",
		"info -t device-share -i 7 -c 1",
	}
	if got := npuCalls(t, logPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("npu-smi calls = %q, want %q", got, want)
	}
}

func TestAscendChipQueriesUseTheNPUOnOneChipCards(t *testing.T) {
	logPath := fakeNPUSMI(t, readNPUFixture(t, "npu-smi-info-910b4.txt"))
	p := &AscendProber{}
	if got := p.PartitionHardwareID(t.Context(), 5); got != "7D16F66C-00C0C9DF-50" {
		t.Fatalf("PartitionHardwareID(5) = %q", got)
	}
	if got := npuCalls(t, logPath); !reflect.DeepEqual(got, []string{"info", "info -t board -i 5 -c 0"}) {
		t.Fatalf("npu-smi calls = %q", got)
	}
}

// Device injection names devices by index, which on 910C is the Phy-ID:
// the same number as /dev/davinciN.
func TestAscendRuntimeInjectionUsesPhysicalIDsOn910C(t *testing.T) {
	gpus := parseAscendInfo(readNPUFixture(t, "npu-smi-info-910c-sparse.txt"))
	indexes := []int{gpus[6].Index, gpus[7].Index}
	inj := (&AscendProber{}).RuntimeInjectionForDriver(indexes, "nerdctl")
	if got := strings.Join(inj.DockerArgs[:4], " "); got != "--device /dev/davinci14 --device /dev/davinci15" {
		t.Fatalf("nerdctl devices = %q", got)
	}
	docker := (&AscendProber{}).RuntimeInjection(indexes)
	if docker.Env["ASCEND_VISIBLE_DEVICES"] != "14,15" || docker.Env["ASCEND_RT_VISIBLE_DEVICES"] != "0,1" {
		t.Fatalf("docker env = %+v", docker.Env)
	}
}

func indexOfGPU(gpus []proto.GPUInfo, index int) int {
	for i, g := range gpus {
		if g.Index == index {
			return i
		}
	}
	return -1
}
