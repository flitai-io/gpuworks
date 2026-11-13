package app

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pdVerifyFixture is one active PD deployment with a prefill and a decode
// rank on one node, both running and awake. uuid is the suffix that keeps the
// sqlite DSN distinct per test.
func pdVerifyFixture(t *testing.T, uuid string) (*store.Store, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:pdverify_"+uuid+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("pd-node-"+uuid, store.HashToken("token-"+uuid), "pd-node-"+uuid); err != nil {
		t.Fatal(err)
	}
	node, err := st.NodeByUUID("pd-node-" + uuid)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Name: "pd-model-" + uuid, Source: "huggingface", SourceID: "org/pd-" + uuid})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{
		Name: "pd-" + uuid, ModelID: model.ID, ServedName: "pd-" + uuid + "-served", Engine: "vllm",
		State:     store.DeployActive,
		Placement: `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"pd-node-` + uuid + `"}]},"decode":{"units":[{"node_uuid":"pd-node-` + uuid + `"}]}}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []struct{ role, suffix string }{{"prefill", "prefill"}, {"decode", "decode"}} {
		spec := `{"uuid":"` + role.suffix + `-` + uuid + `","desired_state":"running","sleepable":true}`
		if _, err := st.CreateInstance(&store.Instance{
			UUID: role.suffix + "-" + uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
			APIPort: 31000, GPUIndexes: `[0]`, Role: role.role, ScheduleExpl: `{"pd":{"role":"` + role.role + `"}}`,
			DesiredStateJSON: spec,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return st, dep
}

// The PD verifier must not verify through a replica the gateway refuses. A
// sleep order takes effect at once, before the engine reports the new level,
// and for that whole window the gateway answers 503. Reading the reported
// level alone listed the ordered replica as verifiable, so the verifier probed
// an engine on its way to sleep and recorded a handoff it never made — a
// conclusion that then excluded the replica from verification for good, since
// a conclusion is what makes verified() skip it.
func TestPDVerifySkipsInstancesOrderedAsleepBeforeTheEngineReports(t *testing.T) {
	st, dep := pdVerifyFixture(t, "pending")
	// Order the prefill to sleep and leave its reported level at 0: the drain
	// window, intent 1 / level 0.
	if err := st.OrderInstancesSleep(dep.ID, []string{"prefill-pending"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	draining, err := st.InstanceByUUID("prefill-pending")
	if err != nil {
		t.Fatal(err)
	}
	if draining.SleepIntent() == 0 || draining.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (>0, 0)", draining.SleepIntent(), draining.SleepLevel)
	}

	adapter := &pdVerifyAdapter{store: st}
	pending, err := adapter.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pending {
		if c.PrefillUUID == "prefill-pending" {
			t.Fatalf("scheduled verification through a replica under a sleep order: %+v", c)
		}
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none while the only prefill is ordered asleep", pending)
	}
}

// The decoder side of the same judgement: a decode rank ordered to sleep
// cannot verify a prefill either, so a deployment whose only decoder is
// draining has no verifiable prefill until it wakes.
func TestPDVerifyNeedsAnAwakeDecoder(t *testing.T) {
	st, dep := pdVerifyFixture(t, "decoder")
	if err := st.OrderInstancesSleep(dep.ID, []string{"decode-decoder"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	draining, err := st.InstanceByUUID("decode-decoder")
	if err != nil {
		t.Fatal(err)
	}
	if draining.SleepIntent() == 0 || draining.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (>0, 0)", draining.SleepIntent(), draining.SleepLevel)
	}

	adapter := &pdVerifyAdapter{store: st}
	pending, err := adapter.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want none: a draining decoder cannot verify anything", pending)
	}
}
