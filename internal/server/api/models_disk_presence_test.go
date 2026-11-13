package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The page summary must retain the real disk-space failure just as the model
// detail does. Otherwise the list describes its only copy as not downloaded,
// hiding the failed acquisition before the operator opens the model.
func TestModelPageCountsDiskSpaceFailureAndKeepsItsDetail(t *testing.T) {
	for _, location := range []string{"local", "shared"} {
		t.Run(location, func(t *testing.T) {
			st, err := store.Open(":memory:", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("disk-node", store.HashToken("disk-node"), "disk-node")
			if err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/disk-only", Revision: "main", Name: "disk-only", WeightBytes: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			const detail = "insufficient disk space at /mnt/models: available=1048576 required=1073741824"
			file := store.ModelFile{ModelID: model.ID, NodeID: node.ID, Path: "/mnt/models/fixture/model", State: proto.MFInsufficientDisk, Message: detail}
			if location == "shared" {
				storage, err := st.CreateStorage(&store.Storage{Name: "shared-models", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: "/mnt/models", Export: "fixture:/models"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				file.StorageID, file.NodeID, file.WriterNodeID, file.Path = storage.ID, 0, node.ID, "fixture/model"
			}
			if _, err := st.UpsertModelFile(&file); err != nil {
				t.Fatal(err)
			}
			router, token := modelEvictionRouter(t, st, nil)
			get := func(path string, out any) {
				t.Helper()
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.Header.Set("Authorization", "Bearer "+token)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
				}
				if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
					t.Fatal(err)
				}
			}
			var page struct {
				Items []struct {
					ID       uint           `json:"id"`
					Presence map[string]int `json:"presence"`
				} `json:"items"`
				Total int `json:"total"`
			}
			get("/api/v1/models?page=1", &page)
			if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != model.ID {
				t.Fatalf("model page=%+v", page)
			}
			want := map[string]int{"ready_nodes": 0, "cached_nodes": 0, "ready_storages": 0, "in_flight": 0, "in_flight_percent": 0, "failed": 1}
			for key, value := range want {
				if got, present := page.Items[0].Presence[key]; !present || got != value {
					t.Errorf("presence.%s=%d present=%v want=%d; row=%+v", key, got, present, value, page.Items[0])
				}
			}
			type locationDetail struct {
				State   string `json:"state"`
				Path    string `json:"path"`
				Message string `json:"message"`
			}
			var modelDetail struct {
				ID       uint             `json:"id"`
				Nodes    []locationDetail `json:"nodes"`
				Storages []locationDetail `json:"storages"`
			}
			get(fmt.Sprintf("/api/v1/models/%d", model.ID), &modelDetail)
			copies := append(modelDetail.Nodes, modelDetail.Storages...)
			if modelDetail.ID != model.ID || len(copies) != 1 || copies[0].State != "insufficient_disk_space" || copies[0].Message != detail || copies[0].Path != file.Path {
				t.Fatalf("model detail lost the disk failure or original path: %+v", modelDetail)
			}
		})
	}
}
