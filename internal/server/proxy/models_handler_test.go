package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// modelIDs reads GET /v1/models as a client does.
func modelIDs(t *testing.T, p *Proxy, key string) (int, []string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	p.ModelsHandler()(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || list.Data == nil {
		t.Fatalf("models body = %s, want an OpenAI list", rec.Body)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.Object != "model" || m.OwnedBy != "gpuworks" {
			t.Fatalf("model entry = %+v", m)
		}
		ids = append(ids, m.ID)
	}
	return rec.Code, ids
}

// GET /v1/models answers from the route table the gateway routes by: a client
// polling it reads no deployment rows, the names it lists are the ones a
// request can reach, and a name two deployments share is listed once.
func TestModelsListsTheRoutedNamesWithoutReadingDeployments(t *testing.T) {
	st, err := store.Open("file:proxy_models_routes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/models", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("models-node", store.HashToken("node"), "models-node")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := st.CreateDeployment(&store.Deployment{Name: fmt.Sprintf("d%02d", i), ModelID: model.ID, NodeID: node.ID, ServedName: fmt.Sprintf("served-%02d", i), Engine: "vllm", State: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	// Created before served names were unique: both are routed under one name.
	for range 2 {
		legacySharedNameDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "shared", Engine: "vllm", State: "active"})
	}
	if _, err := st.CreateDeployment(&store.Deployment{Name: "stopped", ModelID: model.ID, NodeID: node.ID, ServedName: "stopped", Engine: "vllm", State: store.DeployStopped}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(&store.Deployment{Name: "unnamed", ModelID: model.ID, NodeID: node.ID, Engine: "vllm", State: "active"}); err != nil {
		t.Fatal(err)
	}
	key, err := st.CreateAPIKey("models", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), dialTestHub{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(p.Close)
	p.routes.rebuild()

	var deploymentReads atomic.Int64
	name := "test:models_deployment_reads"
	if err := st.DB().Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "deployments" {
			deploymentReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(name) })

	code, ids := modelIDs(t, p, key)
	if code != http.StatusOK {
		t.Fatalf("models = %d", code)
	}
	if reads := deploymentReads.Load(); reads != 0 {
		t.Errorf("one GET /v1/models read the deployments table %d times, want 0: the route table already holds them", reads)
	}
	want := []string{"shared"}
	for i := 0; i < 20; i++ {
		want = append(want, fmt.Sprintf("served-%02d", i))
	}
	slices.Sort(want)
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	if !slices.Equal(sorted, want) {
		t.Fatalf("models = %v, want %v (every routed name once; no stopped or unnamed deployment)", ids, want)
	}
	if !slices.IsSorted(ids) {
		t.Errorf("models = %v, want them in name order so a poll reads the same list", ids)
	}
}
