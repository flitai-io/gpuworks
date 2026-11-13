package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/planning"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func (r *autoRig) deploymentNodes(t *testing.T, id uint) [][]string {
	t.Helper()
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", id), "")
	var out dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s %v", rec.Code, rec.Body, err)
	}
	return replicaNodes(out)
}

// The scheduling automation sets a replica count through the same path as
// PUT /deployments/{id}/replicas: an automatically placed multi-node
// deployment gets its new replica's nodes picked as the API picks them, and
// every refusal reaches the caller as a product error with its code.
func TestReplicaScalerPlacesAnAutomaticMultiNodeDeployment(t *testing.T) {
	r := newAutoRig(t, 4)
	scale := (&apiHandlers{d: Deps{Store: r.st}}).scaling().ScaleScheduled
	dep := r.mustCreate(t, r.multiBody("scaled", ""))

	if err := scale(dep.ID, "", 2); err != nil {
		t.Fatal(err)
	}
	// The same nodes as TestScaleAutoMultiNodeGrowsByCount's request gets.
	nodes := r.deploymentNodes(t, dep.ID)
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 2 || !stored.UnitsAuto || len(nodes) != 2 ||
		!slices.Equal(nodes[0], []string{"au-1", "au-2"}) || !slices.Equal(nodes[1], []string{"au-2", "au-1"}) {
		t.Fatalf("after growing to 2: replicas %d, nodes %v", stored.Replicas, nodes)
	}
	// The same count again changes nothing, and a count over the limit is the limit.
	if err := scale(dep.ID, "", 2); err != nil || len(r.deploymentNodes(t, dep.ID)) != 2 {
		t.Fatalf("same count: %v", err)
	}
	if err := scale(dep.ID, "", 1); err != nil {
		t.Fatal(err)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 || len(r.deploymentNodes(t, dep.ID)) != 1 {
		t.Fatalf("after shrinking to 1: replicas %d", stored.Replicas)
	}

	// No room for another replica: the platform's own placement refusal.
	r.occupy(t, "au-1", 1)
	r.occupy(t, "au-2", 1)
	r.occupy(t, "au-3", 0, 1)
	r.occupy(t, "au-4", 0, 1)
	err := scale(dep.ID, "", 2)
	if err == nil || productErrorCode(t, err) != ErrorUnitPlacementNone {
		t.Fatalf("no room: err = %v, want %s", err, ErrorUnitPlacementNone)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 {
		t.Fatalf("replicas = %d after a refused grow", stored.Replicas)
	}

	// Switched to placement by hand since the schedule was written: the order
	// is refused with the cause, nothing changes.
	if rec := r.do(r.session, http.MethodPut, schedPath(r.storedDeployment(t, dep.ID), "placement-mode"), `{"mode":"manual"}`); rec.Code != http.StatusOK {
		t.Fatalf("placement mode = %d %s", rec.Code, rec.Body)
	}
	if err := scale(dep.ID, "", 2); err == nil || productErrorCode(t, err) != "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" {
		t.Fatalf("manual placement: err = %v", err)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 {
		t.Fatalf("replicas = %d after a refused order", stored.Replicas)
	}
}

func TestReplicaScalerSingleNodeAndPD(t *testing.T) {
	rig := newScheduleRig(t)
	scale := (&apiHandlers{d: Deps{Store: rig.st}}).scaling().ScaleScheduled
	if err := scale(rig.plain.ID, "", 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := rig.st.GetDeployment(rig.plain.ID); got.Replicas != 3 {
		t.Fatalf("replicas = %d, want 3", got.Replicas)
	}
	if err := scale(rig.plain.ID, "", planning.MaxReplicas+67); err != nil {
		t.Fatal(err)
	}
	if got, _ := rig.st.GetDeployment(rig.plain.ID); got.Replicas != planning.MaxReplicas {
		t.Fatalf("replicas = %d, want the limit %d", got.Replicas, planning.MaxReplicas)
	}
	if err := scale(rig.pd.ID, "", 2); err == nil || productErrorCode(t, err) != "SCHEDULE_INVALID" {
		t.Fatalf("PD without a role: err = %v", err)
	}
	if err := scale(rig.plain.ID, "decode", 2); err == nil || productErrorCode(t, err) != "SCHEDULE_INVALID" {
		t.Fatalf("a role of a deployment that is not PD: err = %v", err)
	}
	if err := scale(rig.pdManual.ID, "decode", 2); err == nil || productErrorCode(t, err) != "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" {
		t.Fatalf("PD placed by hand: err = %v", err)
	}
	if err := scale(rig.multi.ID, "", 2); err == nil || productErrorCode(t, err) != "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" {
		t.Fatalf("multi-node placed by hand: err = %v", err)
	}
	if err := scale(99999, "", 2); err == nil {
		t.Fatal("an unknown deployment is an error")
	}
}

// A scheduled PD role count goes through the PD update (PUT
// /deployments/{id}/pd): a grow gets its nodes picked beside the placed
// units, a shrink drops the newest, the other role is left alone, and the
// refusals are the update's own.
func TestReplicaScalerScalesOnePDRole(t *testing.T) {
	r := newAutoRig(t, 3)
	scale := (&apiHandlers{d: Deps{Store: r.st}}).scaling().ScaleScheduled
	pd := r.mustCreate(t, r.pdAutoBody("pd-sched", `{}`, `{}`, ""))
	before := pdNodes(t, pd, "decode")

	if err := scale(pd.ID, "prefill", 3); err != nil {
		t.Fatal(err)
	}
	got := r.deploymentView(t, pd.ID)
	prefill := pdNodes(t, got, "prefill")
	if len(prefill) != 3 || prefill[0] != pdNodes(t, pd, "prefill")[0] || !slices.Equal(pdNodes(t, got, "decode"), before) {
		t.Fatalf("after growing prefill to 3: prefill %v decode %v (decode was %v)", prefill, pdNodes(t, got, "decode"), before)
	}
	if stored := r.storedDeployment(t, pd.ID); !stored.UnitsAuto || strings.Contains(stored.Placement, `"node_uuid":""`) || strings.Contains(stored.Placement, `"gpus"`) {
		t.Fatalf("stored auto %v placement %s: every new unit has a node and pins no card", stored.UnitsAuto, stored.Placement)
	}

	// The same count changes nothing; a shrink drops the newest units.
	if err := scale(pd.ID, "prefill", 3); err != nil {
		t.Fatal(err)
	}
	if err := scale(pd.ID, "prefill", 1); err != nil {
		t.Fatal(err)
	}
	if after := pdNodes(t, r.deploymentView(t, pd.ID), "prefill"); !slices.Equal(after, prefill[:1]) {
		t.Fatalf("after shrinking to 1: %v, want the oldest unit %v", after, prefill[:1])
	}

	// The mode lets prefill be empty and come back, and not decode.
	if err := scale(pd.ID, "prefill", 0); err != nil {
		t.Fatal(err)
	}
	if n := len(pdNodes(t, r.deploymentView(t, pd.ID), "prefill")); n != 0 {
		t.Fatalf("prefill units = %d after scaling to 0", n)
	}
	if err := scale(pd.ID, "prefill", 2); err != nil {
		t.Fatal(err)
	}
	if n := len(pdNodes(t, r.deploymentView(t, pd.ID), "prefill")); n != 2 {
		t.Fatalf("prefill units = %d after coming back", n)
	}
	err := scale(pd.ID, "decode", 0)
	if err == nil || productErrorCode(t, err) != "PD_ROLE_UNITS_EMPTY" {
		t.Fatalf("decode to 0: err = %v, want PD_ROLE_UNITS_EMPTY", err)
	}
	if n := len(pdNodes(t, r.deploymentView(t, pd.ID), "decode")); n != 1 {
		t.Fatalf("decode units = %d after a refused order", n)
	}
	if err := scale(pd.ID, "decode", 33); err == nil || productErrorCode(t, err) != "PD_ROLE_UNITS_TOO_MANY" {
		t.Fatalf("decode to 33: err = %v", err)
	}
	if err := scale(pd.ID, "encode", 1); err == nil || productErrorCode(t, err) != "SCHEDULE_INVALID" {
		t.Fatalf("unknown role: err = %v", err)
	}
	if err := scale(pd.ID, "", 1); err == nil || productErrorCode(t, err) != "SCHEDULE_INVALID" {
		t.Fatalf("no role: err = %v", err)
	}

	// No room for another unit: the platform's own placement refusal, and the
	// placement stays as it was.
	r.occupy(t, "au-1", 0, 1)
	r.occupy(t, "au-2", 0, 1)
	r.occupy(t, "au-3", 0, 1)
	err = scale(pd.ID, "decode", 20)
	if err == nil || productErrorCode(t, err) != ErrorUnitPlacementNone {
		t.Fatalf("no room: err = %v, want %s", err, ErrorUnitPlacementNone)
	}
	if n := len(pdNodes(t, r.deploymentView(t, pd.ID), "decode")); n != 1 {
		t.Fatalf("decode units = %d after a refused grow", n)
	}

	// Switched to placement by hand since the schedule was written: refused
	// with the cause.
	if rec := r.do(r.session, http.MethodPut, schedPath(r.storedDeployment(t, pd.ID), "placement-mode"), `{"mode":"manual"}`); rec.Code != http.StatusOK {
		t.Fatalf("placement mode = %d %s", rec.Code, rec.Body)
	}
	if err := scale(pd.ID, "decode", 2); err == nil || productErrorCode(t, err) != "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" {
		t.Fatalf("manual placement: err = %v", err)
	}
}

func (r *autoRig) deploymentView(t *testing.T, id uint) dto.Deployment {
	t.Helper()
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", id), "")
	var out dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s %v", rec.Code, rec.Body, err)
	}
	return out
}
