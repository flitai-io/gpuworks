package proxy

import (
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Freeze a previously healthy route snapshot: state reports must stop traffic
// without waiting for its asynchronous rebuild, including retry candidates.
func TestCachedRouteRejectsKnownNonRunningInstance(t *testing.T) {
	for _, state := range []string{proto.InstFailed, proto.InstStarting, proto.InstRemoving, proto.InstStopped, "deleted", "port changed", "headless"} {
		t.Run(state, func(t *testing.T) {
			st, err := store.Open("file:route_recovery_"+state+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
			if err != nil {
				t.Fatal(err)
			}
			node, err := st.RegisterNode("recovery-node", store.HashToken("recovery-node"), "recovery-node")
			if err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "model", Engine: "callable", State: store.DeployActive})
			if err != nil {
				t.Fatal(err)
			}
			inst, err := st.CreateInstance(&store.Instance{UUID: "recovery-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001})
			if err != nil {
				t.Fatal(err)
			}
			p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{node.UUID: true}}, nil, 0)
			p.Close() // Retain the running snapshot, stop its refresh subscription.
			switch state {
			case "deleted":
				err = st.DeleteInstance(inst.UUID)
			case "port changed":
				err = updateInstanceRow(st, inst.UUID, map[string]any{"api_port": 30002})
			case "headless":
				err = updateInstanceRow(st, inst.UUID, map[string]any{"headless": true})
			case proto.InstFailed:
				err = st.SetInstanceFailed(inst.UUID, proto.RuntimeFailure{Code: proto.ErrorContainerExited}, "engine exited; recovery pending")
			default:
				err = st.SetInstanceState(inst.UUID, state, "engine exited; recovery pending")
			}
			if err != nil {
				t.Fatal(err)
			}
			_, tgt, status, message, _ := p.selectStatus("model")
			if state == "port changed" {
				if status != 0 || tgt.port != 30002 {
					t.Fatalf("changed port not refreshed: status=%d target=%+v", status, tgt)
				}
				tgt.lease.release(outcomeCanceled)
			} else if status != http.StatusServiceUnavailable || tgt.instanceUUID != "" {
				t.Fatalf("known %s routed to %+v: status=%d message=%q", state, tgt, status, message)
			}
			rows, _ := p.routes.snapshot("model")
			attempt := &routeAttempt{candidates: rows[0].candidates, tried: map[string]bool{}, modelName: "model"}
			retry := &retryTransport{balancer: p.balancer, hub: p.hub, awake: p.awake}
			if next := retry.pickNext(attempt); next != nil {
				next.release(outcomeCanceled)
				t.Fatal("retry selected known non-running instance")
			}
			switch state {
			case "deleted":
				inst.ID = 0
				_, err = st.CreateInstance(inst)
			case "port changed":
				err = updateInstanceRow(st, inst.UUID, map[string]any{"api_port": 30001})
			case "headless":
				err = updateInstanceRow(st, inst.UUID, map[string]any{"headless": false})
			default:
				err = st.SetInstanceState(inst.UUID, proto.InstRunning, "engine healthy")
			}
			if err != nil {
				t.Fatal(err)
			}
			_, tgt, status, message, _ = p.selectStatus("model")
			if status != 0 || tgt.instanceUUID != inst.UUID {
				t.Fatalf("recovered instance not routed: status=%d message=%q", status, message)
			}
			tgt.lease.release(outcomeCanceled)
		})
	}
}
