package accelerator

import (
	"reflect"
	"strings"
	"testing"
)

func TestLevelAndScheduling(t *testing.T) {
	cases := map[string]struct {
		status      string
		schedulable bool
		degraded    bool
	}{
		"":                        {HealthOK, true, false},
		HealthMemoryErrorHistory:  {HealthOK, true, false},
		HealthMemoryRepairPending: {HealthWarning, true, true},
		HealthVendorWarning:       {HealthWarning, true, true},
		HealthMemoryUncorrectable: {HealthError, false, false},
		HealthMemoryRemapFailed:   {HealthError, false, false},
		HealthResetRequired:       {HealthError, false, false},
		HealthRebootRequired:      {HealthError, false, false},
		HealthPowerCycleRequired:  {HealthError, false, false},
		HealthVendorFault:         {HealthError, false, false},
		// A code from a newer agent this build does not know is never
		// silently scheduled.
		"some_future_code": {HealthError, false, false},
	}
	for code, want := range cases {
		status := HealthLevel(code)
		if status != want.status || HealthSchedulable(status) != want.schedulable || HealthDegraded(status) != want.degraded {
			t.Errorf("code %q: level %q schedulable %v degraded %v, want %+v", code, status, HealthSchedulable(status), HealthDegraded(status), want)
		}
	}
}

func TestMostSevere(t *testing.T) {
	if got := MostSevereHealth(); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := MostSevereHealth(HealthMemoryErrorHistory, HealthMemoryRepairPending, HealthMemoryUncorrectable); got != HealthMemoryUncorrectable {
		t.Fatalf("memory = %q", got)
	}
	if got := MostSevereHealth(HealthMemoryRemapFailed, HealthPowerCycleRequired, HealthResetRequired); got != HealthPowerCycleRequired {
		t.Fatalf("recovery = %q", got)
	}
}

func TestEvidenceRoundTrip(t *testing.T) {
	fields := []HealthField{
		{Name: "remapped_rows.pending", Value: "Yes"},
		{Name: "remapped_rows.uncorrectable", Value: "0"},
		{Name: "Error Information", Value: "node type=PORT, sensor type=Safety State,\nevent state=up to down"},
	}
	text := FormatHealthEvidence(fields)
	want := "remapped_rows.pending=Yes\nremapped_rows.uncorrectable=0\nError Information=node type=PORT, sensor type=Safety State, event state=up to down"
	if text != want {
		t.Fatalf("FormatHealthEvidence = %q", text)
	}
	got := ParseHealthEvidence(text)
	fields[2].Value = "node type=PORT, sensor type=Safety State, event state=up to down"
	if !reflect.DeepEqual(got, fields) {
		t.Fatalf("ParseHealthEvidence = %+v", got)
	}
	if HealthFieldValue(got, "remapped_rows.pending") != "Yes" || HealthFieldValue(got, "missing") != "" {
		t.Fatalf("HealthFieldValue lookup = %+v", got)
	}
}

// Rows written by agents that predate the evidence format hold a sentence;
// it must come back whole and unnamed rather than be split on its own "=".
func TestParseEvidenceKeepsLegacySentencesWhole(t *testing.T) {
	for _, legacy := range []string{
		"remapped rows pending",
		"uncorrected volatile DRAM ECC=1; remapped rows pending",
		"uncorrected volatile ECC errors: 3",
		"node type=PORT, sensor type=Safety State, event state=port link status change(up to down)",
	} {
		got := ParseHealthEvidence(legacy)
		if len(got) != 1 || got[0].Name != "" || got[0].Value != legacy {
			t.Errorf("ParseHealthEvidence(%q) = %+v", legacy, got)
		}
	}
	if got := ParseHealthEvidence("  "); got != nil {
		t.Errorf("blank = %+v", got)
	}
}

func TestGuideFor(t *testing.T) {
	const bdf = "0000:7e:00.0"
	pending, ok := HealthGuideFor(HealthCard{Vendor: "ppu", Code: HealthMemoryRepairPending, BDF: bdf}, nil)
	if !ok {
		t.Fatal("PPU pending has no guide")
	}
	want := HealthGuide{
		Probe:   "ppu-smi --query-ppu=pci.bus_id,ecc.errors.uncorrected.volatile.dram,ecc.errors.uncorrected.aggregate.dram,retired_pages.double_bit.count,retired_pages.pending,remapped_rows.uncorrectable,remapped_rows.pending,remapped_rows.failure --format=csv -i 0000:7e:00.0",
		Recheck: "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0",
		Actions: []string{"ppu-smi -r -i 0000:7e:00.0"},
	}
	if !reflect.DeepEqual(pending, want) {
		t.Fatalf("PPU pending guide = %+v", pending)
	}
	// A failed replacement and history have no command to run.
	for _, code := range []string{HealthMemoryRemapFailed, HealthMemoryErrorHistory} {
		if g, ok := HealthGuideFor(HealthCard{Vendor: "ppu", Code: code, BDF: bdf}, nil); !ok || len(g.Actions) != 0 || g.Recheck == "" {
			t.Errorf("PPU %s guide = %+v, %v", code, g, ok)
		}
	}
	reset, _ := HealthGuideFor(HealthCard{Vendor: "ppu", Code: HealthResetRequired, BDF: bdf}, nil)
	if reset.Probe != "ppu-smi --query-ppu=pci.bus_id,xid.correctable.ppu_reset,xid.correctable.os_reboot,xid.correctable.cold_reboot --format=csv -i 0000:7e:00.0" ||
		!reflect.DeepEqual(reset.Actions, []string{"ppu-smi -r -i 0000:7e:00.0"}) {
		t.Errorf("PPU reset guide = %+v", reset)
	}
	if reboot, _ := HealthGuideFor(HealthCard{Vendor: "ppu", Code: HealthRebootRequired, BDF: bdf}, nil); len(reboot.Actions) != 0 {
		t.Errorf("a host reboot is stated in prose, not as a command: %+v", reboot)
	}

	nv, ok := HealthGuideFor(HealthCard{Vendor: "nvidia", Code: HealthMemoryUncorrectable, BDF: "00000000:65:00.0"}, nil)
	if !ok || nv.Recheck != "nvidia-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 00000000:65:00.0" ||
		!reflect.DeepEqual(nv.Actions, []string{"nvidia-smi -r -i 00000000:65:00.0"}) ||
		nv.Probe != "nvidia-smi --query-gpu=pci.bus_id,ecc.errors.uncorrected.volatile.total,ecc.errors.uncorrected.aggregate.total,retired_pages.pending,remapped_rows.pending,remapped_rows.failure --format=csv -i 00000000:65:00.0" {
		t.Errorf("NVIDIA guide = %+v", nv)
	}

	ascend, ok := HealthGuideFor(HealthCard{Vendor: "ascend", Code: HealthVendorWarning, BDF: "0000:c2:00.0"}, []HealthField{{Name: "NPU", Value: "1"}, {Name: "Chip", Value: "0"}})
	if !ok || ascend.Probe != "npu-smi info" || ascend.Recheck != "npu-smi info -t health -i 1 -c 0" || len(ascend.Actions) != 0 {
		t.Errorf("Ascend guide = %+v", ascend)
	}
	if g, ok := HealthGuideFor(HealthCard{Vendor: "ascend", Code: HealthVendorWarning, BDF: "0000:c2:00.0"}, nil); !ok || g.Recheck != "" {
		t.Errorf("Ascend guide without a chip address = %+v", g)
	}

	for _, tc := range []struct {
		vendor, code, target string
		index                int
	}{
		{vendor: "ppu", code: "", target: bdf},
		// A card the vendor tool never enumerated has no index of its own.
		{vendor: "ppu", code: HealthMemoryRepairPending, index: -1},
		{vendor: "apple", code: HealthMemoryRepairPending, target: bdf},
		{vendor: "hygon", code: HealthResetRequired, target: bdf},
		{vendor: "ppu", code: "legacy_code", target: bdf},
	} {
		if g, ok := HealthGuideFor(HealthCard{Vendor: tc.vendor, Code: tc.code, BDF: tc.target, Index: tc.index}, nil); ok {
			t.Errorf("HealthGuideFor(%+v) = %+v, want none", tc, g)
		}
	}
	if HealthTarget("", 3) != "3" || HealthTarget(" 0000:7e:00.0 ", 3) != "0000:7e:00.0" || HealthTarget("", -1) != "" {
		t.Error("HealthTarget")
	}
}

func TestToolCommand(t *testing.T) {
	for vendor, want := range map[string]string{"nvidia": "nvidia-smi", "ppu": "ppu-smi", "ascend": "npu-smi info", "hygon": "hy-smi", "amd": "rocminfo"} {
		if got := ToolCommand(vendor); got != want {
			t.Errorf("ToolCommand(%q) = %q", vendor, got)
		}
	}
}

// The probe command queries the fields the agent recorded, so it reproduces
// exactly what the card was judged on; a row without structured evidence gets
// the whole group.
func TestHealthGuideProbeQueriesTheRecordedFields(t *testing.T) {
	g, _ := HealthGuideFor(HealthCard{Vendor: "ppu", Code: HealthMemoryRepairPending, BDF: "0000:7e:00.0"}, []HealthField{{Name: "remapped_rows.pending", Value: "Yes"}})
	if g.Probe != "ppu-smi --query-ppu=pci.bus_id,remapped_rows.pending --format=csv -i 0000:7e:00.0" {
		t.Fatalf("probe = %q", g.Probe)
	}
	g, _ = HealthGuideFor(HealthCard{Vendor: "nvidia", Code: HealthMemoryRemapFailed, BDF: "00000000:65:00.0"}, []HealthField{{Name: "remapped_rows.pending", Value: "Yes"}, {Name: "remapped_rows.failure", Value: "Yes"}})
	if g.Probe != "nvidia-smi --query-gpu=pci.bus_id,remapped_rows.pending,remapped_rows.failure --format=csv -i 00000000:65:00.0" {
		t.Fatalf("probe = %q", g.Probe)
	}
	g, _ = HealthGuideFor(HealthCard{Vendor: "ppu", Code: HealthMemoryRepairPending, BDF: "0000:7e:00.0"}, []HealthField{{Value: "remapped rows pending"}})
	if !strings.Contains(g.Probe, strings.Join(PPUHealthMemoryFields, ",")) {
		t.Fatalf("legacy probe = %q", g.Probe)
	}
}

func TestNVIDIARecoveryGuide(t *testing.T) {
	bdf := "0000:17:00.0"
	g, ok := HealthGuideFor(HealthCard{Vendor: "nvidia", Code: HealthResetRequired, BDF: bdf}, nil)
	if !ok || g.Probe != "nvidia-smi --query-gpu=pci.bus_id,gpu_recovery_action --format=csv -i "+bdf || len(g.Actions) != 1 || g.Actions[0] != "nvidia-smi -r -i "+bdf {
		t.Fatalf("reset guide = %+v ok=%v", g, ok)
	}
	g, ok = HealthGuideFor(HealthCard{Vendor: "nvidia", Code: HealthRebootRequired, BDF: bdf}, nil)
	if !ok || len(g.Actions) != 0 || !strings.Contains(g.Recheck, "xid") {
		t.Fatalf("reboot guide = %+v ok=%v", g, ok)
	}
}

func TestKunlunHealthGuideUsesVerifiedQuery(t *testing.T) {
	guide, ok := HealthGuideFor(HealthCard{Vendor: VendorKunlunxin, Code: HealthMemoryUncorrectable, BDF: "0000:07:00.0"}, nil)
	if !ok || guide.Probe != "xpu-smi -q -x" || guide.Recheck != "xpu-smi -q -x" || len(guide.Actions) != 0 {
		t.Fatalf("guide = %+v, %v", guide, ok)
	}
	if ToolCommand(VendorKunlunxin) != "xpu-smi -q -x" {
		t.Fatal("incorrect diagnostic command")
	}
}
