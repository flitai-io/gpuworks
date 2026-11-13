package api

import (
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// putPortInstance adds an instance of a deployment on a node holding the given
// ports, the first being its API port.
func putPortInstance(t *testing.T, st *store.Store, node *store.Node, deployment, uuid, state string, ports ...int) {
	t.Helper()
	d := seedDeployment(t, st, deployment)
	inst := &store.Instance{UUID: uuid, DeploymentID: d.ID, NodeID: node.ID, State: state, APIPort: ports[0], Ports: mustJSONString(t, ports)}
	if _, err := st.CreateInstance(inst); err != nil {
		t.Fatal(err)
	}
}

func mustJSONString(t *testing.T, v any) string { return string(mustJSON(t, v)) }

// Changing engine_ports is allowed whatever the running instances hold: the
// answer warns, exactly, about the instances whose ports fall outside the new
// range, and they keep their ports and state.
func TestEnginePortsEditWarnsAboutInstancesOutsideTheNewRange(t *testing.T) {
	rig := newSelectionRig(t, &selectionAgents{})
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"prod","server_url":"https://gw.example","engine_ports":"40000-40099"}`))
	uuid := created["uuid"].(string)
	base := "/api/v1/kube/clusters/" + uuid
	c := entryOf(t, rig, uuid)
	n1 := seedKubeNode(t, rig.st, c, "gpu-1")
	n2 := seedKubeNode(t, rig.st, c, "gpu-2")
	putPortInstance(t, rig.st, n1, "chat", "in-range", proto.InstRunning, 40050)
	putPortInstance(t, rig.st, n1, "chat", "below", proto.InstRunning, 40010)
	putPortInstance(t, rig.st, n2, "embed", "above", proto.InstStarting, 40090)
	// A PD instance holds a block: only the side channel ports outside the range are named.
	putPortInstance(t, rig.st, n2, "pd", "block", proto.InstRunning, 40060, 40061, 40062, 40063)
	// Finished instances hold nothing; another entry's instances are not this entry's.
	putPortInstance(t, rig.st, n1, "old", "stopped", proto.InstStopped, 40001)
	putPortInstance(t, rig.st, n1, "old", "failed", proto.InstFailed, 40002)
	other, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "other", EnginePorts: "40000-40099"})
	if err != nil {
		t.Fatal(err)
	}
	on := seedKubeNode(t, rig.st, other, "other-1")
	putPortInstance(t, rig.st, on, "elsewhere", "other-entry", proto.InstRunning, 40010)

	// 40050-40062: below, above, and the end of the block are outside.
	rec := rig.do(rig.session, http.MethodPut, base, `{"engine_ports":"40050-40062"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	got := decodeKube[map[string]any](t, rec)
	want := `[{"code":"KUBE_ENGINE_PORTS_OUTSIDE","params":{"range":"40050-40062","instances":[` +
		`{"instance_uuid":"below","deployment":"chat","ports":[40010]},` +
		`{"instance_uuid":"above","deployment":"embed","ports":[40090]},` +
		`{"instance_uuid":"block","deployment":"pd","ports":[40063]}]}}]`
	if string(mustCompact(t, mustJSON(t, got["warnings"]))) != string(mustCompact(t, []byte(want))) {
		t.Fatalf("warnings = %s\nwant %s", mustJSON(t, got["warnings"]), want)
	}
	if stopped := mustJSON(t, got["stopped_instances"]); string(stopped) != "[]" {
		t.Fatalf("stopped_instances = %s", stopped)
	}
	// Saved, and the instances are left alone.
	if entryOf(t, rig, uuid).EnginePorts != "40050-40062" {
		t.Fatal("the edit was not saved")
	}
	for uuid, port := range map[string]int{"below": 40010, "above": 40090, "in-range": 40050} {
		inst := instanceOf(t, rig.st, uuid)
		if inst.APIPort != port || inst.State == proto.InstRemoving || inst.State == proto.InstStopped {
			t.Errorf("%s = port %d state %s; want kept as it was", uuid, inst.APIPort, inst.State)
		}
	}
}

// No warning when every held port is inside the new range, when engine_ports
// does not change (even if instances sit outside the range already), or
// when the entry runs nothing.
func TestEnginePortsEditWithoutInstancesOutsideTheRangeDoesNotWarn(t *testing.T) {
	rig := newSelectionRig(t, &selectionAgents{})
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"prod","server_url":"https://gw.example","engine_ports":"40000-40099"}`))
	uuid := created["uuid"].(string)
	base := "/api/v1/kube/clusters/" + uuid
	c := entryOf(t, rig, uuid)
	n := seedKubeNode(t, rig.st, c, "gpu-1")

	warnings := func(body string) string {
		t.Helper()
		rec := rig.do(rig.session, http.MethodPut, base, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body)
		}
		return string(mustJSON(t, decodeKube[map[string]any](t, rec)["warnings"]))
	}
	// Nothing runs: widening or narrowing says nothing.
	if got := warnings(`{"engine_ports":"41000-41099"}`); got != "[]" {
		t.Fatalf("no instances: warnings = %s", got)
	}
	putPortInstance(t, rig.st, n, "chat", "a", proto.InstRunning, 41010)
	putPortInstance(t, rig.st, n, "chat", "b", proto.InstRunning, 41099, 41098)
	// Still inside, including the edges, and a wider range.
	for _, ports := range []string{"41010-41099", "41000-42000"} {
		if got := warnings(`{"engine_ports":"` + ports + `"}`); got != "[]" {
			t.Fatalf("engine_ports %s: warnings = %s", ports, got)
		}
	}
	// Not changed: an instance outside the range is not news.
	putPortInstance(t, rig.st, n, "embed", "c", proto.InstRunning, 39000)
	for _, body := range []string{`{"priority":3}`, `{"engine_ports":"41000-42000"}`} {
		if got := warnings(body); got != "[]" {
			t.Fatalf("%s: warnings = %s; engine_ports did not change", body, got)
		}
	}
	// An edit that empties the field returns to the default range, which is judged like any other.
	if got := warnings(`{"engine_ports":""}`); got == "[]" {
		t.Fatalf("the default range %s leaves the running instances outside, with no warning", kubeDefaultEnginePorts)
	}
}

// A confirmed edit that stops instances does not also warn about the ports of
// the ones it just asked to stop.
func TestEnginePortsWarningSkipsInstancesJustAskedToStop(t *testing.T) {
	r := newStoppingRig(t)
	if err := r.st.DB().Model(&store.Instance{}).Where("uuid = ?", "inst-gpu-a1-chat").Updates(map[string]any{"api_port": 40000, "ports": "[40000]"}).Error; err != nil {
		t.Fatal(err)
	}
	rec := r.do(r.session, http.MethodPut, r.base, `{"node_selector":"gpuworks.ai/pool=b","engine_ports":"41000-41099","confirm_drop_instances":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d %s", rec.Code, rec.Body)
	}
	if got := string(mustJSON(t, decodeKube[map[string]any](t, rec)["warnings"])); got != "[]" {
		t.Fatalf("warnings = %s; the instance on the node that leaves is already stopping", got)
	}
}
