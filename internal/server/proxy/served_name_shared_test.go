package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A new deployment may not take a served name another holds, but deployments
// created before that rule may still share one. They are not balanced: the
// newest one that can answer takes every request, and an older one answers
// only while every newer one cannot (asleep, failed, still starting).
func TestSharedServedNameRoutesToTheNewestDeploymentThatCanAnswer(t *testing.T) {
	st, err := store.Open("file:proxy_shared_served_name?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model-shared", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("shared-name-node", store.HashToken("node"), "shared-name-node")
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"older", "newer"} {
		dep := legacySharedNameDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "shared", Engine: "vllm", State: "active", Sleepable: true, Replicas: 1})
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true, APIPort: 30021 + i})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30021 + i, DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"shared-name-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	// Each step rebuilds the route snapshot first, so the assertion is about
	// the ordering and not about how soon an asynchronous refresh lands.
	want := func(phase, uuid string) {
		t.Helper()
		p.routes.rebuild()
		for range 4 {
			_, tgt, status, msg, _ := p.selectDeploymentInstance("shared")
			if status != 0 || tgt.instanceUUID != uuid {
				t.Fatalf("%s: selection = %d %q -> %q, want %s", phase, status, msg, tgt.instanceUUID, uuid)
			}
			p.releaseTarget(tgt)
		}
	}
	want("both running", "newer")

	newer, err := st.InstanceByUUID("newer")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(newer.DeploymentID, []string{"newer"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	want("newer ordered to sleep", "older")

	if err := st.OrderInstancesSleep(newer.DeploymentID, []string{"newer"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "newer", State: proto.InstFailed}); err != nil {
		t.Fatal(err)
	}
	want("newer failed", "older")

	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "older", State: proto.InstFailed}); err != nil {
		t.Fatal(err)
	}
	p.routes.rebuild()
	if _, _, status, msg, _ := p.selectDeploymentInstance("shared"); status != http.StatusServiceUnavailable {
		t.Fatalf("neither can answer: selection = %d %q, want 503", status, msg)
	}
}

// legacySharedNameDeployment writes a deployment row the way one created
// before served names were unique was written, bypassing the create that now
// refuses a served name in use.
func legacySharedNameDeployment(t *testing.T, st *store.Store, d *store.Deployment) *store.Deployment {
	t.Helper()
	if err := st.DB().Create(d).Error; err != nil {
		t.Fatal(err)
	}
	return d
}
