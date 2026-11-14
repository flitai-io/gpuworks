package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The deployment DTO reads deployments.node_ids twice: once for the rank-ordered
// node_uuids/node_names, once for which nodes' model files make up
// model_progress. Pin the wire bytes for every stored shape: a single-node
// deployment carries no node list (no node_uuids key at all), an unreadable list
// is reported as no list (no node_uuids, no units) while model_progress falls
// back to the primary node. A single-node deployment whose one replica has no
// instance yet says so (placement_status); with no scheduling event recorded
// there is no reason to give.
func TestDeploymentDTONodeIDsWire(t *testing.T) {
	const progress = `{"model_id":1,"model_name":"org/m","preparation":{"phase":"ready","total_known":false,"idle_seconds":0},"node_uuid":"%s","node_name":"%s","state":"present","path":"/m","progress":0,"size_bytes":0,"downloaded":0,"transferred_bytes":0,"rate_bps":0,"acquisition":"","storage_id":1,"storage_name":"default","storage_type":"local"}`
	progressA := fmt.Sprintf(progress, "node-a", "node-a")
	progressB := fmt.Sprintf(progress, "node-b", "node-b")
	const head = `{"operations":[],"id":1,"template_deviated":false,"name":"dep","engine":"","model_id":1,"model_name":"org/m","node_uuid":"node-a","node_name":"node-a",`
	const tail = `"node_requirements":{},"placement_strategy":"spread","served_name":"","desired_state":"active","runtime_state":"pending","state":"pending","sleepable":false,"replicas":0,"max_replicas":1024,"routing_policy":"least_request",`
	// No replica runs, and the check is off.
	const ready = `"ready_replicas":0,`
	const health = `"health":{"status":"off","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]},"tokenize_enabled":true,` +
		// A new deployment is resident (never yields, no auto sleep) and moves a
		// replica off a node offline for ten minutes.
		`"node_replacement":{"after_sec":600},"yield_policy":{"policy":"never"},"sleep_held":false,"sleep_wakes_on_request":false,`
	// It cannot sleep (sleepable false), so it may hold no yield policy and
	// its schedule orders no sleep or wake; a single-node deployment's replica
	// count may be scheduled, a multi-node one placed by hand may not.
	const notSleepable = `"wake_wait_limit_sec":1800,"yield_policy_eligible":{"ok":false,"reason":"SLEEP_NOT_ENABLED"},` +
		`"schedule_capabilities":{"sleep":{"ok":false,"reason":"SLEEP_NOT_ENABLED"},"wake":{"ok":false,"reason":"SLEEP_NOT_ENABLED"},"replicas":`
	const single = notSleepable + `[{"minimum":1,"maximum":1024,"ok":true}]},"health_checkable":false,`
	const multi = notSleepable + `[{"minimum":1,"maximum":1024,"ok":false,"reason":"SCHEDULE_REPLICAS_MANUAL_PLACEMENT"}]},"health_checkable":false,`
	// A multi-node deployment carries who places it; a single-node one has
	// no placement_mode key.
	const manual = `"placement_mode":"manual",`
	for _, tc := range []struct {
		name         string
		nodeIDs      string
		nnodes       int
		startTimeout int
		want         string
	}{
		{
			name: "single node, empty column",
			want: head + tail + ready + health + single + `"node_rank":0,"created_at":"2026-10-01T00:00:00Z","model_progress":[` + progressA + `],"placement_status":{"unscheduled":1}}`,
		},
		{
			name: "distributed", nodeIDs: "[1,2]", nnodes: 2,
			want: head + `"node_uuids":["node-a","node-b"],"node_names":["node-a","node-b"],` + tail + ready + health + multi + manual +
				`"nnodes":2,"node_rank":0,"created_at":"2026-10-01T00:00:00Z","model_progress":[` + progressA + `,` + progressB + `],` +
				`"units":[{"node_uuid":"node-a","node_name":"node-a","followers":[{"node_uuid":"node-b","node_name":"node-b"}]}]}`,
		},
		{
			name: "empty array", nodeIDs: "[]",
			want: head + tail + ready + health + single + `"node_rank":0,"created_at":"2026-10-01T00:00:00Z","model_progress":[` + progressA + `],"placement_status":{"unscheduled":1}}`,
		},
		{
			name: "null", nodeIDs: "null",
			want: head + tail + ready + health + single + `"node_rank":0,"created_at":"2026-10-01T00:00:00Z","model_progress":[` + progressA + `],"placement_status":{"unscheduled":1}}`,
		},
		{
			// A deployment that states its own instance health budget carries it;
			// the ones above state none and have no start_timeout_sec key.
			name: "own start timeout", startTimeout: 3600,
			want: head + strings.TrimSuffix(tail, ",") + `,"start_timeout_sec":3600,` + ready + health + single + `"node_rank":0,"created_at":"2026-10-01T00:00:00Z","model_progress":[` + progressA + `],"placement_status":{"unscheduled":1}}`,
		},
		{
			name: "unreadable column", nodeIDs: "not json", nnodes: 2,
			want: head + tail + ready + health + multi + manual + `"nnodes":2,"node_rank":0,"created_at":"2026-10-01T00:00:00Z","model_progress":[` + progressA + `]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(":memory:", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			a, err := st.RegisterNode("node-a", store.HashToken("a"), "node-a")
			if err != nil {
				t.Fatal(err)
			}
			b, err := st.RegisterNode("node-b", store.HashToken("b"), "node-b")
			if err != nil {
				t.Fatal(err)
			}
			if a.ID != 1 || b.ID != 2 {
				t.Fatalf("node ids = %d, %d; the literal node_ids assume 1, 2", a.ID, b.ID)
			}
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "org/m"})
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range []*store.Node{a, b} {
				if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/m"}); err != nil {
					t.Fatal(err)
				}
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: "dep", ModelID: model.ID, NodeID: a.ID, NodeIDs: tc.nodeIDs, NNodes: tc.nnodes, StartTimeoutSec: tc.startTimeout})
			if err != nil {
				t.Fatal(err)
			}
			dep.CreatedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			got, err := json.Marshal((&apiHandlers{d: Deps{Store: st}}).deploymentDTO(dep, false))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("deployment wire\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// A replica's weights copied from another node say which node they come from,
// as the model page does; downloaded ones say nothing about a node.
func TestDeploymentModelProgressNamesThePeerItCopiesFrom(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	holder, _ := st.RegisterNode("holder", store.HashToken("h"), "holder")
	target, _ := st.RegisterNode("target", store.HashToken("t"), "target")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "org/m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: target.ID, State: proto.MFSyncing, Path: "/m",
		Acquisition: proto.ModelAcquirePeer, TransferSourceNodeID: holder.ID}); err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "dep", ModelID: model.ID, NodeID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	progress := (&apiHandlers{d: Deps{Store: st}}).deploymentDTO(dep, false).ModelProgress
	if len(progress) != 1 || progress[0].Acquisition != proto.ModelAcquirePeer || progress[0].SourceNodeUUID != "holder" || progress[0].SourceNodeName != "holder" {
		t.Fatalf("model progress = %+v, want the copy's source node", progress)
	}
}
