package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/planning"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestScaleReplicasOfASingleNodeDeployment(t *testing.T) {
	h, st, plain, _ := replicaFixture(t)
	rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, plain.ID, `{"replicas":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grow = %d %s", rec.Code, rec.Body.String())
	}
	var out dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Replicas != 3 {
		t.Fatalf("grow body = %s", rec.Body.String())
	}
	// The response carries the bound the count is checked against, so the
	// UI's editor needs no copy of it.
	var wire map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil || wire["max_replicas"] != float64(1024) {
		t.Fatalf("max_replicas on the wire = %v (%v)", wire["max_replicas"], err)
	}
	// Shrinking by two, naming one: that one drains, the other is the
	// controller's pick.
	if rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, plain.ID, `{"replicas":1,"remove":["plain-1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}
	if dep := mustDeployment(t, st, plain.ID); dep.Replicas != 1 {
		t.Fatalf("replicas = %d", dep.Replicas)
	}
	if got := instanceStates(t, st, "plain-0", "plain-1"); got["plain-1"] != proto.InstDraining || got["plain-0"] != proto.InstRunning {
		t.Fatalf("states = %v", got)
	}
	// Changing a stopped deployment's count is its configuration.
	if err := st.SetDeploymentState(plain.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	if rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, plain.ID, `{"replicas":2}`); rec.Code != http.StatusOK || mustDeployment(t, st, plain.ID).Replicas != 2 {
		t.Fatalf("stopped = %d %s", rec.Code, rec.Body.String())
	}
}

func TestScaleReplicasRefusals(t *testing.T) {
	h, st, plain, multi := replicaFixture(t)
	pd, err := st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	var pdID uint
	for _, dep := range pd {
		if strings.Contains(dep.Placement, `"pd"`) {
			pdID = dep.ID
		}
	}
	cases := []struct {
		name, body string
		id         uint
		status     int
		code       string
	}{
		{name: "unknown deployment", id: 9999, body: `{"replicas":1}`, status: http.StatusNotFound},
		{name: "unreadable body", id: plain.ID, body: `{"replicas":`, status: http.StatusBadRequest},
		{name: "unknown field", id: plain.ID, body: `{"replicas":1,"force":true}`, status: http.StatusBadRequest},
		{name: "units for single-node", id: plain.ID, body: `{"units":[{"node_uuid":"pd-a"}]}`, status: http.StatusBadRequest, code: ErrorReplicasBodyShape},
		{name: "PD", id: pdID, body: `{"replicas":2}`, status: http.StatusBadRequest, code: ErrorReplicasBodyShape},
		{name: "multi-node", id: multi.ID, body: `{"replicas":2}`, status: http.StatusBadRequest, code: ErrorReplicasBodyShape},
		{name: "no count", id: plain.ID, body: `{}`, status: http.StatusBadRequest, code: ErrorReplicasTooFew},
		{name: "zero", id: plain.ID, body: `{"replicas":0}`, status: http.StatusBadRequest, code: ErrorReplicasTooFew},
		{name: "too many", id: plain.ID, body: `{"replicas":1025}`, status: http.StatusBadRequest, code: ErrorReplicasTooMany},
		{name: "remove while growing", id: plain.ID, body: `{"replicas":3,"remove":["plain-0"]}`, status: http.StatusBadRequest, code: ErrorReplicasRemoveTooMany},
		{name: "remove more than shrunk", id: plain.ID, body: `{"replicas":1,"remove":["plain-0","plain-1"]}`, status: http.StatusBadRequest, code: ErrorReplicasRemoveTooMany},
		{name: "remove a failed attempt", id: plain.ID, body: `{"replicas":1,"remove":["plain-failed"]}`, status: http.StatusBadRequest, code: ErrorReplicasRemoveInvalid},
		{name: "remove another deployment's", id: plain.ID, body: `{"replicas":1,"remove":["multi-leader"]}`, status: http.StatusBadRequest, code: ErrorReplicasRemoveInvalid},
	}
	for _, tc := range cases {
		rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, tc.id, tc.body)
		if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`)) {
			t.Fatalf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if err := st.SetDeploymentState(plain.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	if rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, plain.ID, `{"replicas":1}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorDeploymentDeleting) {
		t.Fatalf("deleting = %d %s", rec.Code, rec.Body.String())
	}
	if dep := mustDeployment(t, st, plain.ID); dep.Replicas != 2 {
		t.Fatalf("a refused scale changed replicas to %d", dep.Replicas)
	}
	if got := instanceStates(t, st, "plain-0", "plain-1"); got["plain-0"] != proto.InstRunning || got["plain-1"] != proto.InstRunning {
		t.Fatalf("a refused scale drained: %v", got)
	}
}

// A multi-node deployment lists its replicas: each spans the rank count on
// distinct nodes, and no two share a leader node.
func TestScaleReplicasOfAMultiNodeDeployment(t *testing.T) {
	h, st, _, multi := replicaFixture(t)
	body := `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]},{"node_uuid":"pd-b","followers":[{"node_uuid":"pd-a"}]}]}`
	rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, multi.ID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("list replicas = %d %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{`"replicas":2`, `"units":[{"node_uuid":"pd-a","node_name":"pd-a","followers":[{"node_uuid":"pd-b","node_name":"pd-b"}]},{"node_uuid":"pd-b","node_name":"pd-b","followers":[{"node_uuid":"pd-a","node_name":"pd-a"}]}]`} {
		if !strings.Contains(rec.Body.String(), key) {
			t.Fatalf("response lacks %s: %s", key, rec.Body.String())
		}
	}
	dep := mustDeployment(t, st, multi.ID)
	nodeA, _ := st.NodeByUUID("pd-a")
	nodeB, _ := st.NodeByUUID("pd-b")
	if dep.Replicas != 2 || dep.NodeID != nodeA.ID || dep.NodeIDs != fmt.Sprintf("[%d,%d]", nodeA.ID, nodeB.ID) || !strings.Contains(dep.Placement, `"units":[`) {
		t.Fatalf("deployment = replicas %d node %d nodes %s placement %s", dep.Replicas, dep.NodeID, dep.NodeIDs, dep.Placement)
	}
	// Back to one replica: the list is replaced, the second one will drain.
	if rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, multi.ID, `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]}]}`); rec.Code != http.StatusOK || mustDeployment(t, st, multi.ID).Replicas != 1 {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name, body string
		code       string
	}{
		{"a count", `{"replicas":2}`, ErrorReplicasBodyShape},
		{"an empty list", `{"units":[]}`, ErrorReplicasBodyShape},
		{"too few ranks", `{"units":[{"node_uuid":"pd-a"}]}`, ErrorReplicaUnitRanks},
		{"too many ranks", `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"},{"node_uuid":"pd-b"}]}]}`, ErrorReplicaUnitRanks},
		{"an unknown node", `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"nope"}]}]}`, ErrorReplicaUnitNode},
		{"one node twice", `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-a"}]}]}`, ErrorReplicaUnitNode},
		{"pinned GPUs", `{"units":[{"node_uuid":"pd-a","gpus":[0],"followers":[{"node_uuid":"pd-b"}]}]}`, ErrorReplicaUnitGPUs},
		{"a shared leader", `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]},{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]}]}`, ErrorReplicaLeaderShared},
		{"too many replicas", `{"units":[` + strings.Repeat(`{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]},`, planning.MaxReplicas) + `{"node_uuid":"pd-b","followers":[{"node_uuid":"pd-a"}]}]}`, ErrorReplicasTooMany},
	}
	for _, tc := range cases {
		rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, multi.ID, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("%s = %d %s, want %s", tc.name, rec.Code, rec.Body.String(), tc.code)
		}
	}
	if dep := mustDeployment(t, st, multi.ID); dep.Replicas != 1 {
		t.Fatalf("a refused list changed replicas to %d", dep.Replicas)
	}
}

// The list is checked against the nodes as they are: a replica it adds or
// moves runs on online nodes, and the ranks listed on a node, whichever
// replicas they belong to, fit within its GPUs. A replica the deployment
// already lists is kept as it is, so the list can still change while one of
// its nodes is down.
func TestScaleReplicasChecksTheNodesTheListNames(t *testing.T) {
	h, st, _, multi := replicaFixture(t)
	nodeA, _ := st.NodeByUUID("pd-a")
	nodeB, _ := st.NodeByUUID("pd-b")
	// TP 4 over two ranks puts two GPUs on each rank's node; pd-a and pd-b
	// have two each, so one replica fits and a second leads onto a full node.
	wide, err := st.CreateDeployment(&store.Deployment{Name: "wide", ServedName: "wide", State: "active", NodeID: nodeA.ID, NodeIDs: fmt.Sprintf("[%d,%d]", nodeA.ID, nodeB.ID), NNodes: 2, Replicas: 1, Placement: `{"tp":4,"pp":1}`})
	if err != nil {
		t.Fatal(err)
	}
	two := `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]},{"node_uuid":"pd-b","followers":[{"node_uuid":"pd-a"}]}]}`
	rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, wide.ID, two)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorReplicaUnitGPUsInsufficient+`"`) || !strings.Contains(rec.Body.String(), `"required":4`) {
		t.Fatalf("two wide replicas on two nodes = %d %s", rec.Code, rec.Body.String())
	}
	if err := st.SetNodeState("pd-b", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	// The deployment's own replica (pd-a then pd-b) stays listed although
	// pd-b is down.
	rec = pdOpsRequest(h.scaleReplicas, http.MethodPut, multi.ID, `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("keeping a replica whose node is down = %d %s", rec.Code, rec.Body.String())
	}
	// Moving it, or adding one, onto the offline node is refused.
	for name, body := range map[string]string{
		"moved": `{"units":[{"node_uuid":"pd-b","followers":[{"node_uuid":"pd-a"}]}]}`,
		"added": `{"units":[{"node_uuid":"pd-a","followers":[{"node_uuid":"pd-b"}]},{"node_uuid":"pd-b","followers":[{"node_uuid":"pd-a"}]}]}`,
	} {
		rec = pdOpsRequest(h.scaleReplicas, http.MethodPut, multi.ID, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorReplicaUnitNodeOffline+`"`) {
			t.Fatalf("a %s replica on an offline node = %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// A growing single-node deployment may name where its new replicas first
// start; the names are checked before anything is written.
func TestScaleReplicasPlaceOnRefusals(t *testing.T) {
	h, st, plain, multi := replicaFixture(t)
	for _, tc := range []struct {
		name, body string
		depID      uint
		status     int
		code       string
	}{
		{"more nodes than replicas added", `{"replicas":3,"place_on":["pd-a","pd-b"]}`, plain.ID, http.StatusBadRequest, ErrorReplicasPlaceOnTooMany},
		{"naming nodes while shrinking", `{"replicas":1,"place_on":["pd-a"]}`, plain.ID, http.StatusBadRequest, ErrorReplicasPlaceOnTooMany},
		{"a multi-node deployment", `{"replicas":2,"place_on":["pd-a"]}`, multi.ID, http.StatusBadRequest, ErrorReplicasBodyShape},
		{"an unknown node", `{"replicas":3,"place_on":["no-such-node"]}`, plain.ID, http.StatusConflict, ErrorReplicasPlaceOnRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, tc.depID, tc.body)
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tc.status || body["code"] != tc.code {
				t.Fatalf("PUT %s = %d %s, want %d %s", tc.body, rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			if dep := mustDeployment(t, st, plain.ID); dep.Replicas != 2 {
				t.Fatalf("a refused request changed the count to %d", dep.Replicas)
			}
		})
	}
	// An offline node is named in the refusal with its reason.
	if err := st.SetNodeState("pd-b", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	rec := pdOpsRequest(h.scaleReplicas, http.MethodPut, plain.ID, `{"replicas":3,"place_on":["pd-b"]}`)
	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body.Code != ErrorReplicasPlaceOnRefused || body.Params["reason"] != "offline" {
		t.Fatalf("offline node = %d %s", rec.Code, rec.Body.String())
	}
}
