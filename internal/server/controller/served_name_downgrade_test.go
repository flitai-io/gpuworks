package controller

import (
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A replica launched with --served-model-name /model on a host agent that
// keeps names verbatim, whose node is then downgraded to an agent that
// translates them: that agent's relaunch registers the host path, its port
// identity check refuses the engine with ENGINE_PORT_IN_USE, and the retry is
// planned against the agent now connected, without the flag and without an
// expected name, so the gateway reaches it under its host model path. The
// failure is an ordinary charged one: only ENGINE_PORT_REFUSED is free.
func TestDowngradedHostAgentRetriesAFlaggedReplicaWithoutTheName(t *testing.T) {
	st, err := store.Open("file:controller_served_name_downgrade?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node := sparseGPUNode(t, st, "served-name-downgrade-node")
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("runtime_driver", proto.RuntimeDriverHost).Error; err != nil {
		t.Fatal(err)
	}
	// The host runtime exists only on a Mac, so the node's cards are a Mac's.
	if err := st.DB().Model(&store.GPU{}).Where("node_id = ?", node.ID).Update("vendor", "apple").Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/served-name-downgrade", Revision: "main", Name: "served-name-downgrade",
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/Users/a/models/q"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "served-name-downgrade", ModelID: model.ID, NodeID: node.ID, Engine: "vllm", State: "active",
		Placement: `{"tp":1,"gpus":[6]}`, QuickParams: `{"served_model_name":"","max_model_len":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &capsHub{
		distributedTestHub: &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}},
		caps:               map[string][]string{node.UUID: {proto.CapLaunchNameVerbatim}},
	}
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rows, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("instances = %+v, %v", rows, err)
	}
	first := rows[0]
	var flagged proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(first.DesiredStateJSON), &flagged); err != nil {
		t.Fatal(err)
	}
	if deploy.ServedModelName(flagged.ResolvedLaunch) != "/model" || flagged.ServedModelName != "/model" {
		t.Fatalf("first launch on the verbatim agent: name %q expected %q", deploy.ServedModelName(flagged.ResolvedLaunch), flagged.ServedModelName)
	}

	// The node's agent is replaced by one that predates CapLaunchNameVerbatim.
	hub.caps[node.UUID] = []string{proto.CapImagePullCandidates}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: first.UUID, State: proto.InstFailed,
		Message: proto.ErrorEnginePortInUse + ": port 30000 serves /Users/a/models/q, not /model"}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rows, err = st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	var retry *store.Instance
	for i := range rows {
		if rows[i].UUID != first.UUID && rows[i].State != proto.InstFailed {
			retry = &rows[i]
		}
	}
	if retry == nil {
		t.Fatalf("no retry after the identity refusal: %+v", rows)
	}
	var plain proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(retry.DesiredStateJSON), &plain); err != nil {
		t.Fatal(err)
	}
	if name := deploy.ServedModelName(plain.ResolvedLaunch); name != "" || plain.ServedModelName != "" {
		t.Fatalf("retry on the translating agent: name %q expected %q, want neither (%v %v)", name, plain.ServedModelName, plain.Entrypoint, plain.Argv)
	}
	if plain.ModelPath != "/Users/a/models/q" || plain.ModelMount == "" {
		t.Fatalf("retry model path %q mount %q: the gateway names the engine from them", plain.ModelPath, plain.ModelMount)
	}
	var failed []store.Instance
	for _, row := range rows {
		if row.State == proto.InstFailed {
			failed = append(failed, row)
		}
	}
	if charged := planning.ChargedAttempts(failed); len(charged) != 1 || charged[0].UUID != first.UUID {
		t.Fatalf("charged attempts = %d, want the refused engine alone", len(charged))
	}
	if planning.RetryAtOnce(failed) {
		t.Fatal("an engine refused by its identity check was retried without backoff, like a port refusal")
	}
}
