package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestKubeRolloutOfACloudEntryOverTheWire(t *testing.T) {
	rig := newKubeSourceRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", fmt.Sprintf(`{"name":"ack","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1"]}`, rig.provider))
	uuid := decodeKube[map[string]any](t, rec)["uuid"].(string)
	path := "/api/v1/kube/clusters/" + uuid + "/rollout"

	// Not applied yet: there is no DaemonSet, which is an answer.
	rec = rig.do(rig.session, http.MethodGet, path, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) !=
		`{"done":true,"desired":null,"rows":[{"code":"DAEMONSET_MISSING","detail":"no DaemonSet gpuworks-agent in namespace gpuworks-system"}]}` {
		t.Fatalf("before apply = %d %s", rec.Code, rec.Body)
	}

	// One node runs its agent, the other is still pulling: ask again later.
	rig.api.mu.Lock()
	rig.api.reads = map[string]string{
		"/apis/apps/v1/namespaces/gpuworks-system/daemonsets/gpuworks-agent": `{"status":{"observedGeneration":1,"desiredNumberScheduled":2}}`,
		"/api/v1/namespaces/gpuworks-system/pods": `{"items":[` +
			`{"metadata":{"name":"gpuworks-agent-a"},"spec":{"nodeName":"n1"},"status":{"containerStatuses":[{"ready":true,"state":{"running":{}}}]}},` +
			`{"metadata":{"name":"gpuworks-agent-b"},"spec":{"nodeName":"n2"},"status":{"containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"ContainerCreating"}}}]}}]}`,
	}
	rig.api.mu.Unlock()
	rec = rig.do(rig.session, http.MethodGet, path, "")
	want := `{"done":false,"poll_after_seconds":10,"desired":2,"rows":[` +
		`{"node":"n1","pod":"gpuworks-agent-a","code":"OK"},` +
		`{"node":"n2","pod":"gpuworks-agent-b","code":"STILL_STARTING","detail":"still pulling the image or starting"}]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("rollout = %d %s\nwant %s", rec.Code, rec.Body, want)
	}
	// It only reads.
	for p := range rig.api.objects {
		t.Errorf("the rollout wrote %s", p)
	}
}

func TestKubeRolloutRefusals(t *testing.T) {
	rig := newKubeSourceRig(t)
	// An entry made by hand: the server has no way into its cluster.
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", kubeCreateBody)
	uuid := decodeKube[map[string]any](t, rec)["uuid"].(string)
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+uuid+"/rollout", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"KUBE_CLUSTER_NO_SOURCE"`) {
		t.Fatalf("hand-made entry = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/nope/rollout", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown entry = %d", rec.Code)
	}
	if rec := rig.do("", http.MethodGet, "/api/v1/kube/clusters/"+uuid+"/rollout", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	// The cluster refuses the read: what it said, and at which step.
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", fmt.Sprintf(`{"name":"ack","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1"]}`, rig.provider))
	cloud := decodeKube[map[string]any](t, rec)["uuid"].(string)
	rig.api.mu.Lock()
	rig.api.reads = map[string]string{"/apis/apps/v1/namespaces/gpuworks-system/daemonsets/gpuworks-agent": `{"status":{"observedGeneration":1,"desiredNumberScheduled":1}}`}
	rig.api.mu.Unlock()
	rec = rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+cloud+"/rollout", "")
	if rec.Code != statusProviderFailed || !strings.Contains(rec.Body.String(), `"code":"KUBE_CLUSTER_OPERATION_FAILED"`) || !strings.Contains(rec.Body.String(), `"step":"api_server"`) {
		t.Fatalf("refused = %d %s", rec.Code, rec.Body)
	}
}
