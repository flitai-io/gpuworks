package app

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The monitor's service state and the batch model state are the state the
// deployment list shows, judged as the gateway judges a request: a PD
// deployment some role of which it cannot answer without, and whose awake
// replicas are all of other roles, is refused for every request and reads
// asleep, not running on the strength of the role that is still awake. The
// list's own cases are TestDeploymentPDAsleepDecodeRefuses and
// TestDeploymentPDRequiredPrefillAsleepRefuses.
func TestMonitorAndBatchPDSleepStateMatchTheGateway(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		mode                      string
		prefillSleep, decodeSleep int
		wantMonitor               string
		wantBatch                 batch.ModelState
	}{
		{name: "awake", mode: "vllm-nixl", wantMonitor: "running", wantBatch: batch.ModelRunning},
		// The mode serves from its decoders alone: the gateway still answers.
		{name: "optional prefill asleep", mode: "vllm-nixl", prefillSleep: 1, wantMonitor: "running", wantBatch: batch.ModelRunning},
		// No decoder is awake: the gateway refuses every request although
		// prefill is.
		{name: "decode asleep", mode: "vllm-nixl", decodeSleep: 1, wantMonitor: "asleep", wantBatch: batch.ModelAsleep},
		// sglang-nixl cannot run without prefill.
		{name: "required prefill asleep", mode: "sglang-nixl", prefillSleep: 1, wantMonitor: "asleep", wantBatch: batch.ModelAsleep},
		{name: "both asleep", mode: "vllm-nixl", prefillSleep: 1, decodeSleep: 1, wantMonitor: "asleep", wantBatch: batch.ModelAsleep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ts := newMonitorTestStore(t), newMonitorTestTSDB(t)
			node, err := st.RegisterNode("pd-node", store.HashToken("token"), "pd-node")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
				t.Fatal(err)
			}
			placement := `{"pd":{"mode":"` + tc.mode + `","roles":{"prefill":{"units":[{"node_uuid":"pd-node","gpus":[0]}]},"decode":{"units":[{"node_uuid":"pd-node","gpus":[1]}]}}}}`
			dep, err := st.CreateDeployment(&store.Deployment{Name: "pd", ServedName: "pd", Engine: "vllm", NodeID: node.ID, Replicas: 1, State: store.DeployActive, Sleepable: true, Placement: placement})
			if err != nil {
				t.Fatal(err)
			}
			for _, inst := range []store.Instance{
				{UUID: "prefill", DeploymentID: dep.ID, NodeID: node.ID, Role: "prefill", State: proto.InstRunning, GPUIndexes: "[0]", SleepLevel: tc.prefillSleep,
					DesiredStateJSON: `{"uuid":"prefill","desired_state":"running","sleepable":true,"role":"prefill"}`},
				{UUID: "decode", DeploymentID: dep.ID, NodeID: node.ID, Role: "decode", State: proto.InstRunning, GPUIndexes: "[1]", SleepLevel: tc.decodeSleep,
					DesiredStateJSON: `{"uuid":"decode","desired_state":"running","sleepable":true,"role":"decode"}`},
			} {
				if _, err := st.CreateInstance(&inst); err != nil {
					t.Fatal(err)
				}
			}
			svc := newMonitorAPIService(st, ts, func() time.Time { return time.Now() }, nil, nil, nil)
			overview, err := svc.Overview(context.Background(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if len(overview.Services) != 1 || overview.Services[0].State != tc.wantMonitor {
				t.Errorf("monitor services = %+v, want one in state %q", overview.Services, tc.wantMonitor)
			}
			entries, err := batchModels{st: st}.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].State != tc.wantBatch {
				t.Errorf("batch models = %+v, want one in state %v", entries, tc.wantBatch)
			}
		})
	}
}

// A multi-node replica whose leader failed while a follower rank still runs
// serves nothing: the gateway only routes to leaders. The monitor, the list
// and the batches read it failed, not running on the follower's strength.
func TestMonitorAndBatchHeadlessRankIsNotAServingReplica(t *testing.T) {
	st, ts := newMonitorTestStore(t), newMonitorTestTSDB(t)
	node, err := st.RegisterNode("n", store.HashToken("token"), "n")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "mn", ServedName: "mn", Engine: "vllm", NodeID: node.ID, Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []store.Instance{
		{UUID: "leader", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstFailed, NNodes: 2, DistGroup: "g", DistAttempt: 1},
		{UUID: "follower", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, NNodes: 2, NodeRank: 1, Headless: true, DistGroup: "g", DistAttempt: 1, DesiredStateJSON: `{"headless":true}`},
	} {
		if _, err := st.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := newMonitorAPIService(st, ts, time.Now, nil, nil, nil).Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Services) != 1 || overview.Services[0].State != "failed" {
		t.Errorf("monitor services = %+v, want one failed", overview.Services)
	}
	entries, err := batchModels{st: st}.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].State != batch.ModelFailed {
		t.Errorf("batch models = %+v, want one failed", entries)
	}
}
