package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func metaxFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Sanitized mx-smi inventory fixture with synthetic UUIDs. UUID-to-address
// relationships are retained to test stable cross-file identity matching.
func TestParseMetaXList(t *testing.T) {
	gpus := parseMetaXList(metaxFixture(t, "mx-smi-list.txt"))
	if len(gpus) != 8 {
		t.Fatalf("parsed %d cards, want 8: %+v", len(gpus), gpus)
	}
	first := gpus[0]
	if first.Index != 0 || first.Name != "MXC550" || first.PCIeBDF != "0000:0f:00.0" {
		t.Fatalf("card 0 = %+v", first)
	}
	if first.Vendor != accelerator.VendorMetaX {
		t.Fatalf("vendor = %q", first.Vendor)
	}
	if first.UUID != "GPU-00000000-0000-4000-8000-000000000001" {
		t.Fatalf("uuid = %q", first.UUID)
	}
	// "Available" is the driver having brought the card up: no finding.
	if first.HealthCode != "" {
		t.Fatalf("an available card carries finding %q", first.HealthCode)
	}
	// Indexes are the tool's, in order, and the last card is card 7.
	if last := gpus[7]; last.Index != 7 || last.PCIeBDF != "0000:d7:00.0" {
		t.Fatalf("card 7 = %+v", last)
	}
}

// A card the driver lists but cannot use is a runtime finding, not a silent
// healthy card, and the driver's own words are the evidence.
func TestParseMetaXListGradesUnavailableCard(t *testing.T) {
	out := "mx-smi  version: 3.100.0\n" +
		"GPU#0    MXC550      0000:0f:00.0   Unavailable (UUID: GPU-1, reason: ECC)\n"
	gpus := parseMetaXList(out)
	if len(gpus) != 1 {
		t.Fatalf("parsed %d cards", len(gpus))
	}
	if gpus[0].HealthCode != accelerator.HealthRuntimeUnusable {
		t.Fatalf("health code = %q, want %q", gpus[0].HealthCode, accelerator.HealthRuntimeUnusable)
	}
	if gpus[0].UUID != "GPU-1" {
		t.Fatalf("uuid = %q; the UUID is identity, not part of the state", gpus[0].UUID)
	}
}

// The CSV is addressed by column name: the column set changes with the flags
// given, and reading it by position silently reports another column's value.
func TestParseMetaXCSVReadsColumnsByName(t *testing.T) {
	metrics := parseMetaXCSV(metaxFixture(t, "mx-smi-metrics.csv"))
	if len(metrics) != 8 {
		t.Fatalf("parsed %d metric rows, want 8", len(metrics))
	}
	idle := metrics[0]
	if idle.Index != 0 || idle.Vendor != accelerator.VendorMetaX {
		t.Fatalf("row 0 = %+v", idle)
	}
	// 881084 KB used of 67108864 KB, as the CSV reports it; the platform's
	// metric is bytes.
	if got, want := idle.Values["gpuworks_gpu_memory_used_bytes"], float64(881084*1024); got != want {
		t.Fatalf("used = %v, want %v", got, want)
	}
	if got, want := idle.Values["gpuworks_gpu_memory_total_bytes"], float64(67108864*1024); got != want {
		t.Fatalf("total = %v, want %v", got, want)
	}
	if got := idle.Values["gpuworks_gpu_utilization"]; got != 0 {
		t.Fatalf("utilization = %v, want 0 on the idle card", got)
	}
	if _, ok := idle.Values["gpuworks_gpu_temperature_celsius"]; !ok {
		t.Fatal("temperature is missing")
	}
	// Card 4 held a container's workload at capture time: 55117924 KB.
	if got, want := metrics[4].Values["gpuworks_gpu_memory_used_bytes"], float64(55117924*1024); got != want {
		t.Fatalf("card 4 used = %v, want %v", got, want)
	}
}

// A column the tool did not report stays absent rather than reading as zero:
// "no answer" and "zero" are different facts about a card, and a card with
// nothing to report produces no metric at all rather than one full of zeros.
func TestParseMetaXCSVLeavesMissingColumnsUnknown(t *testing.T) {
	csv := "timestamp,deviceId,bdfId,utilization.vram.used [KB]\n" +
		"2026/11/20 11:00:00,GPU#0,0000:0f:00.0,N/A\n"
	if metrics := parseMetaXCSV(csv); len(metrics) != 0 {
		t.Fatalf("a row with no answer produced %+v", metrics)
	}

	// A reported column is kept and an absent one is not invented, even
	// when the absent one is the interesting one.
	csv = "timestamp,deviceId,utilization.GPU [%]\n" +
		"2026/11/20 11:00:00,GPU#0,0\n"
	metrics := parseMetaXCSV(csv)
	if len(metrics) != 1 {
		t.Fatalf("parsed %d rows", len(metrics))
	}
	if got := metrics[0].Values["gpuworks_gpu_utilization"]; got != 0 {
		t.Fatalf("utilization = %v, want the reported zero", got)
	}
	if _, ok := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; ok {
		t.Fatal("an absent column was read as a value")
	}
}

func TestParseMetaXVersions(t *testing.T) {
	version := metaxFixture(t, "mx-smi-version.txt")
	if got := parseMetaXDriverVersion(version); got != "3.100.0" {
		t.Fatalf("KMD = %q, want the kernel driver version", got)
	}
	if got := parseMetaXFrameworkVersion(version); got != "3.8.5.14" {
		t.Fatalf("MACA = %q", got)
	}
}

func TestParseMetaXSummaryReadsMemoryByAddress(t *testing.T) {
	cards := parseMetaXSummary(metaxFixture(t, "mx-smi-summary.txt"))
	// The same total is printed as vis_vram and vram; the card's own
	// accounting is the latter and it is what a scheduler compares against.
	if got := cards["0000:0f:00.0"].memoryTotalBytes; got != 67108864<<10 {
		t.Fatalf("card 0 memory = %d", got)
	}
	if _, ok := cards["0000:ff:00.0"]; ok {
		t.Fatal("an address the summary does not have produced a card")
	}
}

func TestParseMetaXProcessesReadsTheTextTable(t *testing.T) {
	procs, err := parseMetaXProcesses(metaxFixture(t, "mx-smi-process.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) == 0 {
		t.Fatal("no processes parsed from the table")
	}
	var found bool
	for _, p := range procs {
		if p.PID == 879033 {
			found = true
			if p.MemoryBytes != 31276<<20 || p.Index != 3 || !strings.Contains(p.Name, "sgl_diffusion") {
				t.Fatalf("pid 879033 = %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("pid 879033 missing from %+v", procs)
	}
	// The header rows and the banner are not processes.
	for _, p := range procs {
		if p.PID == 0 || strings.Contains(p.Name, "Process") {
			t.Fatalf("header parsed as a process: %+v", p)
		}
	}
}

func TestMetaXComputeProcessesInventorySafety(t *testing.T) {
	list := "GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\n"
	other := "GPU#0 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n"
	table := "| Process: |\n| GPU PID Process Name GPU Memory |\n| 0 123 worker 42 |\n+---+\nEnd of Log\n"
	tests := []struct {
		name, before, process, after string
		failAt                       int
		wantErr                      bool
		wantCount                    int
	}{
		{"mapped", list, table, list, 0, false, 1},
		{"empty", list, "| Process: |\n| GPU PID Process Name GPU Memory |\n+---+\nEnd of Log\n", list, 0, true, 0},
		{"explicit empty", list, "| Process: |\n| GPU PID Process Name GPU Memory |\n| no process found |\n+---+\nEnd of Log\n", list, 0, false, 0},
		{"empty before process", list, strings.Replace(table, "| 0 123", "| no process found |\n| 0 123", 1), list, 0, true, 0},
		{"empty after process", list, strings.Replace(table, "+---+", "| no process found |\n+---+", 1), list, 0, true, 0},
		{"duplicate empty", list, strings.Replace(table, "| 0 123 worker 42 |", "| no process found |\n| no process found |", 1), list, 0, true, 0},
		{"unmapped", list, strings.Replace(table, "| 0 123", "| 1 123", 1), list, 0, true, 0},
		{"renumbered", list, table, other, 0, true, 0},
		{"query failure", list, table, list, 2, true, 0},
		{"partial table", list, strings.TrimSuffix(table, "End of Log\n"), list, 0, true, 0},
		{"malformed row", list, strings.Replace(table, "worker 42", "worker invalid", 1), list, 0, true, 0},
		{"malformed index", list, strings.Replace(table, "| 0 123", "| invalid 123", 1), list, 0, true, 0},
		{"malformed index beside valid row", list, strings.Replace(table, "+---+", "| invalid 456 other 7 |\n+---+", 1), list, 0, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			p := &MetaXProber{run: func(_ context.Context, args ...string) (string, error) {
				calls++
				if calls == tt.failAt {
					return "partial", os.ErrDeadlineExceeded
				}
				switch calls {
				case 1:
					return tt.before, nil
				case 2:
					return tt.process, nil
				default:
					return tt.after, nil
				}
			}}
			procs, err := p.ComputeProcesses(t.Context())
			if (err != nil) != tt.wantErr || len(procs) != tt.wantCount {
				t.Fatalf("processes = %+v, error = %v", procs, err)
			}
			if len(procs) > 0 && procs[0].PCIeBDF != "0000:0f:00.0" {
				t.Fatalf("wrong address: %+v", procs[0])
			}
		})
	}
}

func TestMetaXCommandTimeoutAndPartialOutput(t *testing.T) {
	shortVendorCLITimeouts(t, 100*time.Millisecond, time.Second)
	dir := fakeCLI(t, "mx-smi", "echo 'GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)'; exec /bin/sleep 5\n")
	t.Setenv("PATH", dir)
	p := &MetaXProber{}
	if p.Available() {
		t.Fatal("timed-out partial output reported an available driver")
	}
	if got := p.RuntimeInjection([]int{0}); got.UnsupportedReason == "" {
		t.Fatal("timed-out inventory accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "mx-smi"), []byte("#!/bin/sh\necho 'GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if p.Available() {
		t.Fatal("failed command's partial output reported an available driver")
	}
	if _, err := p.ComputeProcesses(t.Context()); err == nil {
		t.Fatal("failed inventory reported empty occupancy")
	}
}

// A header whose address cannot be read ends the block before it: the values
// that follow are not the previous card's. Measured risk on a mangled or
// truncated mx-smi summary — card 0 would have absorbed card 1's memory.
func TestParseMetaXSummaryDropsInvalidDeviceBlock(t *testing.T) {
	cards := parseMetaXSummary(`GPU#0 MXC550 0000:0f:00.0
    vram total : 100 KB
GPU#1 MXC550 invalid
    vram total : 999 KB
GPU#2 MXC550 0000:48:00.0
    vram total : 200 KB
GPU#3
    vram total : 888 KB
`)
	if len(cards) != 2 || cards["0000:0f:00.0"].memoryTotalBytes != 100<<10 || cards["0000:48:00.0"].memoryTotalBytes != 200<<10 {
		t.Fatalf("a device block without an address corrupted card memory: %+v", cards)
	}
}
func TestIdentifyMetaXMetricsRequiresUniqueBDFAndUUID(t *testing.T) {
	csv := "deviceId,bdfId,utilization.GPU [%]\nGPU#0,0000:0f:00.0,12\nGPU#1,0000:34:00.0,42\n"
	gpus := parseMetaXList("GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n")

	missingUUID := append([]proto.GPUInfo(nil), gpus...)
	missingUUID[0].UUID = ""
	got := identifyMetaXMetrics(csv, missingUUID)
	if len(got) != 1 || got[0].Index != 1 || got[0].UUID != "GPU-b" {
		t.Fatalf("missing UUID should drop only its card's metric: %+v", got)
	}

	missingBDF := append([]proto.GPUInfo(nil), gpus...)
	missingBDF[0].PCIeBDF = ""
	got = identifyMetaXMetrics(csv, missingBDF)
	if len(got) != 1 || got[0].Index != 1 || got[0].UUID != "GPU-b" {
		t.Fatalf("missing inventory BDF should drop only its card's metric: %+v", got)
	}
}

// Metrics carry the inventory card's UUID, because that is what the server
// groups per-card statistics by: an unlabeled sample would split the card's
// history from its own inventory row.
func TestIdentifyMetaXMetricsCarriesTheInventoryUUID(t *testing.T) {
	csv := "deviceId,bdfId,utilization.GPU [%]\nGPU#1,0000:34:00.0,42\nGPU#0,0000:0f:00.0,12\n"
	gpus := parseMetaXList("GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n")
	got := identifyMetaXMetrics(csv, gpus)
	if len(got) != 2 || got[0].UUID != "GPU-a" || got[1].UUID != "GPU-b" {
		t.Fatalf("metrics = %+v", got)
	}
}

// A sample the output cannot be trusted to place is dropped rather than
// labeled with the wrong card: a missing sample leaves a gap, a mislabeled
// one leaves a card whose numbers belong to another.
func TestIdentifyMetaXMetricsDropsUnverifiableIdentities(t *testing.T) {
	csv := "deviceId,bdfId,utilization.GPU [%]\nGPU#1,0000:34:00.0,42\nGPU#0,0000:0f:00.0,12\n"
	gpus := parseMetaXList("GPU#0 MXC550 0000:0f:00.0 Available (UUID: GPU-a)\nGPU#1 MXC550 0000:34:00.0 Available (UUID: GPU-b)\n")

	reordered := append([]proto.GPUInfo(nil), gpus...)
	reordered[0].PCIeBDF, reordered[1].PCIeBDF = reordered[1].PCIeBDF, reordered[0].PCIeBDF
	if got := identifyMetaXMetrics(csv, reordered); len(got) != 0 {
		t.Fatalf("a renumbered inventory attributed stale samples: %+v", got)
	}

	duplicated := append([]proto.GPUInfo(nil), gpus...)
	duplicated[1].UUID = duplicated[0].UUID
	if got := identifyMetaXMetrics(csv, duplicated); len(got) != 0 {
		t.Fatalf("a UUID shared by two cards attributed samples: %+v", got)
	}

	anonymous := append([]proto.GPUInfo(nil), gpus...)
	anonymous[0].UUID = ""
	if got := identifyMetaXMetrics(csv, anonymous); len(got) != 1 || got[0].UUID != "GPU-b" {
		t.Fatalf("a card without a UUID attributed samples: %+v", got)
	}

	// Only the index the rows disagree about loses its sample; the other
	// card still agrees with the listing and keeps its own.
	conflicting := csv + "GPU#1,0000:48:00.0,99\n"
	if got := identifyMetaXMetrics(conflicting, gpus); len(got) != 1 || got[0].Index != 0 || got[0].UUID != "GPU-a" {
		t.Fatalf("rows disagreeing about one index attributed samples: %+v", got)
	}
}

// The metrics read asks the listing who each index is before it answers: the
// CSV alone cannot say, and a sample without the inventory's UUID would be
// filed under a series of its own on the server.
func TestMetaXMetricsAreIdentifiedAgainstAFreshInventory(t *testing.T) {
	const csv = "deviceId,bdfId,utilization.GPU [%]\nGPU#0,0000:0f:00.0,12\nGPU#1,0000:34:00.0,42\n"
	const list = "GPU#0    MXC550      0000:0f:00.0   Available (UUID: GPU-a)\n" +
		"GPU#1    MXC550      0000:34:00.0   Available (UUID: GPU-b)\n"
	asked := []string{}
	p := &MetaXProber{
		lookPath: func(string) (string, error) { return "/usr/bin/mx-smi", nil },
		run: func(_ context.Context, args ...string) (string, error) {
			asked = append(asked, args[0])
			if args[0] == "-L" {
				return list, nil
			}
			return "", nil
		},
		readFile: func(string) ([]byte, error) { return []byte(csv), nil },
	}
	metrics, err := p.Metrics(t.Context())
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if len(metrics) != 2 || metrics[0].UUID != "GPU-a" || metrics[1].UUID != "GPU-b" {
		t.Fatalf("metrics = %+v", metrics)
	}
	if len(asked) != 2 || asked[0] != "-o" || asked[1] != "-L" {
		t.Fatalf("mx-smi was asked: %v", asked)
	}
}

// A listing that cannot be read is not a reason to report unlabeled metrics:
// the caller keeps the last series it had rather than starting a new one.
func TestMetaXMetricsRefuseAFailedInventory(t *testing.T) {
	p := &MetaXProber{
		lookPath: func(string) (string, error) { return "/usr/bin/mx-smi", nil },
		run: func(_ context.Context, args ...string) (string, error) {
			if args[0] == "-L" {
				return "partial", os.ErrDeadlineExceeded
			}
			return "", nil
		},
		readFile: func(string) ([]byte, error) {
			return []byte("deviceId,bdfId,utilization.GPU [%]\nGPU#0,0000:0f:00.0,12\n"), nil
		},
	}
	if metrics, err := p.Metrics(t.Context()); err == nil || metrics != nil {
		t.Fatalf("metrics = %+v, %v", metrics, err)
	}
}

// Samples that agree with no card of the listing are a failure, not an empty
// answer: an empty answer reads as a node with no telemetry to report.
func TestMetaXMetricsRefuseSamplesTheirInventoryCannotPlace(t *testing.T) {
	p := &MetaXProber{
		lookPath: func(string) (string, error) { return "/usr/bin/mx-smi", nil },
		run: func(_ context.Context, args ...string) (string, error) {
			if args[0] == "-L" {
				return "GPU#0    MXC550      0000:34:00.0   Available (UUID: GPU-b)\n", nil
			}
			return "", nil
		},
		readFile: func(string) ([]byte, error) {
			return []byte("deviceId,bdfId,utilization.GPU [%]\nGPU#3,0000:0f:00.0,12\n"), nil
		},
	}
	if metrics, err := p.Metrics(t.Context()); err == nil || metrics != nil {
		t.Fatalf("metrics = %+v, %v", metrics, err)
	}
}
