package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// capsHub is a hub whose agents advertise caps, as hub.Hub.NodeCaps reports
// them (nil for an offline node).
type capsHub struct {
	*distributedTestHub
	caps map[string][]string
}

func (h *capsHub) NodeCaps(uuid string) []string { return h.caps[uuid] }

// A deployment whose alias was cleared is launched under the model mount's
// name, so the gateway, the desired state's ServedModelName and the engine
// agree - except on a host-process agent too old to keep that name verbatim,
// which would translate it to a host path and then refuse its own engine.
func TestClearedAliasLaunchNamesTheModelUnlessTheHostAgentWouldTranslateIt(t *testing.T) {
	for i, tc := range []struct {
		name   string
		driver string
		caps   []string
		want   string
	}{
		{"container agent of any version", "docker", nil, "/model"},
		{"host agent that keeps names", proto.RuntimeDriverHost, []string{proto.CapLaunchNameVerbatim}, "/model"},
		{"older host agent", proto.RuntimeDriverHost, []string{proto.CapImagePullCandidates}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(fmt.Sprintf("file:controller_cleared_alias_%d?mode=memory&cache=shared", i), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node := sparseGPUNode(t, st, fmt.Sprintf("cleared-alias-node-%d", i))
			if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("runtime_driver", tc.driver).Error; err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: fmt.Sprintf("fixture/cleared-alias-%d", i), Revision: "main", Name: "cleared-alias",
				WeightBytes: 1 << 30,
				Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/cleared-alias"}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: "cleared-alias", ModelID: model.ID, NodeID: node.ID, Engine: "vllm", State: "active",
				Placement: `{"tp":1,"gpus":[6]}`, QuickParams: `{"served_model_name":"","max_model_len":512}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := &capsHub{
				distributedTestHub: &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}},
				caps:               map[string][]string{node.UUID: tc.caps},
			}
			c := New(st, hub, nil)
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			updates := hub.updates[node.UUID]
			if len(updates) != 1 || len(updates[0].Instances) != 1 {
				t.Fatalf("state updates=%+v, want one instance push", updates)
			}
			spec := updates[0].Instances[0]
			if got := deploy.ServedModelName(spec.ResolvedLaunch); got != tc.want {
				t.Fatalf("launch names the model %q, want %q: %v %v", got, tc.want, spec.Entrypoint, spec.Argv)
			}
			if spec.ServedModelName != tc.want {
				t.Fatalf("desired ServedModelName = %q, want %q", spec.ServedModelName, tc.want)
			}
		})
	}
}

func TestTranslatesServedNameWithoutCapsCountsTheHostAgentAsOld(t *testing.T) {
	c := New(nil, &distributedTestHub{}, nil)
	if !c.translatesServedName(&store.Node{UUID: "n", RuntimeDriver: proto.RuntimeDriverHost}) {
		t.Fatal("a host agent whose caps are unknown was trusted to keep the name")
	}
	if c.translatesServedName(&store.Node{UUID: "n", RuntimeDriver: "nerdctl"}) || c.translatesServedName(nil) {
		t.Fatal("a container agent was treated as translating the name")
	}
}

// A manual command is launched as written, so a --served-model-name /model
// in it (copied from the evaluate preview, say) reaches an older host agent,
// which translates it to the host path. The desired state then must not
// expect /model, or that agent would refuse its own engine as foreign; a name
// no mount target covers is still checked.
func TestManualCommandNameIsNotExpectedFromAnOlderHostAgent(t *testing.T) {
	for i, tc := range []struct {
		name   string
		caps   []string
		script string
		want   string
	}{
		{"older host agent, mount name", nil, "vllm serve /model --served-model-name /model --port 8000", ""},
		{"older host agent, name below the mount", nil, "vllm serve /model --served-model-name /model/x --port 8000", ""},
		{"older host agent, plain alias", nil, "vllm serve /model --served-model-name qwen --port 8000", "qwen"},
		{"host agent that keeps names", []string{proto.CapLaunchNameVerbatim}, "vllm serve /model --served-model-name /model --port 8000", "/model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(fmt.Sprintf("file:controller_manual_name_%d?mode=memory&cache=shared", i), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node := sparseGPUNode(t, st, fmt.Sprintf("manual-name-node-%d", i))
			if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("runtime_driver", proto.RuntimeDriverHost).Error; err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: fmt.Sprintf("fixture/manual-name-%d", i), Revision: "main", Name: "manual-name",
				WeightBytes: 1 << 30,
				Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/manual-name"}); err != nil {
				t.Fatal(err)
			}
			overrides, err := json.Marshal(map[string]string{"command_mode": "manual", "cmd_display": tc.script})
			if err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: "manual-name", ModelID: model.ID, NodeID: node.ID, Engine: "vllm", State: "active",
				Placement: `{"tp":1,"gpus":[6]}`, QuickParams: `{"served_model_name":"","max_model_len":512}`,
				ContainerOverrides: string(overrides),
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := &capsHub{
				distributedTestHub: &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}},
				caps:               map[string][]string{node.UUID: tc.caps},
			}
			if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			updates := hub.updates[node.UUID]
			if len(updates) != 1 || len(updates[0].Instances) != 1 {
				t.Fatalf("state updates=%+v, want one instance push", updates)
			}
			spec := updates[0].Instances[0]
			if spec.CommandMode != "manual" {
				t.Fatalf("command mode = %q, want manual", spec.CommandMode)
			}
			if spec.ServedModelName != tc.want {
				t.Fatalf("desired ServedModelName = %q, want %q (launch %v %v)", spec.ServedModelName, tc.want, spec.Entrypoint, spec.Argv)
			}
		})
	}
}
