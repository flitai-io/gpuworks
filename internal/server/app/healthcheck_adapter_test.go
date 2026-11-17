package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/healthcheck"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The checks go to the deployments with the check on, each with what its
// model serves: chat first, else text completion, else embeddings; a stopped one is listed
// inactive, and one serving neither is not checked.
func TestHealthSourceListsTheCheckedDeploymentsAndWhatToAsk(t *testing.T) {
	st, err := store.Open("file:health_source?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model := func(name, ops string) uint {
		m, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/" + name, Revision: "main", Name: name, Operations: ops})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	chat := model("chat", `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"},{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`)
	emb := model("emb", `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"}]`)
	rerank := model("rerank", `[{"type":"openai.rerank","endpoint":"/v1/rerank"}]`)
	decision := model("decision", `[{"type":"systemone.decide","endpoint":"/v1/systemone"}]`)
	completion := model("completion", `[{"type":"openai.embeddings","endpoint":"/v1/embeddings"},{"type":"openai.completions","endpoint":"/v1/completions"}]`)
	deployment := func(name string, modelID uint, state store.DeploymentState, on bool, threshold int) uint {
		d, err := st.CreateDeployment(&store.Deployment{Name: name, ServedName: name, ModelID: modelID, State: state})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetDeploymentHealthCheck(d.ID, on, threshold); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	a := deployment("a", chat, store.DeployActive, true, 60)
	b := deployment("b", emb, store.DeployActive, true, 5)
	c := deployment("c", chat, store.DeployStopped, true, 60)
	deployment("off", chat, store.DeployActive, false, 60)
	deployment("ranker", rerank, store.DeployActive, true, 60)
	d := deployment("translate", completion, store.DeployActive, true, 30)
	e := deployment("triage", decision, store.DeployActive, true, 10)

	got, err := healthSource{store: st}.Targets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []healthcheck.Target{
		{DeploymentID: a, Kind: healthcheck.KindChat, Path: "/v1/chat/completions", Threshold: time.Minute, Active: true},
		{DeploymentID: b, Kind: healthcheck.KindEmbeddings, Path: "/v1/embeddings", Threshold: 5 * time.Second, Active: true},
		{DeploymentID: c, Kind: healthcheck.KindChat, Path: "/v1/chat/completions", Threshold: time.Minute},
		{DeploymentID: d, Kind: healthcheck.KindCompletions, Path: "/v1/completions", Threshold: 30 * time.Second, Active: true},
		{DeploymentID: e, Kind: healthcheck.KindDecision, Path: "/v1/systemone", Threshold: 10 * time.Second, Active: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets =\n%+v\nwant\n%+v", got, want)
	}
}
