package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// createFromPools makes an entry from the account's cluster c-gpu holding the
// given pools and answers its uuid and apply token.
func (rig *kubeSourceRig) createFromPools(name string, pools ...string) (uuid, token string) {
	rig.t.Helper()
	ids, _ := json.Marshal(pools)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		fmt.Sprintf(`{"name":%q,"server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":%s}`, name, rig.provider, ids))
	if rec.Code != http.StatusCreated {
		rig.t.Fatalf("create %s = %d %s", name, rec.Code, rec.Body)
	}
	created := decodeKube[map[string]any](rig.t, rec)
	apply := created["apply_command"].(string)
	token = apply[strings.Index(apply, "token=")+len("token="):]
	token = strings.TrimSuffix(token, "'")
	token, _ = url.QueryUnescape(token)
	return created["uuid"].(string), token
}

func (rig *kubeSourceRig) manifest(token string) string {
	rig.t.Helper()
	rec := rig.do("", http.MethodGet, "/kube/manifest.yaml?token="+url.QueryEscape(token), "")
	if rec.Code != http.StatusOK {
		rig.t.Fatalf("manifest = %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func (rig *kubeSourceRig) poolsOf(uuid string) string {
	rig.t.Helper()
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	list := decodeKube[struct {
		Items []struct {
			UUID    string   `json:"uuid"`
			PoolIDs []string `json:"pool_ids"`
		} `json:"items"`
	}](rig.t, rec)
	for _, it := range list.Items {
		if it.UUID == uuid {
			got, _ := json.Marshal(it.PoolIDs)
			return string(got)
		}
	}
	rig.t.Fatalf("entry %s is not listed", uuid)
	return ""
}

// One entry holds several GPU node pools of its cluster, and the manifest
// selects exactly them with a required nodeAffinity over the pool-id label.
func TestAnEntryHoldsSeveralPoolsAndTheManifestSelectsThem(t *testing.T) {
	rig := newKubeSourceRig(t)
	uuid, token := rig.createFromPools("ack", "np-2", "np-1")
	if got := rig.poolsOf(uuid); got != `["np-1","np-2"]` {
		t.Fatalf("pool_ids = %s", got)
	}
	m := rig.manifest(token)
	for _, want := range []string{"nodeAffinity:", "requiredDuringSchedulingIgnoredDuringExecution:", `key: "alibabacloud.com/nodepool-id"`, "operator: In", `- "np-1"`, `- "np-2"`} {
		if !strings.Contains(m, want) {
			t.Fatalf("manifest lacks %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, `"np-ppu"`) || strings.Contains(m, "nodeSelector:") {
		t.Fatalf("manifest selects what the entry does not hold:\n%s", m)
	}
	// The admin's manifest endpoint is the same document.
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters/"+uuid+"/manifest", ""); rec.Body.String() != m {
		t.Fatalf("admin manifest differs from the token's")
	}
	// An entry made by hand keeps its free-form selector and has no pools.
	hand := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"hand","server_url":"https://gw.example","node_selector":"gpu=a"}`)
	body := decodeKube[map[string]any](t, hand)
	if !strings.Contains(hand.Body.String(), `"pool_ids":[]`) || body["node_selector"] != "gpu=a" {
		t.Fatalf("an entry by hand = %s", hand.Body)
	}
	apply := body["apply_command"].(string)
	handToken, _ := url.QueryUnescape(strings.TrimSuffix(apply[strings.Index(apply, "token=")+6:], "'"))
	if hm := rig.manifest(handToken); !strings.Contains(hm, "nodeSelector:") || strings.Contains(hm, "nodeAffinity") {
		t.Fatalf("manifest of an entry by hand:\n%s", hm)
	}
}

// Pools of different vendors share one entry: each agent finds out which
// accelerator its node has, so the manifest selects the pools and nothing
// else, on create and on edit.
func TestAnEntryHoldsPoolsOfSeveralVendors(t *testing.T) {
	rig := newKubeSourceRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		fmt.Sprintf(`{"name":"mixed","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1","np-ppu"]}`, rig.provider))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mixed create = %d %s", rec.Code, rec.Body)
	}
	created := decodeKube[map[string]any](t, rec)
	if _, ok := created["vendor"]; ok {
		t.Fatalf("an entry still names a vendor: %s", rec.Body)
	}
	uuid := created["uuid"].(string)
	if got := rig.poolsOf(uuid); got != `["np-1","np-ppu"]` {
		t.Fatalf("pools = %s", got)
	}
	apply := created["apply_command"].(string)
	token, _ := url.QueryUnescape(strings.TrimSuffix(apply[strings.Index(apply, "token=")+6:], "'"))
	m := rig.manifest(token)
	if !strings.Contains(m, `- "np-1"`) || !strings.Contains(m, `- "np-ppu"`) || strings.Contains(m, "/usr/local") {
		t.Fatalf("manifest of a mixed entry:\n%s", m)
	}
	edit := rig.do(rig.session, http.MethodPut, "/api/v1/kube/clusters/"+uuid+"/pools", `{"provider_pool_ids":["np-ppu"]}`)
	if edit.Code != http.StatusOK || rig.poolsOf(uuid) != `["np-ppu"]` {
		t.Fatalf("edit to another vendor's pool = %d %s", edit.Code, edit.Body)
	}
}

// The pool set of an entry can be edited; the manifest follows at once (the
// cluster follows when it is applied again), and the cloud listing says which
// pools the entry holds.
func TestEditingTheNodePoolsOfAnEntry(t *testing.T) {
	rig := newKubeSourceRig(t)
	uuid, token := rig.createFromPools("ack", "np-1")
	path := "/api/v1/kube/clusters/" + uuid + "/pools"

	rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2","np-1"," np-2 "]}`)
	updated := decodeKube[map[string]any](t, rec)
	if rec.Code != http.StatusOK || updated["uuid"] != uuid || updated["can_apply"] != true {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	if got, _ := json.Marshal(updated["pool_ids"]); string(got) != `["np-1","np-2"]` {
		t.Fatalf("pool_ids = %s; duplicates and blanks collapse", got)
	}
	if m := rig.manifest(token); !strings.Contains(m, `- "np-2"`) {
		t.Fatalf("the manifest did not follow the edit:\n%s", m)
	}
	inEntry := func() string {
		rec := rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/providers/%d/clusters", rig.provider), "")
		list := decodeKube[struct {
			Items []struct {
				ExternalID string `json:"external_id"`
				GPUPools   []struct {
					ID      string `json:"id"`
					InEntry bool   `json:"in_entry"`
				} `json:"gpu_pools"`
			} `json:"items"`
		}](t, rec)
		var held []string
		for _, c := range list.Items {
			for _, p := range c.GPUPools {
				if p.InEntry {
					held = append(held, c.ExternalID+"/"+p.ID)
				}
			}
		}
		return strings.Join(held, ",")
	}
	if got := inEntry(); got != "c-gpu/np-1,c-gpu/np-2" {
		t.Fatalf("pools in the entry = %s", got)
	}
	// Taking one away.
	if rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":["np-2"]}`); rec.Code != http.StatusOK {
		t.Fatalf("shrink = %d %s", rec.Code, rec.Body)
	}
	if got := inEntry(); got != "c-gpu/np-2" || rig.poolsOf(uuid) != `["np-2"]` {
		t.Fatalf("after shrinking: in entry %s, pools %s", got, rig.poolsOf(uuid))
	}
	if m := rig.manifest(token); strings.Contains(m, `"np-1"`) {
		t.Fatalf("the manifest still selects the pool taken away:\n%s", m)
	}

	for name, tc := range map[string]struct{ body, want string }{
		"none":                      {`{"provider_pool_ids":[]}`, "provider_pool_ids"},
		"only blanks":               {`{"provider_pool_ids":[" "]}`, "provider_pool_ids"},
		"unknown pool":              {`{"provider_pool_ids":["np-nope"]}`, "np-nope"},
		"a pool of another cluster": {`{"provider_pool_ids":["np-o"]}`, "np-o"},
	} {
		rec := rig.do(rig.session, http.MethodPut, path, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorKubeClusterInvalid) || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rig.poolsOf(uuid) != `["np-2"]` {
		t.Fatalf("a refused edit changed the pools: %s", rig.poolsOf(uuid))
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/kube/clusters/nope/pools", `{"provider_pool_ids":["np-1"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown entry = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPut, path, `{"provider_pool_ids":"np-1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d", rec.Code)
	}
	if rec := rig.do("", http.MethodPut, path, `{"provider_pool_ids":["np-1"]}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous edit = %d", rec.Code)
	}
	viewer, err := managementSessionTestToken(t, rig.st, rig.signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(viewer, http.MethodPut, path, `{"provider_pool_ids":["np-1"]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-admin's edit = %d", rec.Code)
	}
	hand := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"hand","server_url":"https://gw.example"}`))
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/kube/clusters/"+hand["uuid"].(string)+"/pools", `{"provider_pool_ids":["np-1"]}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), ErrorKubeClusterNoSource) {
		t.Fatalf("pools of an entry made by hand = %d %s", rec.Code, rec.Body)
	}
}

// The pools an entry holds outlive its cloud account: the label they are
// selected by is kept with them, so the manifest still renders.
func TestAnEntrysPoolsOutliveItsAccount(t *testing.T) {
	rig := newKubeSourceRig(t)
	uuid, token := rig.createFromPools("ack", "np-1", "np-2")
	if rec := rig.do(rig.session, http.MethodDelete, fmt.Sprintf("/api/v1/providers/%d", rig.provider), ""); rec.Code/100 != 2 {
		t.Fatalf("delete account = %d %s", rec.Code, rec.Body)
	}
	if m := rig.manifest(token); !strings.Contains(m, `key: "alibabacloud.com/nodepool-id"`) || !strings.Contains(m, `- "np-2"`) {
		t.Fatalf("manifest after the account is gone:\n%s", m)
	}
	if rig.poolsOf(uuid) != `["np-1","np-2"]` {
		t.Fatalf("pool_ids = %s", rig.poolsOf(uuid))
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/kube/clusters/"+uuid+"/pools", `{"provider_pool_ids":["np-1"]}`); rec.Code != http.StatusConflict {
		t.Fatalf("editing pools without an account = %d %s", rec.Code, rec.Body)
	}
}

// An entry from before pools selected by a free-form selector; editing its
// pools replaces that selector.
func TestEditingThePoolsOfAnEntryWithALegacySelectorReplacesIt(t *testing.T) {
	rig := newKubeSourceRig(t)
	provider := rig.provider
	c, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "legacy", Namespace: "gpuworks-system", ServerURL: "https://gw.example", AgentImage: "a:1",
		NodeSelector: "alibabacloud.com/nodepool-id=np-1,gpu=l20", EnginePorts: "40000-42767", ProviderID: &provider, ProviderClusterID: "c-gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if m := rig.manifest(c.Token); !strings.Contains(m, "nodeSelector:") || strings.Contains(m, "nodeAffinity") {
		t.Fatalf("a legacy entry selects by its selector:\n%s", m)
	}
	if rec := rig.do(rig.session, http.MethodPut, "/api/v1/kube/clusters/"+c.UUID+"/pools", `{"provider_pool_ids":["np-1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	m := rig.manifest(c.Token)
	if strings.Contains(m, "nodeSelector:") || !strings.Contains(m, "nodeAffinity:") {
		t.Fatalf("after editing its pools the entry selects by them alone:\n%s", m)
	}
}
