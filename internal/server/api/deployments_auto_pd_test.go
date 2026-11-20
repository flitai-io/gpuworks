package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// pdNodes are the node of every unit of a role, in order.
func pdNodes(t *testing.T, out dto.Deployment, role string) []string {
	t.Helper()
	if out.PD == nil {
		t.Fatalf("no pd section: %+v", out)
	}
	for _, r := range out.PD.Roles {
		if r.Role == role {
			var nodes []string
			for _, u := range r.Units {
				nodes = append(nodes, u.NodeUUID)
			}
			return nodes
		}
	}
	t.Fatalf("no %s role in %+v", role, out.PD.Roles)
	return nil
}

// An automatic 1P1D whose units name no node gets one on a node with free
// cards: binpack puts both on the one node with two free cards (au-1 is
// full), spread puts them on different nodes. No card is pinned, and the
// deployment is stored as placed by the platform.
func TestCreateAutoPDPicksTheUnits(t *testing.T) {
	r := newAutoRig(t, 3)
	r.occupy(t, "au-1", 0, 1)

	out := r.mustCreate(t, r.pdAutoBody("pd-packed", `{}`, `{}`, ""))
	if out.PlacementMode != "auto" || out.PlacementStrategy != "binpack" || out.NNodes > 1 {
		t.Fatalf("created = mode %q strategy %q nnodes %d", out.PlacementMode, out.PlacementStrategy, out.NNodes)
	}
	if got := pdNodes(t, out, "prefill"); len(got) != 1 || got[0] != "au-2" {
		t.Fatalf("prefill on %v, want au-2 (au-1 is full, binpack fills the lowest node)", got)
	}
	if got := pdNodes(t, out, "decode"); len(got) != 1 || got[0] != "au-2" {
		t.Fatalf("decode on %v, want au-2: binpack keeps 1P1D on the one node with two free cards", got)
	}
	dep := r.storedDeployment(t, out.ID)
	if !dep.UnitsAuto {
		t.Fatal("the PD deployment is not stored as placed by the platform")
	}
	if strings.Contains(dep.Placement, `"gpus"`) || strings.Contains(dep.Placement, "auto") || !strings.Contains(dep.Placement, `"node_uuid":"au-2"`) {
		t.Fatalf("stored placement = %s, want nodes filled in and no pinned cards", dep.Placement)
	}
	if rec := r.do(r.session, http.MethodGet, "/api/v1/deployments/"+jsonOfUint(out.ID), ""); !strings.Contains(rec.Body.String(), `"placement_mode":"auto"`) {
		t.Fatalf("GET lacks placement_mode auto: %s", rec.Body)
	}

	spread := r.mustCreate(t, r.pdAutoBody("pd-spread", `{}`, `{}`, `,"placement_strategy":"spread"`))
	p, d := pdNodes(t, spread, "prefill"), pdNodes(t, spread, "decode")
	if spread.PlacementStrategy != "spread" || len(p) != 1 || len(d) != 1 || p[0] == d[0] || p[0] == "au-1" || d[0] == "au-1" {
		t.Fatalf("spread put prefill on %v and decode on %v, want two different nodes with room", p, d)
	}
}

// Units that name a node keep it and the others are placed beside them,
// leaving the named unit's card alone: three one-card units never share
// a two-card node more than twice. (The named unit's card used to go
// uncounted, so a node could be given a third unit and the create failed in
// the node check with PD_NODE_GPUS_INSUFFICIENT.)
func TestCreateAutoPDKeepsTheNamedUnits(t *testing.T) {
	r := newAutoRig(t, 3)
	out := r.mustCreate(t, r.pdAutoBody("pd-mixed", `{"node_uuid":"au-3"},{}`, `{}`, ""))
	prefill, decode := pdNodes(t, out, "prefill"), pdNodes(t, out, "decode")
	if out.PlacementMode != "auto" || len(prefill) != 2 || prefill[0] != "au-3" || prefill[1] == "" || len(decode) != 1 || decode[0] == "" {
		t.Fatalf("prefill on %v decode on %v mode %q, want au-3 kept first and every unit placed", prefill, decode, out.PlacementMode)
	}
	perNode := map[string]int{}
	for _, node := range append(prefill, decode...) {
		perNode[node]++
	}
	for node, units := range perNode {
		if units > 2 {
			t.Fatalf("%s was given %d one-card units on 2 cards: %v", node, units, perNode)
		}
	}
	// Binpack fills the node the named unit is on before opening another.
	if perNode["au-3"] != 2 {
		t.Fatalf("units per node = %v, want au-3 filled with two", perNode)
	}
}

func TestCreateAutoPDRefusals(t *testing.T) {
	r := newAutoRig(t, 2)
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
		role       string
	}{
		{"a prefill unit with followers", r.pdAutoBody("f1", `{"followers":[{}]}`, `{}`, ""), http.StatusBadRequest, ErrorAutoPlacementPDMultiNode, "prefill"},
		{"a decode unit with followers", r.pdAutoBody("f2", `{}`, `{"followers":[{"node_uuid":"au-2"}]}`, ""), http.StatusBadRequest, ErrorAutoPlacementPDMultiNode, "decode"},
		{"more units than cards", r.pdAutoBody("big", `{},{},{}`, `{},{}`, ""), http.StatusConflict, ErrorUnitPlacementNone, ""},
		{"nnodes beside the PD section", r.pdAutoBody("shape", `{}`, `{}`, `,"nnodes":2`), http.StatusBadRequest, "PD_SHAPE_CONFLICT", ""},
		{"node_uuids beside the PD section", r.pdAutoBody("shape2", `{}`, `{}`, `,"node_uuids":["au-1"]`), http.StatusBadRequest, "PD_SHAPE_CONFLICT", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", tc.body)
			code, params := errorBody(t, rec.Body.String())
			if rec.Code != tc.status || code != tc.code {
				t.Fatalf("create = %d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			if tc.role != "" && params["role"] != tc.role {
				t.Fatalf("params = %v, want role %s", params, tc.role)
			}
		})
	}
	if n := r.deployments(t); n != 0 {
		t.Fatalf("refused creates left %d deployments", n)
	}
}

func TestCreateAutoPDWithNoRoomCountsWhatIsLeftOut(t *testing.T) {
	r := newAutoRig(t, 2)
	r.occupy(t, "au-1", 0, 1)
	r.occupy(t, "au-2", 0, 1)
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", r.pdAutoBody("pd-none", `{}`, `{}`, ""))
	code, params := errorBody(t, rec.Body.String())
	if rec.Code != http.StatusConflict || code != ErrorUnitPlacementNone || params["nodes"] != float64(2) || params["busy"] != float64(2) {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
}

// A PD request that pins its own nodes and cards is the operator's: it is
// stored as manual with the pinned cards kept.
func TestCreateManualPDKeepsPinnedCards(t *testing.T) {
	r := newAutoRig(t, 2)
	body := strings.Replace(r.pdAutoBody("pd-manual", `{"node_uuid":"au-1","gpus":[1]}`, `{"node_uuid":"au-2"}`, ""), `"placement_mode":"auto",`, ``, 1)
	out := r.mustCreate(t, body)
	if out.PlacementMode != "manual" || r.storedDeployment(t, out.ID).UnitsAuto {
		t.Fatalf("manual PD = mode %q", out.PlacementMode)
	}
	if !strings.Contains(r.storedDeployment(t, out.ID).Placement, `"gpus":[1]`) {
		t.Fatalf("pinned card lost: %s", r.storedDeployment(t, out.ID).Placement)
	}
	// Without a mode word a unit that names no node is still the operator's
	// omission, refused as before.
	rec := r.do(r.session, http.MethodPost, "/api/v1/deployments", strings.Replace(r.pdAutoBody("pd-bare", `{}`, `{"node_uuid":"au-2"}`, ""), `"placement_mode":"auto",`, ``, 1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a manual PD unit without a node = %d %s, want 400", rec.Code, rec.Body)
	}
}

func autoEvaluate(t *testing.T, r *autoRig, body string) (int, map[string]json.RawMessage, dto.EvaluateResponse) {
	t.Helper()
	rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", body)
	var raw map[string]json.RawMessage
	var resp dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("evaluate body %q: %v", rec.Body, err)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, raw, resp
}

// Evaluate previews an automatic multi-node request on the nodes the create
// would pick now, and says so.
func TestEvaluateAutoMultiNodePreviewsThePickedNodes(t *testing.T) {
	r := newAutoRig(t, 4)
	r.occupy(t, "au-3", 0)
	status, raw, resp := autoEvaluate(t, r, r.multiBody("preview", ""))
	if status != http.StatusOK || !resp.OK || !resp.AutoPlaced || string(raw["auto_placed"]) != "true" {
		t.Fatalf("evaluate = %d ok %v auto_placed %s", status, resp.OK, raw["auto_placed"])
	}
	// Automatic node placement keeps its own default PP when only TP was sent.
	if resp.FormValues["pp"] != float64(1) {
		t.Fatalf("automatic node placement lost its PP default: %v", resp.FormValues)
	}
	if len(resp.RankCommands) != 2 || resp.RankCommands[0].NodeUUID != "au-3" || resp.RankCommands[1].NodeUUID != "au-1" || resp.RankCommands[0].Role != "leader" {
		t.Fatalf("rank commands = %+v, want au-3 leading and au-1 following", resp.RankCommands)
	}
	if resp.Acquisition == nil || len(resp.Acquisition.Nodes) != 2 || resp.Acquisition.Nodes[0].NodeUUID != "au-3" {
		t.Fatalf("acquisition = %+v", resp.Acquisition)
	}
	if n := r.deployments(t); n != 1 { // the holder only
		t.Fatalf("evaluate created a deployment: %d", n)
	}
	// What was previewed is what a create now does.
	if out := r.mustCreate(t, r.multiBody("preview", "")); out.NodeUUIDs[0] != "au-3" || out.NodeUUIDs[1] != "au-1" {
		t.Fatalf("created on %v, evaluated on au-3 and au-1", out.NodeUUIDs)
	}
	// A named node leads the preview.
	_, _, named := autoEvaluate(t, r, r.multiBody("preview2", `,"node_uuids":["au-4"]`))
	if !named.AutoPlaced || len(named.RankCommands) != 2 || named.RankCommands[0].NodeUUID != "au-4" {
		t.Fatalf("named preview = %+v", named.RankCommands)
	}
}

func TestEvaluateAutoPDPreviewsThePickedNodes(t *testing.T) {
	r := newAutoRig(t, 3)
	r.occupy(t, "au-1", 0, 1)
	status, _, resp := autoEvaluate(t, r, r.pdAutoBody("pd-preview", `{}`, `{}`, ""))
	if status != http.StatusOK || !resp.OK || !resp.AutoPlaced || resp.PD == nil || len(resp.PD.Roles) != 2 {
		t.Fatalf("evaluate = %d ok %v auto %v pd %+v", status, resp.OK, resp.AutoPlaced, resp.PD)
	}
	for _, role := range resp.PD.Roles {
		if len(role.Units) != 1 || role.Units[0].NodeUUID != "au-2" || !role.Units[0].Fits {
			t.Fatalf("%s preview = %+v, want one unit that fits on au-2", role.Role, role.Units)
		}
	}
	// A manual PD preview does not claim the server picked anything.
	manual := strings.Replace(r.pdAutoBody("pd-man", `{"node_uuid":"au-2"}`, `{"node_uuid":"au-3"}`, ""), `"placement_mode":"auto",`, ``, 1)
	status, raw, _ := autoEvaluate(t, r, manual)
	if _, present := raw["auto_placed"]; status != http.StatusOK || present {
		t.Fatalf("manual PD evaluate = %d auto_placed present %v", status, present)
	}
}

// Evaluate answers an automatic failure the way the form wants it: HTTP 200,
// ok false, the error on placement_mode.
func TestEvaluateAutoFailuresAreFieldErrors(t *testing.T) {
	r := newAutoRig(t, 2)
	noTP := strings.Replace(r.multiBody("no-tp", ""), `,"placement":{"tp":2}`, "", 1)
	status, raw, resp := autoEvaluate(t, r, noTP)
	errs := fieldErrorsOf(t, raw)
	if status != http.StatusOK || resp.OK || !resp.AutoPlaced || len(errs) != 1 || errs[0].Field != "placement_mode" || errs[0].Code != ErrorAutoPlacementTPRequired {
		t.Fatalf("no tp = %d ok %v errors %+v", status, resp.OK, errs)
	}

	r.occupy(t, "au-1", 0, 1)
	status, raw, resp = autoEvaluate(t, r, r.multiBody("none", ""))
	errs = fieldErrorsOf(t, raw)
	if status != http.StatusOK || resp.OK || !resp.AutoPlaced || len(errs) != 1 || errs[0].Field != "placement_mode" || errs[0].Code != ErrorUnitPlacementNone {
		t.Fatalf("nothing fits = %d ok %v errors %+v", status, resp.OK, errs)
	}
	if errs[0].Params["busy"] != float64(1) || errs[0].Params["nodes"] != float64(2) || errs[0].Params["largest"] != float64(1) {
		t.Fatalf("params = %v", errs[0].Params)
	}

	status, raw, _ = autoEvaluate(t, r, r.pdAutoBody("pd-follower", `{"followers":[{}]}`, `{}`, ""))
	errs = fieldErrorsOf(t, raw)
	if status != http.StatusOK || len(errs) != 1 || errs[0].Field != "placement_mode" || errs[0].Code != ErrorAutoPlacementPDMultiNode || errs[0].Params["role"] != "prefill" {
		t.Fatalf("pd follower = %d errors %+v", status, errs)
	}
}

// A request that is not an automatic multi-node or PD one has no auto_placed.
func TestEvaluateWithoutAutoPlacementOmitsAutoPlaced(t *testing.T) {
	r := newAutoRig(t, 2)
	manual := strings.Replace(r.multiBody("manual", `,"node_uuids":["au-1","au-2"]`), `"placement_mode":"auto",`, ``, 1)
	status, raw, resp := autoEvaluate(t, r, manual)
	if _, present := raw["auto_placed"]; status != http.StatusOK || !resp.OK || present {
		t.Fatalf("manual multi-node evaluate = %d ok %v auto_placed present %v", status, resp.OK, present)
	}
	single := `{"model_id":` + jsonOfUint(r.modelID) + `,"engine":"vllm","node_uuid":"au-1","placement_mode":"auto"}`
	status, raw, _ = autoEvaluate(t, r, single)
	if _, present := raw["auto_placed"]; status != http.StatusOK || present {
		t.Fatalf("single-node auto evaluate = %d auto_placed present %v", status, present)
	}
}
