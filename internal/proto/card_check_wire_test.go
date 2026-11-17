package proto

import (
	"encoding/json"
	"testing"
)

// A status report carries each check in short keys, and leaves out what a
// check does not have.
func TestCardCheckWire(t *testing.T) {
	got, err := json.Marshal(GPUInfo{Index: 0, UUID: "GPU-1", Checks: []CardCheck{
		{ID: "vendor_health", Status: CheckPass},
		{ID: "runtime", Status: CheckSkip, Code: CardCheckSkipBusy, AgeS: 388, MS: 812},
		{ID: "pcie_link", Status: CheckWarn, Code: "PCIE_LINK_DEGRADED", Evidence: []DoctorField{{Name: "PCIe Link 0000:3f:00.0", Value: "x8 of x16"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"index":0,"uuid":"GPU-1","name":"","memory_total_bytes":0,"compute_capability":"","numa_node":0,"healthy":false,` +
		`"checks":[{"id":"vendor_health","s":"pass"},{"id":"runtime","s":"skip","r":"CARD_CHECK_SKIPPED_BUSY","age":388,"ms":812},` +
		`{"id":"pcie_link","s":"warn","r":"PCIE_LINK_DEGRADED","e":[{"name":"PCIe Link 0000:3f:00.0","value":"x8 of x16"}]}]}`
	if string(got) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", got, want)
	}
	var back GPUInfo
	if err := json.Unmarshal(got, &back); err != nil || len(back.Checks) != 3 || back.Checks[1].AgeS != 388 || back.Checks[1].MS != 812 ||
		back.Checks[2].Evidence[0].Value != "x8 of x16" {
		t.Fatalf("round trip = %+v, %v", back.Checks, err)
	}
	// An older agent sends no checks at all.
	var old GPUInfo
	if err := json.Unmarshal([]byte(`{"index":1,"uuid":"GPU-2"}`), &old); err != nil || old.Checks != nil {
		t.Fatalf("old agent = %+v, %v", old.Checks, err)
	}
}

func TestCardCheckDetailWire(t *testing.T) {
	got, err := json.Marshal(CardCheckDetail{Ident: "GPU-1", Check: "runtime", AgentStartedAt: "2026-11-01T00:00:00Z",
		Last: &CardCheckRun{Status: CheckPass, At: "2026-11-01T01:00:00Z", DurationMS: 812, Probe: "python3 -c ..."}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ident":"GPU-1","check":"runtime","last":{"state":"pass","at":"2026-11-01T01:00:00Z","duration_ms":812,"probe":"python3 -c ..."},"agent_started_at":"2026-11-01T00:00:00Z"}`
	if string(got) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", got, want)
	}
}
