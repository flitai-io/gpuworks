package kubeapply

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
)

// fakeAPI is an API server holding objects by path. It answers the way the
// real one does where this package depends on it: 404 Status for a missing
// object (and for anything in a namespace that does not exist), server-side
// apply only with its content type and a field manager, dry runs that
// change nothing, and Status bodies for refusals.
type fakeAPI struct {
	mu        sync.Mutex
	objects   map[string]map[string]any // path -> body
	requests  []string                  // "METHOD path?query"
	forbidden map[string]bool           // "METHOD path" refused with 403
	token     string                    // required bearer token, "" for none
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{objects: map[string]map[string]any{}, forbidden: map[string]bool{}}
}

func status(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "message": msg, "code": code})
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
		status(w, 401, "Unauthorized", "Unauthorized")
		return
	}
	if f.forbidden[r.Method+" "+r.URL.Path] {
		status(w, 403, "Forbidden", fmt.Sprintf("%s is forbidden: User \"ram-user\" cannot %s", r.URL.Path, strings.ToLower(r.Method)))
		return
	}
	if ns, ok := namespaceOf(r.URL.Path); ok && !strings.HasSuffix(r.URL.Path, "/namespaces/"+ns) {
		if _, exists := f.objects["/api/v1/namespaces/"+ns]; !exists {
			status(w, 404, "NotFound", fmt.Sprintf("namespaces %q not found", ns))
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.Path == "/apis/apps/v1/daemonsets" {
			var items []any
			for p, o := range f.objects {
				if strings.Contains(p, "/daemonsets/") {
					items = append(items, o)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
			return
		}
		o, ok := f.objects[r.URL.Path]
		if !ok {
			status(w, 404, "NotFound", "not found")
			return
		}
		_ = json.NewEncoder(w).Encode(o)
	case http.MethodPatch:
		if r.Header.Get("Content-Type") != "application/apply-patch+yaml" {
			status(w, 415, "UnsupportedMediaType", "the body of the request was in an unknown format")
			return
		}
		if r.URL.Query().Get("fieldManager") == "" {
			status(w, 422, "Invalid", "PatchOptions.meta.k8s.io \"\" is invalid: fieldManager: Required value")
			return
		}
		b, _ := io.ReadAll(r.Body)
		var o map[string]any
		if err := json.Unmarshal(b, &o); err != nil {
			status(w, 400, "BadRequest", err.Error())
			return
		}
		// As the API server: a PriorityClass's value and preemption policy
		// cannot change once it exists.
		if have, ok := f.objects[r.URL.Path]; ok && o["kind"] == "PriorityClass" &&
			(fmt.Sprint(have["value"]) != fmt.Sprint(o["value"]) || fmt.Sprint(have["preemptionPolicy"]) != fmt.Sprint(o["preemptionPolicy"])) {
			status(w, 422, "Invalid", `PriorityClass.scheduling.k8s.io is invalid: value: Forbidden: may not be changed in an update.`)
			return
		}
		if r.URL.Query().Get("dryRun") != "All" {
			f.objects[r.URL.Path] = o
		}
		_ = json.NewEncoder(w).Encode(o)
	case http.MethodDelete:
		if _, ok := f.objects[r.URL.Path]; !ok {
			status(w, 404, "NotFound", "not found")
			return
		}
		if r.URL.Query().Get("dryRun") != "All" {
			delete(f.objects, r.URL.Path)
			if ns, ok := strings.CutPrefix(r.URL.Path, "/api/v1/namespaces/"); ok && !strings.Contains(ns, "/") {
				for p := range f.objects {
					if strings.Contains(p, "/namespaces/"+ns+"/") {
						delete(f.objects, p)
					}
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Success"})
	default:
		status(w, 405, "MethodNotAllowed", "")
	}
}

func namespaceOf(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "namespaces" && i+1 < len(parts) {
			return parts[i+1], true
		}
	}
	return "", false
}

func (f *fakeAPI) has(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[path]
	return ok
}

// serve starts f over TLS and returns a kubeconfig for it with the user
// written by user (a YAML "user:" body).
func serve(t *testing.T, f *fakeAPI, user string) []byte {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: ctx
clusters:
- name: c
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: ctx
  context: {cluster: c, user: u}
users:
- name: u
  user:
%s
`, srv.URL, base64.StdEncoding.EncodeToString(ca), user))
}

func manifest(t *testing.T) []Object {
	t.Helper()
	return manifestWith(t, func(*kubemanifest.Params) {})
}

func manifestWith(t *testing.T, edit func(*kubemanifest.Params)) []Object {
	t.Helper()
	p := kubemanifest.Params{ClusterName: "ack", Namespace: "gpuworks-system", ServerURL: "https://gw.example",
		Token: "enroll", AgentImage: "gpuworks/gpuworks-agent:v1", Priority: 10000}
	edit(&p)
	m, err := kubemanifest.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Objects(m)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func TestObjectsOfTheJoinManifest(t *testing.T) {
	var kinds []string
	for _, o := range manifest(t) {
		kinds = append(kinds, o.Kind+"/"+o.Namespace+"/"+o.Name)
	}
	want := []string{"Namespace//gpuworks-system", "ServiceAccount/gpuworks-system/gpuworks-agent", "Secret/gpuworks-system/gpuworks-enroll",
		"Role/gpuworks-system/gpuworks-agent", "RoleBinding/gpuworks-system/gpuworks-agent", "ClusterRole//gpuworks-agent-gpuworks-system",
		"ClusterRoleBinding//gpuworks-agent-gpuworks-system", "PriorityClass//gpuworks-inference-gpuworks-system", "DaemonSet/gpuworks-system/gpuworks-agent"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("objects = %v", kinds)
	}
	for name, m := range map[string]string{
		"unknown kind": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: y}\n",
		"no name":      "apiVersion: v1\nkind: Namespace\nmetadata: {}\n",
		"no namespace": "apiVersion: v1\nkind: Secret\nmetadata: {name: x}\n",
		"not yaml":     "kind: [\n",
		"deployment":   "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: x, namespace: y}\n",
	} {
		if _, err := Objects(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewClientTakesOnlyCredentialsCarriedInTheFile(t *testing.T) {
	f := newFakeAPI()
	f.token = "do-token"
	if c, err := NewClient(serve(t, f, "    token: do-token")); err != nil || !strings.HasPrefix(c.Server(), "https://127.0.0.1:") {
		t.Fatalf("token kubeconfig: %v", err)
	}
	certPEM, keyPEM := clientCert(t)
	certUser := fmt.Sprintf("    client-certificate-data: %s\n    client-key-data: %s", base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(keyPEM))
	if _, err := NewClient(serve(t, newFakeAPI(), certUser)); err != nil {
		t.Fatalf("certificate kubeconfig: %v", err)
	}
	for name, user := range map[string]string{
		"exec":          "    exec: {command: aliyun, apiVersion: client.authentication.k8s.io/v1}",
		"auth provider": "    auth-provider: {name: oidc}",
		"cert file":     "    client-certificate: /home/me/.kube/cert.pem",
		"nothing":       "    username: admin",
	} {
		if _, err := NewClient(serve(t, newFakeAPI(), user)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, kc := range map[string]string{
		"http server":    "current-context: c\nclusters: [{name: c, cluster: {server: 'http://10.0.0.1:6443'}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {token: t}}]\n",
		"no context":     "current-context: x\nclusters: []\ncontexts: []\nusers: []\n",
		"ca file":        "current-context: c\nclusters: [{name: c, cluster: {server: 'https://a:6443', certificate-authority: /ca.pem}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {token: t}}]\n",
		"missing user":   "current-context: c\nclusters: [{name: c, cluster: {server: 'https://a:6443'}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nusers: []\n",
		"bad ca":         "current-context: c\nclusters: [{name: c, cluster: {server: 'https://a:6443', certificate-authority-data: bm90IGEgY2VydA==}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {token: t}}]\n",
		"not kubeconfig": "{",
	} {
		if _, err := NewClient([]byte(kc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func clientCert(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes-admin"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func actions(rs []Result) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Kind+":"+r.Action)
	}
	return out
}

func TestDryRunChangesNothingAndApplyCreatesThenUpdates(t *testing.T) {
	f := newFakeAPI()
	f.token = "tok"
	c, err := NewClient(serve(t, f, "    token: tok"))
	if err != nil {
		t.Fatal(err)
	}
	objs := manifest(t)
	ctx := context.Background()

	dry, err := c.Apply(ctx, objs, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Namespace:create", "ServiceAccount:dependent", "Secret:dependent", "Role:dependent", "RoleBinding:dependent",
		"ClusterRole:create", "ClusterRoleBinding:create", "PriorityClass:create", "DaemonSet:dependent"}
	if !slices.Equal(actions(dry), want) {
		t.Fatalf("dry run = %v", actions(dry))
	}
	if len(f.objects) != 0 {
		t.Fatalf("a dry run created %d objects", len(f.objects))
	}
	for _, q := range f.requests {
		if strings.HasPrefix(q, "PATCH ") && (!strings.Contains(q, "dryRun=All") || !strings.Contains(q, "fieldManager=gpuworks") || !strings.Contains(q, "force=true")) {
			t.Fatalf("dry-run patch = %s", q)
		}
	}

	applied, err := c.Apply(ctx, objs, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range applied {
		if r.Action != ActionCreate {
			t.Fatalf("first apply: %v", actions(applied))
		}
	}
	if !f.has("/apis/apps/v1/namespaces/gpuworks-system/daemonsets/gpuworks-agent") || !f.has("/apis/scheduling.k8s.io/v1/priorityclasses/gpuworks-inference-gpuworks-system") {
		t.Fatalf("objects after apply: %v", f.objects)
	}
	if got := f.objects["/api/v1/namespaces/gpuworks-system/secrets/gpuworks-enroll"]["stringData"].(map[string]any)["token"]; got != "enroll" {
		t.Fatalf("secret body = %v", got)
	}

	// Applied again (an upgrade, a repeated click): every object exists, a
	// dry run now checks all of them.
	again, err := c.Apply(ctx, objs, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		if r.Action != ActionUpdate {
			t.Fatalf("dry run over an applied manifest: %v", actions(again))
		}
	}
}

func TestARefusalStopsTheApplyAndSaysWhy(t *testing.T) {
	f := newFakeAPI()
	c, err := NewClient(serve(t, f, "    token: tok"))
	if err != nil {
		t.Fatal(err)
	}
	f.forbidden["PATCH /apis/rbac.authorization.k8s.io/v1/clusterroles/gpuworks-agent-gpuworks-system"] = true
	rs, err := c.Apply(context.Background(), manifest(t), false)
	if err == nil || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	got := actions(rs)
	if got[5] != "ClusterRole:failed" || got[6] != "ClusterRoleBinding:not_tried" || got[8] != "DaemonSet:not_tried" || !strings.Contains(rs[5].Error, "ram-user") {
		t.Fatalf("results = %v (%q)", got, rs[5].Error)
	}
	if f.has("/apis/apps/v1/namespaces/gpuworks-system/daemonsets/gpuworks-agent") {
		t.Fatal("objects after the refusal were applied")
	}
	unauth := newFakeAPI()
	unauth.token = "right"
	c2, _ := NewClient(serve(t, unauth, "    token: wrong"))
	if _, err := c2.Apply(context.Background(), manifest(t), true); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expired kubeconfig: %v", err)
	}
}

func TestDeleteRemovesTheDaemonSetFirstAndLeavesOtherEntriesAlone(t *testing.T) {
	f := newFakeAPI()
	c, err := NewClient(serve(t, f, "    token: tok"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	objs := manifest(t)
	if _, err := c.Apply(ctx, objs, false); err != nil {
		t.Fatal(err)
	}
	// A second gpuworks entry of the same cluster, in another namespace, with
	// its own cluster-wide objects.
	f.objects["/api/v1/namespaces/gpuworks-npu"] = map[string]any{"metadata": map[string]any{"name": "gpuworks-npu"}}
	f.objects["/apis/apps/v1/namespaces/gpuworks-npu/daemonsets/gpuworks-agent"] = map[string]any{"metadata": map[string]any{"name": "gpuworks-agent", "namespace": "gpuworks-npu"}}
	f.objects["/apis/scheduling.k8s.io/v1/priorityclasses/gpuworks-inference-gpuworks-npu"] = map[string]any{"metadata": map[string]any{"name": "gpuworks-inference-gpuworks-npu"}}

	dry, err := c.Delete(ctx, objs, true)
	if err != nil || dry[0].Kind != "DaemonSet" || dry[0].Action != ActionDelete || !f.has("/apis/apps/v1/namespaces/gpuworks-system/daemonsets/gpuworks-agent") {
		t.Fatalf("dry-run delete = %v, %v", actions(dry), err)
	}
	f.requests = nil
	rs, err := c.Delete(ctx, objs, false)
	if err != nil {
		t.Fatal(err)
	}
	var deletes []string
	for _, q := range f.requests {
		if strings.HasPrefix(q, "DELETE ") {
			deletes = append(deletes, strings.SplitN(q, "?", 2)[0])
		}
	}
	if !strings.HasSuffix(deletes[0], "/daemonsets/gpuworks-agent") || !slices.Contains(deletes, "DELETE /api/v1/namespaces/gpuworks-system") {
		t.Fatalf("deletes = %v", deletes)
	}
	for _, r := range rs {
		if r.Action != ActionDelete {
			t.Fatalf("%s/%s: %s, want delete", r.Kind, r.Name, r.Action)
		}
	}
	if f.has("/apis/scheduling.k8s.io/v1/priorityclasses/gpuworks-inference-gpuworks-system") || f.has("/api/v1/namespaces/gpuworks-system") {
		t.Fatal("the entry's own objects survived")
	}
	if !f.has("/apis/scheduling.k8s.io/v1/priorityclasses/gpuworks-inference-gpuworks-npu") || !f.has("/apis/apps/v1/namespaces/gpuworks-npu/daemonsets/gpuworks-agent") {
		t.Fatal("the other entry's objects were removed")
	}
	// Removed again: everything is already gone.
	rs, err = c.Delete(ctx, objs, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Action != ActionAbsent {
			t.Fatalf("second delete: %v", actions(rs))
		}
	}
}

func TestDeleteGoesOnPastARefusal(t *testing.T) {
	f := newFakeAPI()
	c, _ := NewClient(serve(t, f, "    token: tok"))
	ctx := context.Background()
	objs := manifest(t)
	if _, err := c.Apply(ctx, objs, false); err != nil {
		t.Fatal(err)
	}
	f.forbidden["DELETE /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/gpuworks-agent-gpuworks-system"] = true
	rs, err := c.Delete(ctx, objs, false)
	if err == nil {
		t.Fatal("refusal not returned")
	}
	if f.has("/api/v1/namespaces/gpuworks-system") || f.has("/apis/rbac.authorization.k8s.io/v1/clusterroles/gpuworks-agent-gpuworks-system") {
		t.Fatalf("delete stopped at the refusal: %v", actions(rs))
	}
	if !f.has("/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/gpuworks-agent-gpuworks-system") {
		t.Fatal("refused object deleted")
	}
	// The entry's priority class goes with it.
	if f.has("/apis/scheduling.k8s.io/v1/priorityclasses/gpuworks-inference-gpuworks-system") {
		t.Fatal("the entry's priority class kept")
	}
}

// An entry's edited priority or preemption cannot be patched into its
// PriorityClass (the API server refuses): the dry run reports a replacement
// and the apply deletes the class and creates it again; unchanged, it is an
// ordinary update.
func TestAChangedPriorityClassIsReplaced(t *testing.T) {
	f := newFakeAPI()
	c, _ := NewClient(serve(t, f, "    token: tok"))
	ctx := context.Background()
	if _, err := c.Apply(ctx, manifest(t), false); err != nil {
		t.Fatal(err)
	}
	const pc = "/apis/scheduling.k8s.io/v1/priorityclasses/gpuworks-inference-gpuworks-system"
	before := f.objects[pc]["value"]
	edited := manifestWith(t, func(p *kubemanifest.Params) { p.Priority, p.AllowPreemption = 42, true })
	dry, err := c.Apply(ctx, edited, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range dry {
		if r.Kind == "PriorityClass" && r.Action != ActionReplace {
			t.Fatalf("dry run: %v", actions(dry))
		}
	}
	if f.objects[pc]["value"] != before {
		t.Fatal("a dry run replaced the class")
	}
	rs, err := c.Apply(ctx, edited, false)
	if err != nil {
		t.Fatalf("apply: %v (%v)", err, actions(rs))
	}
	if v, ok := f.objects[pc]["value"].(float64); !ok || v != 42 || f.objects[pc]["preemptionPolicy"] != nil {
		t.Fatalf("class after the apply = %v", f.objects[pc])
	}
	again, err := c.Apply(ctx, edited, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		if r.Kind == "PriorityClass" && r.Action != ActionUpdate {
			t.Fatalf("unchanged class: %v", actions(again))
		}
	}
}
