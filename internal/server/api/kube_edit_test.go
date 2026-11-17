package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestEditingAnEntryMadeByHandOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody))
	base := "/api/v1/kube/clusters/" + created["uuid"].(string)
	apply := "kubectl delete priorityclass gpuworks-inference-gpuworks-system --ignore-not-found && " + created["apply_command"].(string)

	// Nothing changed: nothing to apply.
	rec := rig.do(rig.session, http.MethodPut, base, `{"name":"prod"}`)
	got := decodeKube[map[string]any](t, rec)
	if rec.Code != http.StatusOK || got["reapply"] != false || len(got["changed"].([]any)) != 0 || len(got["commands"].([]any)) != 0 {
		t.Fatalf("no-op edit = %d %s", rec.Code, rec.Body)
	}

	// A new selector: apply again, the same command (the token stays).
	rec = rig.do(rig.session, http.MethodPut, base, `{"node_selector":" gpuworks.ai/pool=b "}`)
	got = decodeKube[map[string]any](t, rec)
	cluster := got["cluster"].(map[string]any)
	if rec.Code != http.StatusOK || cluster["node_selector"] != "gpuworks.ai/pool=b" || got["reapply"] != true ||
		fmt.Sprint(got["changed"]) != "[node_selector]" || fmt.Sprint(got["commands"]) != "["+apply+"]" {
		t.Fatalf("selector edit = %d %s", rec.Code, rec.Body)
	}
	if cluster["agent_image"] != "reg.example/gpuworks-agent:v1" || cluster["namespace"] != "gpuworks-system" {
		t.Fatalf("omitted fields changed: %v", cluster)
	}

	// Priority and preemption: the same line, which replaces the class
	// Kubernetes does not change in place.
	rec = rig.do(rig.session, http.MethodPut, base, `{"priority":500,"allow_preemption":true}`)
	got = decodeKube[map[string]any](t, rec)
	if rec.Code != http.StatusOK || fmt.Sprint(got["changed"]) != "[allow_preemption priority]" || fmt.Sprint(got["commands"]) != "["+apply+"]" {
		t.Fatalf("priority edit = %d %s", rec.Code, rec.Body)
	}

	// An empty engine port range takes the default, as on create.
	rec = rig.do(rig.session, http.MethodPut, base, `{"engine_ports":""}`)
	if rec.Code != http.StatusOK || decodeKube[map[string]any](t, rec)["cluster"].(map[string]any)["engine_ports"] != "40000-42767" {
		t.Fatalf("empty engine ports = %d %s", rec.Code, rec.Body)
	}

	// The grace period, the RuntimeClass and the engine ports go to the
	// running agents live and are no part of the manifest: nothing to apply
	// (TestEditingLiveSettingsNeedsNoReapply).
	rec = rig.do(rig.session, http.MethodPut, base, `{"idle_return_seconds":-1}`)
	got = decodeKube[map[string]any](t, rec)
	if rec.Code != http.StatusOK || got["reapply"] != false || got["cluster"].(map[string]any)["idle_return_seconds"] != float64(-1) {
		t.Fatalf("idle return edit = %d %s", rec.Code, rec.Body)
	}

	// An empty string clears an optional setting.
	rec = rig.do(rig.session, http.MethodPut, base, `{"agent_image":""}`)
	cluster = decodeKube[map[string]any](t, rec)["cluster"].(map[string]any)
	if rec.Code != http.StatusOK || cluster["agent_image"] != "" || !strings.Contains(cluster["effective_agent_image"].(string), "gpuworks-agent") {
		t.Fatalf("clear image = %d %s", rec.Code, rec.Body)
	}
	if rm := fmt.Sprint(cluster["remove_commands"]); !strings.Contains(rm, "kubectl delete namespace gpuworks-system") {
		t.Fatalf("remove commands = %s", rm)
	}

	// Refusals.
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"other","server_url":"https://gw.example"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	for _, tc := range []struct {
		body, code string
		status     int
	}{
		{`{"name":"other"}`, ErrorKubeClusterNameTaken, http.StatusConflict},
		{`{"name":""}`, ErrorKubeClusterInvalid, http.StatusBadRequest},
		{`{"priority":1000000001}`, ErrorKubeClusterInvalid, http.StatusBadRequest},
		{`{"engine_ports":"x"}`, ErrorKubeClusterInvalid, http.StatusBadRequest},
		{`{"node_selector":"no-equals"}`, ErrorKubeClusterInvalid, http.StatusBadRequest},
		{`{"agent_image_source":"nowhere"}`, ErrorKubeClusterInvalid, http.StatusBadRequest},
	} {
		rec := rig.do(rig.session, http.MethodPut, base, tc.body)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
			t.Fatalf("%s = %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	// Fields fixed when the entry was made are not part of the request.
	if rec := rig.do(rig.session, http.MethodPut, base, `{"namespace":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("namespace edit = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPut, base, `{"priority":"high"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/kube/clusters/nope", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown entry = %d", rec.Code)
	}

	// A revoked entry can still be edited, but has nothing to apply.
	rig.do(rig.session, http.MethodPost, base+"/revoke", "")
	rec = rig.do(rig.session, http.MethodPut, base, `{"priority":1}`)
	got = decodeKube[map[string]any](t, rec)
	if rec.Code != http.StatusOK || got["reapply"] != true || len(got["commands"].([]any)) != 0 {
		t.Fatalf("revoked edit = %d %s", rec.Code, rec.Body)
	}
	if rm := got["cluster"].(map[string]any)["remove_commands"]; rm == nil {
		t.Fatal("a revoked entry can still be removed by hand")
	}
}

func TestEditingAnEntryNeedsAnAdministrator(t *testing.T) {
	rig := newTokenRig(t)
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody))
	base := "/api/v1/kube/clusters/" + created["uuid"].(string)
	if rec := rig.do("", http.MethodPut, base, `{"name":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous edit = %d", rec.Code)
	}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(user, http.MethodPut, base, `{"name":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("user edit = %d", rec.Code)
	}
	if rec := rig.do(user, http.MethodGet, "/api/v1/kube/clusters", ""); strings.Contains(rec.Body.String(), "remove_commands") {
		t.Fatalf("user list carries the removal commands: %s", rec.Body)
	}
}

func TestEditingACloudEntryKeepsItsPoolsAndSelector(t *testing.T) {
	rig := newKubeSourceRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", fmt.Sprintf(`{"name":"ack","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1"]}`, rig.provider))
	base := "/api/v1/kube/clusters/" + decodeKube[map[string]any](t, rec)["uuid"].(string)
	if rec := rig.do(rig.session, http.MethodPut, base, `{"node_selector":"a=b"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "node pool") {
		t.Fatalf("selector on a cloud entry = %d %s", rec.Code, rec.Body)
	}
	rec = rig.do(rig.session, http.MethodPut, base, `{"priority":20000}`)
	got := decodeKube[map[string]any](t, rec)
	// The server applies a cloud entry itself (replacing the PriorityClass
	// there), so there are no commands.
	if rec.Code != http.StatusOK || got["reapply"] != true || len(got["commands"].([]any)) != 0 ||
		fmt.Sprint(got["cluster"].(map[string]any)["pool_ids"]) != "[np-1]" {
		t.Fatalf("cloud edit = %d %s", rec.Code, rec.Body)
	}
	applied := rig.do(rig.session, http.MethodPost, base+"/apply", `{"dry_run":false}`)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply after edit = %d %s", applied.Code, applied.Body)
	}
}

// Entries that may be of one cluster and share a namespace name each
// other: in one cluster the one applied later takes the other's agents.
func TestEntriesSharingANamespaceAreNamed(t *testing.T) {
	rig := newTokenRig(t)
	create := func(body string) map[string]any {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body)
		}
		return decodeKube[map[string]any](t, rec)
	}
	create(`{"name":"a","server_url":"https://gw"}`)
	b := create(`{"name":"b","server_url":"https://gw"}`)
	if fmt.Sprint(b["namespace_shared_with"]) != "[a]" {
		t.Fatalf("b = %v", b["namespace_shared_with"])
	}
	if c := create(`{"name":"c","server_url":"https://gw","namespace":"gw-c"}`); fmt.Sprint(c["namespace_shared_with"]) != "[]" {
		t.Fatalf("c = %v", c["namespace_shared_with"])
	}
	// Once nodes of different clusters joined a and b, they no longer clash.
	for name, uid := range map[string]string{"a": "uid-1", "b": "uid-2"} {
		if err := rig.st.DB().Exec("UPDATE kube_clusters SET cluster_uid = ? WHERE name = ?", uid, name).Error; err != nil {
			t.Fatal(err)
		}
	}
	list := decodeKube[struct {
		Items []map[string]any `json:"items"`
	}](t, rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", ""))
	for _, it := range list.Items {
		if fmt.Sprint(it["namespace_shared_with"]) != "[]" {
			t.Fatalf("%s after its cluster is known = %v", it["name"], it["namespace_shared_with"])
		}
	}
	// Of the same cluster: they do.
	if err := rig.st.DB().Exec("UPDATE kube_clusters SET cluster_uid = 'uid-1' WHERE name = 'b'").Error; err != nil {
		t.Fatal(err)
	}
	list = decodeKube[struct {
		Items []map[string]any `json:"items"`
	}](t, rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", ""))
	for _, it := range list.Items {
		want := map[string]string{"a": "[b]", "b": "[a]", "c": "[]"}[it["name"].(string)]
		if fmt.Sprint(it["namespace_shared_with"]) != want {
			t.Fatalf("%s of the same cluster = %v, want %s", it["name"], it["namespace_shared_with"], want)
		}
	}
}
