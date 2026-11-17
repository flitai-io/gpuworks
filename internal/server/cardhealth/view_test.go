package cardhealth

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestBuildView(t *testing.T) {
	received := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	snap := NodeSnapshot{ReceivedAt: received, Online: true, Reports: true, Cards: []CardSnapshot{
		{Ident: "a", Index: 0, Checks: []proto.CardCheck{
			{ID: accelerator.CheckRuntime, Status: proto.CheckPass, AgeS: 300, MS: 800},
			{ID: accelerator.CheckVendorHealth, Status: proto.CheckPass},
			{ID: accelerator.CheckNVLink, Status: proto.CheckNA},
		}},
		{Ident: "b", Index: 1, Checks: []proto.CardCheck{
			{ID: accelerator.CheckVendorHealth, Status: proto.CheckFail, Code: accelerator.HealthMemoryUncorrectable},
			{ID: accelerator.CheckRuntime, Status: proto.CheckSkip, Code: proto.CardCheckNotRunYet},
		}},
	}}
	v := BuildView(snap, received.Add(30*time.Second))
	if strings.Join(v.Checks, ",") != "vendor_health,runtime" || v.Stale {
		t.Fatalf("columns %v stale %v", v.Checks, v.Stale)
	}
	runtime := v.Cards[0].Cells[1]
	if !runtime.CheckedAt.Equal(received.Add(-5*time.Minute)) || runtime.DurationMS != 800 {
		t.Fatalf("runtime cell = %+v", runtime)
	}
	if notRun := v.Cards[1].Cells[1]; !notRun.CheckedAt.IsZero() || notRun.Status != proto.CheckSkip || notRun.Code != proto.CardCheckNotRunYet {
		t.Fatalf("a check that has not run = %+v", notRun)
	}
	if Worst(v.Cards[0].Cells) != proto.CheckPass || Worst(v.Cards[1].Cells) != proto.CheckFail {
		t.Fatal("worst")
	}
	if late := BuildView(snap, received.Add(2*accelerator.ReportPeriod+staleGrace+time.Second)); !late.Stale || !late.Cards[0].Cells[0].Stale {
		t.Fatal("a late report is not stale")
	}
	snap.Online = false
	if off := BuildView(snap, received); !off.Stale {
		t.Fatal("an offline node is not stale")
	}
}

// A card whose checks did not all run never sums up as passed, and one where
// nothing applies sums up as not applicable.
func TestWorstNeverReadsASkipAsPassed(t *testing.T) {
	cells := func(statuses ...string) []Cell {
		out := make([]Cell, len(statuses))
		for i, s := range statuses {
			out[i] = Cell{Status: s}
		}
		return out
	}
	for _, tc := range []struct {
		cells []Cell
		want  string
	}{
		{cells(proto.CheckPass, proto.CheckSkip, proto.CheckNA), proto.CheckSkip},
		{cells(proto.CheckSkip, proto.CheckWarn), proto.CheckWarn},
		{cells(proto.CheckWarn, proto.CheckFail, proto.CheckSkip), proto.CheckFail},
		{cells(proto.CheckPass, proto.CheckNA), proto.CheckPass},
		{cells(proto.CheckNA), proto.CheckNA},
		{nil, proto.CheckNA},
	} {
		if got := Worst(tc.cells); got != tc.want {
			t.Errorf("Worst(%+v) = %q, want %q", tc.cells, got, tc.want)
		}
	}
}
