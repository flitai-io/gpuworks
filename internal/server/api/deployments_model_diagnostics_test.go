package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluateSharedSaveTargetsRequirePublicModelSource(t *testing.T) {
	for _, source := range []string{"local", "huggingface"} {
		t.Run(source, func(t *testing.T) {
			h, model, node, _ := storageFallbackFixture(t)
			if source == "local" {
				var err error
				model, err = h.d.Store.UpsertModel(&store.Model{Source: "local", SourceID: "registered-model", Name: "local-model", WeightBytes: 1 << 30})
				if err != nil {
					t.Fatal(err)
				}
				peer, err := h.d.Store.NodeByUUID("existing-node")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.d.Store.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: peer.ID,
					State: proto.MFPresent, Path: "/models/registered-model"}); err != nil {
					t.Fatal(err)
				}
				for _, source := range []*store.Node{node, peer} {
					inventory, err := json.Marshal(testManagement(fmt.Sprintf("192.0.2.%d", source.ID)))
					if err != nil {
						t.Fatal(err)
					}
					if err := h.d.Store.DB().Model(&store.Node{}).Where("id = ?", source.ID).Update("network_inventory", string(inventory)).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			empty, err := h.d.Store.CreateStorage(&store.Storage{Name: "empty-default", Type: proto.StorageNFS,
				Enabled: true, AllNodes: true, IsDefault: true, Path: "/mnt/empty-default", Export: "nfs.example:/empty"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.d.Store.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{
				ID: empty.ID, State: proto.StorageStateReady, Path: empty.Path, Writable: true,
			}}}); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"engine":"vllm","image":"vllm/vllm-openai:latest","model_source":{"provider":%q}}`, model.ID, node.UUID, source)
			out := httptest.NewRecorder()
			h.evaluate(out, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			var response dto.EvaluateResponse
			if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil || out.Code != http.StatusOK || !response.OK || response.Acquisition == nil {
				t.Fatalf("evaluate=%d %s err=%v", out.Code, out.Body.String(), err)
			}
			offered := false
			for _, target := range response.Acquisition.Targets {
				offered = offered || target.StorageID == empty.ID
			}
			if offered != (source == "huggingface") {
				t.Fatalf("unfillable default shared target offered=%v for %s: %s", offered, source, out.Body.String())
			}
		})
	}
}
