package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// apiClusterDriver is a cloud account with one managed cluster, whose
// kubeconfig points at a fake API server.
type apiClusterDriver struct {
	apiInstallFailingProviderDriver
	kubeconfig []byte
	listErr    error
	lists      atomic.Int32 // how many times the cloud was asked for the clusters
}

func (*apiClusterDriver) Name() string { return "api-cluster-provider" }

func (d *apiClusterDriver) ListClusters(context.Context, provider.Credentials) ([]provider.Cluster, error) {
	d.lists.Add(1)
	if d.listErr != nil {
		return nil, d.listErr
	}
	const label = "alibabacloud.com/nodepool-id"
	return []provider.Cluster{
		// Three GPU node pools, two of NVIDIA cards and one of PPU cards.
		{ExternalID: "c-gpu", Name: "ack-gpu", Region: "cn-shanghai", Version: "1.35.7", Type: provider.ClusterTypeACKManaged, State: "running", Running: true, PublicAPI: true,
			GPUPools: []provider.ClusterPool{
				{ID: "np-1", Name: "l20", GPUModel: "NVIDIA L20", GPUCount: 1, Nodes: 1, Vendor: "nvidia", LabelKey: label},
				{ID: "np-2", Name: "a10", GPUModel: "NVIDIA A10", GPUCount: 1, Nodes: 2, Vendor: "nvidia", LabelKey: label},
				{ID: "np-ppu", Name: "ppu", GPUModel: "PPU-ZW810E", GPUCount: 1, Nodes: 1, Vendor: "ppu", LabelKey: label},
			}},
		{ExternalID: "c-private", Name: "ack-private", Region: "cn-hangzhou", State: "running", Running: true,
			GPUPools: []provider.ClusterPool{{ID: "np-p", Name: "l20", GPUModel: "NVIDIA L20", GPUCount: 1, Nodes: 1, Vendor: "nvidia", LabelKey: label}}},
		{ExternalID: "c-other", Name: "ack-other", Region: "cn-beijing", State: "running", Running: true, PublicAPI: true,
			GPUPools: []provider.ClusterPool{{ID: "np-o", Name: "l20", GPUModel: "NVIDIA L20", GPUCount: 1, Nodes: 1, Vendor: "nvidia", LabelKey: label}}},
		{ExternalID: "c-empty", Name: "ack-empty", Region: "cn-hangzhou", State: "running", Running: true, PublicAPI: true, GPUPools: []provider.ClusterPool{}},
	}, nil
}

func (d *apiClusterDriver) ClusterKubeconfig(context.Context, provider.Credentials, string) ([]byte, error) {
	return d.kubeconfig, nil
}

// objectAPI is an API server that keeps what is applied.
type objectAPI struct {
	mu      sync.Mutex
	objects map[string]bool
	refuse  string
}

func (f *objectAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	notFound := func() {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"kind":"Status","reason":"NotFound","code":404}`)
	}
	if f.refuse != "" && strings.Contains(r.URL.Path, f.refuse) && r.Method != http.MethodGet {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"kind":"Status","reason":"Forbidden","message":"clusterroles is forbidden: User \"ram\" cannot patch","code":403}`)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.Path == "/apis/apps/v1/daemonsets" {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		if !f.objects[r.URL.Path] {
			notFound()
			return
		}
		fmt.Fprint(w, `{}`)
	case http.MethodPatch:
		_, _ = io.ReadAll(r.Body)
		if r.URL.Query().Get("dryRun") == "" {
			f.objects[r.URL.Path] = true
		}
		fmt.Fprint(w, `{}`)
	case http.MethodDelete:
		if !f.objects[r.URL.Path] {
			notFound()
			return
		}
		if r.URL.Query().Get("dryRun") == "" {
			delete(f.objects, r.URL.Path)
		}
		fmt.Fprint(w, `{}`)
	}
}

func objectAPIKubeconfig(t *testing.T, f *objectAPI) []byte {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return []byte(fmt.Sprintf("current-context: c\nclusters: [{name: c, cluster: {server: %q, certificate-authority-data: %s}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {token: tok}}]\n", srv.URL, ca))
}

type kubeSourceRig struct {
	*tokenRig
	drv      *apiClusterDriver
	api      *objectAPI
	provider uint
	signer   *auth.TokenSigner
	// agents answers the RPCs the server sends to a node's agent; nil
	// means no agent can be reached.
	agents func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error)
}

func newKubeSourceRig(t *testing.T) *kubeSourceRig {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	admin, _ := st.VerifyUser("admin", "gpuworks-test-password-2026")
	signer, err := auth.NewTokenSigner("kube-source-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("kube-source-test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &objectAPI{objects: map[string]bool{}}
	drv := &apiClusterDriver{kubeconfig: objectAPIKubeconfig(t, fake)}
	provider.Register(drv)
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), drv.Name(), "ali", provider.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	out := &kubeSourceRig{drv: drv, api: fake, provider: p.ID, signer: signer}
	callNode := func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		if out.agents == nil {
			return nil, errors.New("node is offline")
		}
		return out.agents(ctx, uuid, method, params)
	}
	out.tokenRig = &tokenRig{t: t, h: Router(Deps{AuthSigner: signer, Store: st, Providers: svc, AI: &fakeAI{available: true}, CallNode: callNode}), st: st, session: session}
	return out
}

func decodeKube[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	return v
}

func TestAClusterOfACloudAccountBecomesAnEntryOnce(t *testing.T) {
	rig := newKubeSourceRig(t)
	path := fmt.Sprintf("/api/v1/providers/%d/clusters", rig.provider)
	rec := rig.do(rig.session, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("clusters = %d %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`"external_id":"c-gpu"`, `"public_api":true`, `"id":"np-1","name":"l20"`, `"in_entry":false`, `"gpu_model":"NVIDIA L20"`, `"external_id":"c-private"`, `"public_api":false`, `"gpu_pools":[]`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("clusters lack %s: %s", want, rec.Body)
		}
	}
	if strings.Contains(rec.Body.String(), `"entry"`) {
		t.Fatal("an entry before any was made")
	}
	// The cluster's product travels as a code with its name; a cluster whose driver cannot tell carries neither.
	if !strings.Contains(rec.Body.String(), `"external_id":"c-gpu","name":"ack-gpu","region":"cn-shanghai","version":"1.35.7","type":"ack_managed","type_label":{"en":"ACK managed","zh":"ACK 托管版"},"state":"running"`) {
		t.Fatalf("c-gpu lacks its type: %s", rec.Body)
	}
	if strings.Count(rec.Body.String(), `"type"`) != 1 || strings.Count(rec.Body.String(), `"type_label"`) != 1 {
		t.Fatalf("a cluster without a type carries one: %s", rec.Body)
	}

	body := fmt.Sprintf(`{"name":"ack-gpu","server_url":"https://gw.example","provider_pool_ids":["np-2","np-1"],"provider_id":%d,"provider_cluster_id":"c-gpu"}`, rig.provider)
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", body)
	created := decodeKube[map[string]any](t, rec)
	src, _ := created["source"].(map[string]any)
	if rec.Code != http.StatusCreated || src["provider_id"] != float64(rig.provider) || src["provider_name"] != "ali" || src["provider_kind"] != "api-cluster-provider" || src["cluster_id"] != "c-gpu" {
		t.Fatalf("create from a cluster = %d %s", rec.Code, rec.Body)
	}
	if pools, _ := json.Marshal(created["pool_ids"]); string(pools) != `["np-1","np-2"]` || created["node_selector"] != "" {
		t.Fatalf("the entry holds pools %s, selector %q", pools, created["node_selector"])
	}
	if _, has := created["vendor"]; has {
		t.Fatalf("an entry has no vendor: %s", rec.Body)
	}
	after := rig.do(rig.session, http.MethodGet, path, "")
	if !strings.Contains(after.Body.String(), `"entry":{"uuid":"`+created["uuid"].(string)+`","name":"ack-gpu"}`) {
		t.Fatalf("clusters after the entry: %s", after.Body)
	}
	listed := decodeKube[struct {
		Items []struct {
			ExternalID string `json:"external_id"`
			GPUPools   []struct {
				ID      string `json:"id"`
				InEntry bool   `json:"in_entry"`
			} `json:"gpu_pools"`
		} `json:"items"`
	}](t, after)
	for _, c := range listed.Items {
		for _, p := range c.GPUPools {
			if want := c.ExternalID == "c-gpu" && (p.ID == "np-1" || p.ID == "np-2"); p.InEntry != want {
				t.Errorf("pool %s of %s: in_entry = %v, want %v; only the pools the entry holds are in it", p.ID, c.ExternalID, p.InEntry, want)
			}
		}
	}
	again := strings.Replace(body, `"name":"ack-gpu"`, `"name":"ack-gpu-2"`, 1)
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", again); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorKubeClusterSourceTaken) || !strings.Contains(rec.Body.String(), `"cluster":"c-gpu"`) {
		t.Fatalf("second entry from the cluster = %d %s", rec.Code, rec.Body)
	}
	for name, bad := range map[string]string{
		"id without cluster":                fmt.Sprintf(`{"name":"x","server_url":"https://gw","provider_id":%d}`, rig.provider),
		"cluster without id":                `{"name":"x","server_url":"https://gw","provider_cluster_id":"c-gpu"}`,
		"unknown account":                   `{"name":"x","server_url":"https://gw","provider_id":999,"provider_cluster_id":"c-gpu"}`,
		"blank cluster":                     fmt.Sprintf(`{"name":"x","server_url":"https://gw","provider_id":%d,"provider_cluster_id":"  "}`, rig.provider),
		"no pool chosen":                    fmt.Sprintf(`{"name":"x","server_url":"https://gw","provider_id":%d,"provider_cluster_id":"c-other"}`, rig.provider),
		"only blank pools":                  fmt.Sprintf(`{"name":"x","server_url":"https://gw","provider_id":%d,"provider_cluster_id":"c-other","provider_pool_ids":[" "]}`, rig.provider),
		"a pool of another cluster":         fmt.Sprintf(`{"name":"x","server_url":"https://gw","provider_id":%d,"provider_cluster_id":"c-other","provider_pool_ids":["np-1"]}`, rig.provider),
		"a cluster the account lacks":       fmt.Sprintf(`{"name":"x","server_url":"https://gw","provider_id":%d,"provider_cluster_id":"c-nope","provider_pool_ids":["np-1"]}`, rig.provider),
		"pools without a source":            `{"name":"x","server_url":"https://gw","provider_pool_ids":["np-1"]}`,
		"a free-form selector beside pools": fmt.Sprintf(`{"name":"x","server_url":"https://gw","node_selector":"a=b","provider_id":%d,"provider_cluster_id":"c-other","provider_pool_ids":["np-o"]}`, rig.provider),
	} {
		if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", bad); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorKubeClusterInvalid) {
			t.Fatalf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	list := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	if !strings.Contains(list.Body.String(), `"source":{"provider_id":`) {
		t.Fatalf("list lacks the source: %s", list.Body)
	}

	// Deleting the account keeps the entry, without its source.
	if rec := rig.do(rig.session, http.MethodDelete, fmt.Sprintf("/api/v1/providers/%d", rig.provider), ""); rec.Code/100 != 2 {
		t.Fatalf("delete account = %d %s", rec.Code, rec.Body)
	}
	list = rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	if !strings.Contains(list.Body.String(), `"name":"ack-gpu"`) || strings.Contains(list.Body.String(), `"source"`) {
		t.Fatalf("list after deleting the account: %s", list.Body)
	}
	op := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/"+created["uuid"].(string)+"/apply", `{"dry_run":true}`)
	if op.Code != http.StatusConflict || !strings.Contains(op.Body.String(), ErrorKubeClusterNoSource) {
		t.Fatalf("apply after the account is gone = %d %s", op.Code, op.Body)
	}
}

func TestApplyAndRemoveAnEntryOnItsCluster(t *testing.T) {
	rig := newKubeSourceRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", fmt.Sprintf(`{"name":"ack","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1"]}`, rig.provider))
	uuid := decodeKube[map[string]any](t, rec)["uuid"].(string)
	base := "/api/v1/kube/clusters/" + uuid

	dry := rig.do(rig.session, http.MethodPost, base+"/apply", `{"dry_run":true}`)
	got := decodeKube[map[string]any](t, dry)
	results, _ := got["results"].([]any)
	if dry.Code != http.StatusOK || got["dry_run"] != true || len(results) != 9 || len(rig.api.objects) != 0 {
		t.Fatalf("dry run = %d %s (objects %d)", dry.Code, dry.Body, len(rig.api.objects))
	}
	first := results[0].(map[string]any)
	if first["kind"] != "Namespace" || first["name"] != "gpuworks-system" || first["action"] != "create" {
		t.Fatalf("first result = %v", first)
	}
	if results[8].(map[string]any)["action"] != "dependent" {
		t.Fatalf("daemonset in a namespace not yet created = %v", results[8])
	}

	if rec := rig.do(rig.session, http.MethodPost, base+"/apply", `{}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "dry_run") || len(rig.api.objects) != 0 {
		t.Fatalf("apply without dry_run = %d %s; nothing may be applied by default", rec.Code, rec.Body)
	}
	applied := rig.do(rig.session, http.MethodPost, base+"/apply", `{"dry_run":false}`)
	if applied.Code != http.StatusOK || len(rig.api.objects) != 9 || !strings.Contains(applied.Body.String(), `"dry_run":false`) {
		t.Fatalf("apply = %d %s, objects %d", applied.Code, applied.Body, len(rig.api.objects))
	}
	removed := rig.do(rig.session, http.MethodPost, base+"/remove", `{"dry_run":false}`)
	if removed.Code != http.StatusOK || len(rig.api.objects) != 0 {
		t.Fatalf("remove = %d %s, objects %v", removed.Code, removed.Body, rig.api.objects)
	}

	rig.api.refuse = "clusterroles"
	failed := rig.do(rig.session, http.MethodPost, base+"/apply", `{"dry_run":false}`)
	body := decodeKube[map[string]any](t, failed)
	params, _ := body["params"].(map[string]any)
	if failed.Code != http.StatusFailedDependency || body["code"] != ErrorKubeClusterOperationFailed || params["step"] != "api_server" ||
		!strings.Contains(params["reason"].(string), "forbidden") || len(body["results"].([]any)) != 9 {
		t.Fatalf("refused apply = %d %s", failed.Code, failed.Body)
	}

	if rec := rig.do(rig.session, http.MethodPost, base+"/apply", `{"dry_run":"yes"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/nope/apply", `{"dry_run":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown entry = %d", rec.Code)
	}
	if rec := rig.do("", http.MethodPost, base+"/apply", `{"dry_run":true}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous apply = %d", rec.Code)
	}
	byHand := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"hand","server_url":"https://gw.example"}`)
	hand := decodeKube[map[string]any](t, byHand)["uuid"].(string)
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/"+hand+"/remove", `{"dry_run":true}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorKubeClusterNoSource) {
		t.Fatalf("remove for an entry made by hand = %d %s", rec.Code, rec.Body)
	}
	if _, ok := decodeKube[map[string]any](t, byHand)["source"]; ok {
		t.Fatalf("an entry made by hand has a source: %s", byHand.Body)
	}
}

func TestClustersOfAnAccountThatHasNoneOrFails(t *testing.T) {
	rig := newKubeSourceRig(t)
	rig.drv.listErr = errors.New("aliyun: Forbidden.RAM: cs:DescribeClustersV1 not allowed")
	rec := rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/providers/%d/clusters", rig.provider), "")
	if rec.Code != http.StatusFailedDependency || !strings.Contains(rec.Body.String(), "cs:DescribeClustersV1") {
		t.Fatalf("listing refused by the cloud = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/providers/999/clusters", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account = %d %s", rec.Code, rec.Body)
	}
}

// An entry made from a cloud cluster takes the registry the server suggests
// for the cluster's region (here a kind outside mainland China's clouds, so
// Docker Hub), unless the request names another.
func TestACloudEntryTakesTheSuggestedAgentImageSource(t *testing.T) {
	rig := newKubeSourceRig(t)
	rec := rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/providers/%d/clusters", rig.provider), "")
	if !strings.Contains(rec.Body.String(), `"external_id":"c-gpu"`) || !strings.Contains(rec.Body.String(), `"agent_image_source":"dockerhub"`) {
		t.Fatalf("clusters lack the suggested registry: %s", rec.Body)
	}
	suggested := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		fmt.Sprintf(`{"name":"ack-gpu","server_url":"https://gw.example","provider_pool_ids":["np-1"],"provider_id":%d,"provider_cluster_id":"c-gpu"}`, rig.provider)))
	if suggested["agent_image_source"] != "dockerhub" || suggested["effective_agent_image"] != "gpuworks/gpuworks-agent:r1" {
		t.Fatalf("entry without a registry = %v", suggested)
	}
	chosen := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		fmt.Sprintf(`{"name":"ack-other","server_url":"https://gw.example","provider_pool_ids":["np-o"],"provider_id":%d,"provider_cluster_id":"c-other","agent_image_source":"quay"}`, rig.provider)))
	if chosen["agent_image_source"] != "quay" || chosen["effective_agent_image"] != "quay.io/gpuworks/gpuworks-agent:r1" {
		t.Fatalf("entry naming a registry = %v", chosen)
	}
}
