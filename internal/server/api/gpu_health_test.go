package api

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func gpuJSON(t *testing.T, g store.GPU) map[string]any {
	t.Helper()
	node := &store.Node{UUID: "n1", GPUs: []store.GPU{g}}
	raw, err := json.Marshal(nodeDTO(node, false))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		GPUs []map[string]any `json:"gpus"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.GPUs) != 1 {
		t.Fatalf("node json = %s, %v", raw, err)
	}
	return out.GPUs[0]
}

func TestNodeGPUHealthWireCarriesEvidenceAndVendorCommands(t *testing.T) {
	gpu := gpuJSON(t, store.GPU{
		Index: 5, Vendor: "ppu", PCIeBDF: "0000:7e:00.0",
		HealthStatus: "Warning", HealthCode: accelerator.HealthMemoryRepairPending,
		HealthMessage: "remapped_rows.pending=Yes",
	})
	raw, _ := json.Marshal(map[string]any{
		"schedulable": gpu["schedulable"], "health_status": gpu["health_status"], "health_code": gpu["health_code"],
		"health_evidence": gpu["health_evidence"], "health_guide": gpu["health_guide"],
	})
	want := `{"health_code":"MEMORY_REPAIR_PENDING",` +
		`"health_evidence":[{"name":"remapped_rows.pending","value":"Yes"}],` +
		`"health_guide":{"actions":["ppu-smi -r -i 0000:7e:00.0"],` +
		`"probe":"ppu-smi --query-ppu=pci.bus_id,remapped_rows.pending --format=csv -i 0000:7e:00.0",` +
		`"recheck":"ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0"},` +
		`"health_status":"Warning","schedulable":true}`
	if string(raw) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", raw, want)
	}
}

func TestNodeGPUHealthWireOmitsDetailForHealthyCards(t *testing.T) {
	gpu := gpuJSON(t, store.GPU{Index: 0, Vendor: "ppu", PCIeBDF: "0000:7e:00.0", HealthStatus: "OK"})
	for _, key := range []string{"health_evidence", "health_guide", "health_code", "health_message"} {
		if _, ok := gpu[key]; ok {
			t.Errorf("healthy card carries %s: %v", key, gpu[key])
		}
	}
}

// A row written by an agent that predates structured evidence keeps showing
// its sentence and offers no commands for a code this build does not know.
func TestNodeGPUHealthWireReadsLegacyRows(t *testing.T) {
	gpu := gpuJSON(t, store.GPU{
		Index: 5, Vendor: "ppu", PCIeBDF: "0000:7e:00.0",
		HealthStatus: "Warning", HealthCode: "ppu_pending_repair", HealthMessage: "remapped rows pending",
	})
	raw, _ := json.Marshal(gpu["health_evidence"])
	if string(raw) != `[{"name":"","value":"remapped rows pending"}]` {
		t.Fatalf("legacy evidence = %s", raw)
	}
	if _, ok := gpu["health_guide"]; ok || gpu["health_message"] != "remapped rows pending" {
		t.Fatalf("legacy row = %v", gpu)
	}
}

func TestNodeGPUHealthGuideFallsBackToIndexWithoutAddress(t *testing.T) {
	gpu := gpuJSON(t, store.GPU{Index: 3, Vendor: "nvidia", HealthStatus: "Error",
		HealthCode: accelerator.HealthMemoryRemapFailed, HealthMessage: "remapped_rows.failure=Yes"})
	guide, _ := gpu["health_guide"].(map[string]any)
	if guide == nil || !strings.HasSuffix(guide["recheck"].(string), " -i 3") || guide["actions"] != nil {
		t.Fatalf("guide = %v", gpu["health_guide"])
	}
}
