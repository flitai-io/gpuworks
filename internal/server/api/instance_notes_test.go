package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An instance on a card shared with processes outside the platform carries
// the note on every read of it - alone, in the list, in its deployment - and
// one on a clean card carries no notes key at all.
func TestInstanceNotesOnTheWire(t *testing.T) {
	a := newPlacementPolicyAPI(t)
	node, err := a.st.NodeByUUID("policy")
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range []store.GPU{
		{NodeID: node.ID, Index: 0, Ident: "g0", UUID: "g0", MemoryTotal: 140 << 30, ForeignProcesses: `[{"pid":41235,"name":"python3","memory_bytes":67089334272}]`},
		{NodeID: node.ID, Index: 1, Ident: "g1", UUID: "g1", MemoryTotal: 140 << 30},
	} {
		if err := a.st.DB().Create(&card).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := a.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/notes", Revision: "main", Name: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := a.st.CreateDeployment(&store.Deployment{Name: "notes", ServedName: "notes", ModelID: model.ID, NodeID: node.ID, Engine: "vllm", State: "active", Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	for uuid, cards := range map[string]string{"beside": "[0]", "clean": "[1]"} {
		if _, err := a.st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: cards}); err != nil {
			t.Fatal(err)
		}
	}
	want := `[{"code":"INSTANCE_CARD_SHARED_OUTSIDE","params":{"foreign_bytes":67089334272,"foreign_gib":"62.5","gpu_indexes":[0]}}]`
	notesOf := func(raw string) map[string]string {
		t.Helper()
		var items []map[string]json.RawMessage
		var page struct {
			Items     []map[string]json.RawMessage `json:"items"`
			Instances []map[string]json.RawMessage `json:"instances"`
		}
		if strings.HasPrefix(strings.TrimSpace(raw), "[") {
			_ = json.Unmarshal([]byte(raw), &items)
		} else if err := json.Unmarshal([]byte(raw), &page); err == nil {
			items = append(page.Items, page.Instances...)
		}
		if len(items) == 0 {
			var one map[string]json.RawMessage
			_ = json.Unmarshal([]byte(raw), &one)
			items = []map[string]json.RawMessage{one}
		}
		out := map[string]string{}
		for _, item := range items {
			var uuid string
			_ = json.Unmarshal(item["uuid"], &uuid)
			notes, present := item["notes"]
			if present {
				out[uuid] = string(notes)
			} else {
				out[uuid] = ""
			}
		}
		return out
	}
	for _, path := range []string{"/api/v1/instances/beside", "/api/v1/instances", fmt.Sprintf("/api/v1/deployments/%d", dep.ID)} {
		status, _, raw := a.do(a.token, http.MethodGet, path, "")
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, status, raw)
		}
		got := notesOf(raw)
		if got["beside"] != want {
			t.Fatalf("%s: beside notes = %s\nwant %s\nbody %s", path, got["beside"], want, raw)
		}
		if note, seen := got["clean"]; seen && note != "" {
			t.Fatalf("%s: clean instance carries notes %s", path, note)
		}
	}
	// The processes leave: the note goes on the next read.
	if err := a.st.DB().Model(&store.GPU{}).Where("node_id = ?", node.ID).Update("foreign_processes", "").Error; err != nil {
		t.Fatal(err)
	}
	if status, _, raw := a.do(a.token, http.MethodGet, "/api/v1/instances/beside", ""); status != http.StatusOK || strings.Contains(raw, `"notes"`) {
		t.Fatalf("after the processes left: %d %s", status, raw)
	}
}
