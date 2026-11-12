package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const kubeCreateBody = `{"name":"prod","server_url":"https://gw.example:8800/","node_selector":"gpuworks.ai/pool=a","agent_image":"reg.example/gpuworks-agent:v1"}`

func TestKubeClusterLifecycleOverTheWire(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"name": "prod", "namespace": "gpuworks-system", "server_url": "https://gw.example:8800",
		"agent_image": "reg.example/gpuworks-agent:v1", "node_selector": "gpuworks.ai/pool=a",
		"agent_image_source": "dockerhub", "effective_agent_image": "reg.example/gpuworks-agent:v1",
		"priority": float64(10000), "allow_preemption": false,
		"runtime_class": "", "image_pull_secret": "", "containerd_socket": "", "state_dir": "/opt/gpuworks", "api_server": "",
		"engine_ports": "40000-42767", "idle_return_seconds": float64(60), "cluster_uid": "",
		"revoked": false, "node_count": float64(0),
	} {
		if created[key] != want {
			t.Fatalf("created[%q] = %#v, want %#v (body %s)", key, created[key], want, rec.Body)
		}
	}
	if _, leaked := created["token"]; leaked {
		t.Fatal("the enrollment token is a field of its own on the wire")
	}
	apply, _ := created["apply_command"].(string)
	const prefix = "kubectl apply -f 'https://gw.example:8800/kube/manifest.yaml?token="
	if !strings.HasPrefix(apply, prefix) || !strings.HasSuffix(apply, "'") {
		t.Fatalf("apply_command = %q", apply)
	}
	token, err := url.QueryUnescape(strings.TrimSuffix(strings.TrimPrefix(apply, prefix), "'"))
	if err != nil || token == "" {
		t.Fatalf("token in apply command: %q %v", token, err)
	}
	uuid := created["uuid"].(string)

	// The manifest is served to the token holder without a login.
	if rec := rig.do("", http.MethodGet, "/kube/manifest.yaml?token="+url.QueryEscape(token), ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "kind: DaemonSet") || rec.Header().Get("Content-Type") != "application/yaml; charset=utf-8" {
		t.Fatalf("manifest by token = %d %s", rec.Code, rec.Header())
	}
	if rec := rig.do("", http.MethodGet, "/kube/manifest.yaml?token=nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("manifest with a wrong token = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+uuid+"/manifest", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin manifest = %d", rec.Code)
	}

	// Enrollment: idempotent per node, bound to the first cluster UID.
	enroll := func(body string) (int, map[string]any) {
		rec := rig.do("", http.MethodPost, "/api/v1/kube/enroll", body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	tok, _ := json.Marshal(token)
	code, first := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-a","node_name":"gpu-1"}`)
	if code != http.StatusOK || first["node_uuid"] == "" || first["node_token"] == "" || first["idle_return_seconds"] != float64(60) {
		t.Fatalf("enroll = %d %v", code, first)
	}
	if code, again := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-a","node_name":"gpu-1"}`); code != http.StatusOK || again["node_uuid"] != first["node_uuid"] || again["node_token"] != first["node_token"] {
		t.Fatalf("re-enroll = %d %v", code, again)
	}
	if code, out := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-b","node_name":"gpu-2"}`); code != http.StatusForbidden || out["code"] != ErrorKubeEnrollRefused || out["params"].(map[string]any)["reason"] != "other_cluster" {
		t.Fatalf("other cluster = %d %v", code, out)
	}
	if code, out := enroll(`{"token":"nope","cluster_uid":"uid-a","node_name":"gpu-2"}`); code != http.StatusForbidden || out["params"].(map[string]any)["reason"] != "unknown_token" {
		t.Fatalf("unknown token = %d %v", code, out)
	}
	if code, _ := enroll(`{"token":` + string(tok) + `,"cluster_uid":"","node_name":"gpu-2"}`); code != http.StatusBadRequest {
		t.Fatalf("missing cluster uid = %d", code)
	}

	// The node shows up in the list with the cluster's count.
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0]["node_count"] != float64(1) || list.Items[0]["cluster_uid"] != "uid-a" {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}

	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/kube/clusters/"+uuid, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorKubeClusterHasNodes) {
		t.Fatalf("delete with nodes = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/"+uuid+"/revoke", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked":true`) || strings.Contains(rec.Body.String(), "apply_command") {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if code, _ := enroll(`{"token":` + string(tok) + `,"cluster_uid":"uid-a","node_name":"gpu-1"}`); code != http.StatusForbidden {
		t.Fatalf("enroll after revoke = %d", code)
	}
	if rec := rig.do("", http.MethodGet, "/kube/manifest.yaml?token="+url.QueryEscape(token), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("manifest after revoke = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/missing/revoke", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke missing = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodDelete, "/api/v1/kube/clusters/missing", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d", rec.Code)
	}
}

func TestKubeClusterValidation(t *testing.T) {
	rig := newTokenRig(t)
	for _, tc := range []struct{ body, field string }{
		{`{"name":"","server_url":"https://gw"}`, "name"},
		{`{"name":"a","server_url":"gw"}`, "manifest"},
		{`{"name":"a","server_url":"https://gw","engine_ports":"9-1"}`, "engine_ports"},
		{`{"name":"a","server_url":"https://gw","idle_return_seconds":-2}`, "idle_return_seconds"},
		{`{"name":"a","server_url":"https://gw","namespace":"Bad_NS"}`, "manifest"},
		{`{"name":"a","server_url":"https://gw","node_selector":"novalue"}`, "manifest"},
		{`{"name":"a","server_url":"https://gw","state_dir":"opt/x"}`, "manifest"},
		{`{"name":"a","server_url":"https://gw","containerd_socket":"run/x.sock"}`, "manifest"},
		{`{"name":"a","server_url":"https://gw","api_server":"http://10.0.0.5:6443"}`, "manifest"},
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorKubeClusterInvalid) || !strings.Contains(rec.Body.String(), `"field":"`+tc.field+`"`) {
			t.Fatalf("%s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorKubeClusterNameTaken) {
		t.Fatalf("duplicate = %d %s", rec.Code, rec.Body)
	}
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"b","server_url":"https://gw","runtime_class":"nvidia","image_pull_secret":"regcred","idle_return_seconds":-1}`)
	var b map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if rec.Code != http.StatusCreated || b["runtime_class"] != "nvidia" || b["image_pull_secret"] != "regcred" || b["idle_return_seconds"] != float64(-1) {
		t.Fatalf("named runtime class, pull secret and keep = %d %s", rec.Code, rec.Body)
	}
	manifest := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+b["uuid"].(string)+"/manifest", "").Body.String()
	// The RuntimeClass is the NVIDIA instance pods'; the agent runs under the
	// default runtime and passes it on.
	for _, want := range []string{"imagePullSecrets:\n        - name: \"regcred\"", "- name: GPUWORKS_KUBE_RUNTIME_CLASS\n              value: \"nvidia\""} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest lacks %s:\n%s", want, manifest)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"c","server_url":"https://gw","image_pull_secret":"Reg Cred"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), ErrorKubeClusterInvalid) || !strings.Contains(rec.Body.String(), "image_pull_secret") {
		t.Fatalf("bad pull secret = %d %s", rec.Code, rec.Body)
	}
	// A k3s cluster's own state directory, socket and API server reach the
	// agents; no vendor's driver tree is mounted, whatever the nodes have.
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"npu","server_url":"https://gw","state_dir":"/opt/gpuworks-kube","containerd_socket":"/run/k3s/containerd/containerd.sock","api_server":"https://10.0.0.5:6443"}`)
	var npu map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &npu)
	if rec.Code != http.StatusCreated || npu["runtime_class"] != "" || npu["state_dir"] != "/opt/gpuworks-kube" ||
		npu["containerd_socket"] != "/run/k3s/containerd/containerd.sock" || npu["api_server"] != "https://10.0.0.5:6443" {
		t.Fatalf("ascend cluster = %d %s", rec.Code, rec.Body)
	}
	manifest = rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+npu["uuid"].(string)+"/manifest", "").Body.String()
	for _, want := range []string{`"--state=/opt/gpuworks-kube"`, "GPUWORKS_KUBE_CONTAINERD_SOCKET", `value: "10.0.0.5"`, `value: "6443"`} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("k3s manifest lacks %s:\n%s", want, manifest)
		}
	}
	if strings.Contains(manifest, `"/usr/local"`) {
		t.Fatalf("a vendor's directory is mounted:\n%s", manifest)
	}
	if strings.Contains(manifest, "runtimeClassName") {
		t.Fatal("ascend manifest names a RuntimeClass")
	}
}

func TestKubeClusterAccess(t *testing.T) {
	rig := newTokenRig(t)
	if rec := rig.do("", http.MethodGet, "/api/v1/kube/clusters", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", rec.Code)
	}
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(user, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusForbidden {
		t.Fatalf("user create = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	rec := rig.do(user, http.MethodGet, "/api/v1/kube/clusters", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "apply_command") {
		t.Fatalf("user list = %d %s; the token-bearing command is for administrators", rec.Code, rec.Body)
	}
	if rec := rig.do(user, http.MethodGet, "/api/v1/kube/clusters/x/manifest", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("user manifest = %d", rec.Code)
	}
}

func TestKubeNodeDeleteNeedsNoSSH(t *testing.T) {
	rig := newTokenRig(t)
	c, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := rig.st.EnrollKubeNode(c.Token, "uid", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	rec := rig.do(rig.session, http.MethodDelete, "/api/v1/nodes/"+node.UUID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"deleted"`) {
		t.Fatalf("delete kube node = %d %s", rec.Code, rec.Body)
	}
}

// An agent that runs from its node's state directory is upgraded in place
// like any agent: the DaemonSet is left alone.
func TestKubeNodeOnTheStateDirectoryIsUpgradedInPlace(t *testing.T) {
	rig := newTokenRig(t)
	c, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := rig.st.EnrollKubeNode(c.Token, "uid", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rig.h = Router(Deps{AuthSigner: signer, Store: rig.st,
		NodeCaps: func(string) []string { return []string{proto.CapUpgradePush, proto.CapKubeBinaryUpgrade} },
		CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
			methods = append(methods, method)
			return json.RawMessage(`{}`), nil
		}})
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/nodes/"+node.UUID+"/upgrade", "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"upgrading"`) {
		t.Fatalf("upgrade = %d %s", rec.Code, rec.Body)
	}
	if len(methods) != 1 || methods[0] != proto.RPCAgentUpgrade {
		t.Fatalf("RPCs = %v, want only %s", methods, proto.RPCAgentUpgrade)
	}
}

// The assistant reads GET /kube/clusters as the asking administrator but
// must never see the enrollment token: the same request carries the apply
// command for the UI and omits it for the assistant's reader.
func TestAssistantReadOfKubeClustersOmitsTheEnrollmentToken(t *testing.T) {
	rig := newTokenRig(t)
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", ""); !strings.Contains(rec.Body.String(), `"apply_command":"kubectl apply -f`) {
		t.Fatalf("the UI's list lost the apply command: %s", rec.Body)
	}

	reader := &aiReader{mux: rig.h.(*chi.Mux), header: http.Header{"Authorization": {"Bearer " + rig.session}}}
	status, body, err := reader.Get(context.Background(), "/api/v1/kube/clusters", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("assistant read = %d %v %s", status, err, body)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("body = %s err=%v", body, err)
	}
	if list.Items[0]["name"] != "prod" {
		t.Fatalf("the assistant should still see the cluster: %s", body)
	}
	if _, ok := list.Items[0]["apply_command"]; ok || strings.Contains(string(body), "token=") {
		t.Fatalf("the assistant read carries the enrollment token: %s", body)
	}
}

// A Kubernetes node's instances are pods of its cluster, so there is no
// runtime to pick: the node says it offers no choices (the UI hides the
// selector on that), and the switch itself is refused before it reaches the
// agent or the store. A Mac still offers only auto; any other node, every
// driver.
func TestKubeNodeOffersNoRuntimeChoiceAndRefusesTheSwitch(t *testing.T) {
	rig := newTokenRig(t)
	c, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "c"})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := rig.st.EnrollKubeNode(c.Token, "uid", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := rig.st.CreateNodePending("plain")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rig.h = Router(Deps{AuthSigner: signer, Store: rig.st, CallNode: func(_ context.Context, _, _ string, _ any) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{"runtime":"docker"}`), nil
	}})

	rec := rig.do(rig.session, http.MethodPatch, "/api/v1/nodes/"+node.UUID+"/runtime", `{"runtime":"docker"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"NODE_RUNTIME_KUBE_NODE"`) {
		t.Fatalf("runtime switch on a Kubernetes node = %d %s", rec.Code, rec.Body)
	}
	if called {
		t.Fatal("the refused switch reached the agent")
	}
	if stored, _ := rig.st.NodeByUUID(node.UUID); stored.RuntimePreference == proto.RuntimePreferenceDocker {
		t.Fatal("the refused switch was stored")
	}

	choices := func(uuid string) any {
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/nodes/"+uuid, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("node %s = %d %s", uuid, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body["runtime_choices"]
	}
	if got := choices(node.UUID); got != nil {
		t.Fatalf("a Kubernetes node's runtime_choices = %v, want absent", got)
	}
	if got, _ := json.Marshal(choices(plain.UUID)); string(got) != `["auto","docker","nerdctl","udocker"]` {
		t.Fatalf("a plain node's runtime_choices = %s", got)
	}
	if got := nodeDTORuntimeChoices(&store.Node{OS: "darwin"}); len(got) != 1 || got[0] != proto.RuntimePreferenceAuto {
		t.Fatalf("a Mac's runtime choices = %v", got)
	}
}

// A node another entry of the same cluster holds is refused with its own
// reason, which the agent does not answer by falling back to a saved
// identity.
func TestKubeEnrollRefusesANodeAnotherEntryHolds(t *testing.T) {
	rig := newTokenRig(t)
	a, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "a", Namespace: "gw-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "b", Namespace: "gw-b"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := rig.do("", http.MethodPost, "/api/v1/kube/enroll", `{"token":"`+a.Token+`","cluster_uid":"uid","node_name":"gpu-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("first entry = %d %s", rec.Code, rec.Body)
	}
	rec := rig.do("", http.MethodPost, "/api/v1/kube/enroll", `{"token":"`+b.Token+`","cluster_uid":"uid","node_name":"gpu-1"}`)
	var body struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body.Code != ErrorKubeEnrollRefused || body.Params["reason"] != "node_taken" || body.Params["entry"] != "a" {
		t.Fatalf("second entry = %d %s", rec.Code, rec.Body)
	}
}
