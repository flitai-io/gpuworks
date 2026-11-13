package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImageRuntimeSizeSummaryWire(t *testing.T) {
	run := newImageManifestRun(t)
	var image store.Image
	if err := run.store.DB().First(&image).Error; err != nil {
		t.Fatal(err)
	}
	known, zero := int64(8192), int64(0)
	for i, row := range []store.ImageNode{
		{State: store.ImageStatePresent, SizeBytes: 128, RuntimeSizeBytes: &known},
		{State: store.ImageStatePresent, SizeBytes: 256, RuntimeSizeBytes: &zero},
		{State: store.ImageStatePresent, SizeBytes: 512},
		{State: store.ImageStateAbsent, SizeBytes: 1024, RuntimeSizeBytes: &known},
		{State: store.ImageStateSyncing, SizeBytes: 2048, RuntimeSizeBytes: &known},
	} {
		row.ImageVersionID = image.ID
		node, err := run.store.RegisterNode(fmt.Sprintf("size-node-%d", i), store.HashToken(fmt.Sprintf("size-token-%d", i)), fmt.Sprintf("size-node-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		row.NodeID = node.ID
		if _, err := run.store.UpsertImageNode(&row); err != nil {
			t.Fatal(err)
		}
	}
	rec := run.do(http.MethodGet, "/api/v1/images/versions", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range body.Items {
		var item dto.Image
		encoded, _ := json.Marshal(raw)
		if err := json.Unmarshal(encoded, &item); err != nil {
			t.Fatal(err)
		}
		if item.ID != image.ID {
			continue
		}
		found = true
		for key, want := range map[string]string{"present_nodes": "3", "local_runtime_size_bytes": "8192", "local_size_known_nodes": "2"} {
			if string(raw[key]) != want {
				t.Fatalf("%s=%s want=%s", key, raw[key], want)
			}
		}
		if _, ok := raw["size_bytes"]; ok {
			t.Fatalf("system catalog invented archive size: %s", raw["size_bytes"])
		}
		if _, ok := raw["nodes"]; ok {
			t.Fatal("summary leaked node rows")
		}
		roundtrip, _ := json.Marshal(item)
		var again map[string]json.RawMessage
		_ = json.Unmarshal(roundtrip, &again)
		if string(again["local_runtime_size_bytes"]) != "8192" {
			t.Fatalf("roundtrip=%s", roundtrip)
		}
	}
	if !found {
		t.Fatal("version missing")
	}
}
