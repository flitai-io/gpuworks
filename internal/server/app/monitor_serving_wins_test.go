package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// GWKS-334: a replica the gateway serves from wins over a warming one, on
// every surface. These restore the monitor's "serving replica outranks
// warming replacement" and "sleeping service with warming replacement" cases
// (dropped when the monitor started reading the list's state) and assert the
// list state (which the dashboard buckets) and the batch model state agree,
// along with the multi-node cases where a follower decides whether the
// gateway answers at all.
func TestServingReplicaWinsOnEverySurface(t *testing.T) {
	sleepable := func(uuid string) string {
		return fmt.Sprintf(`{"uuid":%q,"desired_state":"running","sleepable":true}`, uuid)
	}
	type inst = store.Instance
	for _, tc := range []struct {
		name        string
		replicas    int
		nnodes      int
		instances   []inst
		wantList    string
		wantMonitor string
		wantBatch   batch.ModelState
	}{
		{
			name: "serving replica outranks warming replacement", replicas: 2,
			instances: []inst{
				{UUID: "a", State: proto.InstRunning, GPUIndexes: "[0]"},
				{UUID: "b", State: proto.InstPulling, GPUIndexes: "[1]"},
			},
			wantList: "running", wantMonitor: "running", wantBatch: batch.ModelRunning,
		},
		{
			name: "rolling restart keeps serving", replicas: 1,
			instances: []inst{
				{UUID: "a", State: proto.InstRunning, GPUIndexes: "[0]"},
				{UUID: "b", State: proto.InstStarting, GPUIndexes: "[1]"},
			},
			wantList: "running", wantMonitor: "running", wantBatch: batch.ModelRunning,
		},
		{
			name: "sleeping service with warming replacement", replicas: 2,
			instances: []inst{
				{UUID: "a", State: proto.InstRunning, GPUIndexes: "[0]", SleepLevel: 1, DesiredStateJSON: sleepable("a")},
				{UUID: "b", State: proto.InstStarting, GPUIndexes: "[1]", DesiredStateJSON: sleepable("b")},
			},
			wantList: deployment.StateAsleep, wantMonitor: "asleep", wantBatch: batch.ModelAsleep,
		},
		{
			name: "leader failed beside a draining follower", replicas: 1, nnodes: 2,
			instances: []inst{
				{UUID: "l", State: proto.InstFailed, NNodes: 2, DistGroup: "g", DistAttempt: 1},
				{UUID: "f", State: proto.InstDraining, NNodes: 2, NodeRank: 1, Headless: true, DistGroup: "g", DistAttempt: 1, DesiredStateJSON: `{"headless":true}`},
			},
			wantList: "failed", wantMonitor: "failed", wantBatch: batch.ModelFailed,
		},
		{
			name: "leader running beside a failed follower", replicas: 1, nnodes: 2,
			instances: []inst{
				{UUID: "l", State: proto.InstRunning, NNodes: 2, DistGroup: "g", DistAttempt: 1},
				{UUID: "f", State: proto.InstFailed, NNodes: 2, NodeRank: 1, Headless: true, DistGroup: "g", DistAttempt: 1, DesiredStateJSON: `{"headless":true}`},
			},
			wantList: "failed", wantMonitor: "failed", wantBatch: batch.ModelFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ts := newMonitorTestStore(t), newMonitorTestTSDB(t)
			node, err := st.RegisterNode("n", store.HashToken("token"), "n")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: "d", ServedName: "d", Engine: "vllm", NodeID: node.ID, Replicas: tc.replicas, NNodes: max(tc.nnodes, 1), State: store.DeployActive, Sleepable: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range tc.instances {
				in.DeploymentID, in.NodeID = dep.ID, node.ID
				if _, err := st.CreateInstance(&in); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := deployment.ReadListRows(st, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := rows.States()[dep.ID]; got != tc.wantList {
				t.Errorf("list (and dashboard) state = %q, want %q", got, tc.wantList)
			}
			overview, err := newMonitorAPIService(st, ts, time.Now, nil, nil, nil).Overview(context.Background(), time.Hour)
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
