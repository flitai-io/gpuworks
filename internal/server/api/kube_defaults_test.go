package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// The cluster form takes its defaults, limits and vendors from here and keeps
// no copy of them. The wire body is pinned literally: every key, in order.
func TestKubeDefaultsDictionaryOnTheWire(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/defaults", "")
	want := `{"namespace":"gpuworks-system","state_dir":"/opt/gpuworks","engine_ports":"40000-42767",` +
		`"idle_return_seconds":60,"idle_return_keep":-1,"agent_image":"gpuworks/gpuworks-agent:r1","stalled_after_seconds":300,` +
		`"agent_image_source":"dockerhub","agent_image_sources":[{"id":"dockerhub","image":"gpuworks/gpuworks-agent:r1"},` +
		`{"id":"swr","image":"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1"},{"id":"quay","image":"quay.io/gpuworks/gpuworks-agent:r1"}],"priority":10000,"priority_max":1000000000,"allow_preemption":false}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("defaults = %d %s\nwant %s", rec.Code, rec.Body, want)
	}
	if rec := rig.do("", http.MethodGet, "/api/v1/kube/defaults", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous defaults = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/defaults", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST defaults = %d", rec.Code)
	}
}

// What the dictionary says is what a request that leaves a field empty gets.
func TestKubeDefaultsAreWhatAnEmptyRequestTakes(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"bare","server_url":"https://gw.example"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	got := decodeKube[map[string]any](t, rec)
	dict := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodGet, "/api/v1/kube/defaults", ""))
	for _, key := range []string{"namespace", "state_dir", "engine_ports", "idle_return_seconds", "priority", "allow_preemption"} {
		if got[key] != dict[key] {
			t.Errorf("%s: a bare request got %v, the dictionary says %v", key, got[key], dict[key])
		}
	}
	// The agent image is not frozen into the entry: it stays empty and the
	// manifest runs the dictionary's image at the server's runtime revision.
	if got["agent_image"] != "" {
		t.Errorf("agent_image: a bare request stored %v, want it left empty", got["agent_image"])
	}
	if got["agent_image_source"] != dict["agent_image_source"] || got["effective_agent_image"] != dict["agent_image"] {
		t.Errorf("a bare request: source %v image %v, the dictionary says %v %v", got["agent_image_source"], got["effective_agent_image"], dict["agent_image_source"], dict["agent_image"])
	}
	manifest := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+got["uuid"].(string)+"/manifest", "")
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Body.String(), fmt.Sprintf("image: %q", dict["agent_image"])) {
		t.Errorf("manifest of a bare entry = %d, want it to run %v:\n%s", manifest.Code, dict["agent_image"], manifest.Body)
	}
	// An entry names no vendor: a RuntimeClass is kept for whichever of its
	// nodes are NVIDIA's, and a vendor in the request is ignored.
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		`{"name":"rc","server_url":"https://gw.example","vendor":"ascend","runtime_class":"special"}`))
	if created["runtime_class"] != "special" {
		t.Errorf("runtime_class = %v, want it kept", created["runtime_class"])
	}
	if _, ok := created["vendor"]; ok {
		t.Errorf("the entry names a vendor: %v", created)
	}
	if vendors, ok := created["vendors"].([]any); !ok || len(vendors) != 0 {
		t.Errorf("vendors of an entry no node joined = %v, want []", created["vendors"])
	}
}

// A cluster no node joined for longer than the dictionary's stalled_after is
// stalled; revoked clusters and ones a node joined never are.
func TestKubeClusterStalledIsComputedByTheServer(t *testing.T) {
	rig := newTokenRig(t)
	make := func(name string) store.KubeCluster {
		c, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return *c
	}
	age := func(c store.KubeCluster, by time.Duration) {
		if err := rig.st.DB().Model(&store.KubeCluster{}).Where("id = ?", c.ID).Update("created_at", time.Now().Add(-by)).Error; err != nil {
			t.Fatal(err)
		}
	}
	fresh := make("fresh")
	waiting := make("waiting")
	age(waiting, kubeStalledAfter+time.Minute)
	almost := make("almost")
	age(almost, kubeStalledAfter-time.Minute)
	enrolled := make("enrolled")
	age(enrolled, time.Hour)
	if _, _, err := rig.st.EnrollKubeNode(enrolled.Token, "uid-1", "gpu-1"); err != nil {
		t.Fatal(err)
	}
	revoked := make("revoked")
	age(revoked, time.Hour)
	if err := rig.st.RevokeKubeCluster(revoked.UUID); err != nil {
		t.Fatal(err)
	}

	rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	list := decodeKube[struct {
		Items []struct {
			Name    string `json:"name"`
			Stalled bool   `json:"stalled"`
			State   string `json:"state"`
		} `json:"items"`
	}](t, rec)
	got := map[string]bool{}
	states := map[string]string{}
	for _, it := range list.Items {
		got[it.Name] = it.Stalled
		states[it.Name] = it.State
	}
	want := map[string]bool{"fresh": false, "waiting": true, "almost": false, "enrolled": false, "revoked": false}
	for name, stalled := range want {
		if has, ok := got[name]; !ok || has != stalled {
			t.Errorf("%s: stalled = %v (listed %v), want %v; body %s", name, has, ok, stalled, rec.Body)
		}
	}
	// Where each entry stands is one value the console shows and filters by.
	wantStates := map[string]string{"fresh": "pending", "waiting": "stalled", "almost": "pending", "enrolled": "active", "revoked": "revoked"}
	for name, state := range wantStates {
		if states[name] != state {
			t.Errorf("%s: state = %q, want %q; body %s", name, states[name], state, rec.Body)
		}
	}
	// A cluster connected on its own is filed under the Kubernetes platform type and connects a cluster.
	if !strings.Contains(rec.Body.String(), `"stalled":false,"state":"pending","category":"kubernetes","resources":[{"type":"kubernetes"}],`) {
		t.Errorf("an entry lacks its state, category or resources: %s", rec.Body)
	}
	_ = fresh
}

// Whether the server can apply or remove an entry is its own word, with the
// reason: by hand has no way in, a private API server cannot be reached, a
// revoked entry can only be removed. A cloud that cannot be asked leaves the
// entry applicable (the operation reports the failure), and the cloud is asked
// once for however many polls.
func TestKubeClusterOperationsAreDecidedByTheServer(t *testing.T) {
	rig := newKubeSourceRig(t)
	create := func(name, body string) map[string]any {
		t.Helper()
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"`+name+`","server_url":"https://gw.example"`+body+`}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, rec.Code, rec.Body)
		}
		return decodeKube[map[string]any](t, rec)
	}
	from := func(cluster string) string {
		return fmt.Sprintf(`,"provider_id":%d,"provider_cluster_id":%q,"provider_pool_ids":[%q]`, rig.provider, cluster, map[string]string{"c-gpu": "np-1", "c-private": "np-p", "c-other": "np-o"}[cluster])
	}
	pub := create("pub", from("c-gpu"))
	priv := create("priv", from("c-private"))
	hand := create("hand", "")
	gone := create("revoked-pub", from("c-other"))
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/"+gone["uuid"].(string)+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}

	ops := func(c map[string]any) string {
		return fmt.Sprintf("apply=%v remove=%v blocked=%v", c["can_apply"], c["can_remove"], c["operation_blocked"])
	}
	if got := ops(pub); got != "apply=true remove=true blocked=<nil>" {
		t.Errorf("a cluster with a public API: %s", got)
	}
	if got := ops(priv); got != "apply=false remove=false blocked=private_api" {
		t.Errorf("a cluster the cloud says is private, as created: %s", got)
	}
	if got := ops(hand); got != "apply=false remove=false blocked=no_source" {
		t.Errorf("an entry made by hand: %s", got)
	}

	rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	list := decodeKube[struct {
		Items []map[string]any `json:"items"`
	}](t, rec)
	byName := map[string]string{}
	for _, it := range list.Items {
		byName[it["name"].(string)] = ops(it)
	}
	for name, want := range map[string]string{
		"pub":         "apply=true remove=true blocked=<nil>",
		"priv":        "apply=false remove=false blocked=private_api",
		"hand":        "apply=false remove=false blocked=no_source",
		"revoked-pub": "apply=false remove=true blocked=revoked",
	} {
		if byName[name] != want {
			t.Errorf("list %s: %s, want %s", name, byName[name], want)
		}
	}

	before := rig.drv.lists.Load()
	for i := 0; i < 5; i++ {
		rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	}
	if after := rig.drv.lists.Load(); after != before {
		t.Errorf("five more polls asked the cloud %d more times; its answer is kept for a minute", after-before)
	}
}

func TestKubeClusterOperationsWhenTheCloudCannotBeAsked(t *testing.T) {
	rig := newKubeSourceRig(t)
	// An entry made while the cloud answered, read while it does not.
	provider := rig.provider
	if _, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "unsure", Namespace: "gpuworks-system", ProviderID: &provider, ProviderClusterID: "c-private"},
		store.KubeClusterPool{PoolID: "np-p", LabelKey: "alibabacloud.com/nodepool-id"}); err != nil {
		t.Fatal(err)
	}
	rig.drv.listErr = errors.New("aliyun: Forbidden.RAM")
	list := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"can_apply":true,"can_remove":true`) || strings.Contains(list.Body.String(), "operation_blocked") {
		t.Fatalf("without the cloud's word the entry stays applicable: %d %s", list.Code, list.Body)
	}
	// Making an entry needs the cloud: its pools are checked against it.
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		fmt.Sprintf(`{"name":"new","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1"]}`, rig.provider))
	if rec.Code != http.StatusFailedDependency || !strings.Contains(rec.Body.String(), "Forbidden.RAM") {
		t.Fatalf("create while the cloud cannot be asked = %d %s", rec.Code, rec.Body)
	}
}

// An entry that names no agent image runs the published image at the
// server's runtime revision: a server upgrade does not move its tag, since
// the agents are upgraded on their nodes, and an entry's own image is kept.
func TestABareEntryRunsTheRuntimeRevisionAcrossServerVersions(t *testing.T) {
	rig := newTokenRig(t)
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = "v26.11.1"
	created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"follow","server_url":"https://gw.example"}`))
	pinned := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		`{"name":"pinned","server_url":"https://gw.example","agent_image":"registry.example/gpuworks-agent:mine"}`))
	version.Version = "v26.11.2"
	for _, tc := range []struct {
		uuid, want string
	}{
		{created["uuid"].(string), `image: "gpuworks/gpuworks-agent:` + kubemanifest.AgentImageRevision + `"`},
		{pinned["uuid"].(string), `image: "registry.example/gpuworks-agent:mine"`},
	} {
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+tc.uuid+"/manifest", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("manifest = %d, want %q:\n%s", rec.Code, tc.want, rec.Body)
		}
	}
}

// An entry made by hand names the registry its nodes reach; the manifest
// runs the published image from there, and a registry the image is not
// published to is refused.
func TestKubeAgentImageSourceChoosesTheRegistry(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"cn","server_url":"https://gw.example","agent_image_source":"swr"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	got := decodeKube[map[string]any](t, rec)
	const swr = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1"
	if got["agent_image_source"] != "swr" || got["effective_agent_image"] != swr {
		t.Fatalf("entry = %v", got)
	}
	manifest := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+got["uuid"].(string)+"/manifest", "")
	if !strings.Contains(manifest.Body.String(), fmt.Sprintf("image: %q", swr)) {
		t.Fatalf("manifest does not run %s:\n%s", swr, manifest.Body)
	}
	// An image of its own wins over the registry.
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"own","server_url":"https://gw.example","agent_image_source":"swr","agent_image":"registry.example/agent:r1"}`)
	if got := decodeKube[map[string]any](t, rec); got["effective_agent_image"] != "registry.example/agent:r1" {
		t.Fatalf("own image entry = %v", got)
	}
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"bad","server_url":"https://gw.example","agent_image_source":"ghcr"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"agent_image_source"`) {
		t.Fatalf("unknown registry = %d %s", rec.Code, rec.Body)
	}
}

func TestKubeAgentImageSourceForACloudClusterFollowsItsRegion(t *testing.T) {
	for _, tc := range []struct{ kind, region, want string }{
		{"aliyun", "cn-shanghai", "swr"},
		{"aliyun", "cn-hangzhou", "swr"},
		{"aliyun", "cn-hongkong", "dockerhub"},
		{"aliyun", "ap-southeast-1", "dockerhub"},
		{"digitalocean", "sgp1", "dockerhub"},
		{"", "cn-shanghai", "dockerhub"},
	} {
		if got := kubeAgentImageSourceFor(tc.kind, tc.region); got != tc.want {
			t.Errorf("kubeAgentImageSourceFor(%q, %q) = %q, want %q", tc.kind, tc.region, got, tc.want)
		}
	}
}

// The priority and preemption the request names reach the manifest; a
// priority out of range is refused at its field.
func TestKubePriorityAndPreemptionReachTheManifest(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"pri","server_url":"https://gw.example","priority":0,"allow_preemption":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	got := decodeKube[map[string]any](t, rec)
	if got["priority"] != float64(0) || got["allow_preemption"] != true {
		t.Fatalf("entry = priority %v allow_preemption %v", got["priority"], got["allow_preemption"])
	}
	m := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+got["uuid"].(string)+"/manifest", "").Body.String()
	if !strings.Contains(m, `name: "gpuworks-inference-gpuworks-system"`+"\nvalue: 0\nglobalDefault") {
		t.Fatalf("manifest PriorityClass:\n%s", m)
	}
	bare := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"bare-pri","server_url":"https://gw.example","namespace":"gw-b"}`))
	m = rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+bare["uuid"].(string)+"/manifest", "").Body.String()
	if !strings.Contains(m, "value: 10000\npreemptionPolicy: Never") {
		t.Fatalf("a bare entry's PriorityClass:\n%s", m)
	}
	for _, bad := range []string{"-1", "1000000001"} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"bad-`+bad+`","server_url":"https://gw.example","priority":`+bad+`}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"priority"`) {
			t.Fatalf("priority %s = %d %s", bad, rec.Code, rec.Body)
		}
	}
}
