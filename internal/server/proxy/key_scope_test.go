package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

type keyScopeRig struct {
	p            *Proxy
	st           *store.Store
	alpha, beta  uint
	upstreamHits *atomic.Int64
}

// newKeyScopeRig serves two models, alpha and beta, one deployment each.
func newKeyScopeRig(t *testing.T) *keyScopeRig {
	t.Helper()
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(upstream.Close)
	st, err := store.Open("file:proxy_key_scope_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-scope", store.HashToken("node"), "node-scope")
	if err != nil {
		t.Fatal(err)
	}
	rig := &keyScopeRig{st: st, upstreamHits: &hits}
	for i, name := range []string{"alpha", "beta"} {
		dep, err := st.CreateDeployment(&store.Deployment{Name: name, ModelID: model.ID, NodeID: node.ID, ServedName: name, Engine: "vllm", State: "active"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{UUID: name + "-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000 + i, Ports: "[30000]"}); err != nil {
			t.Fatal(err)
		}
		if name == "alpha" {
			rig.alpha = dep.ID
		} else {
			rig.beta = dep.ID
		}
	}
	addr := strings.TrimPrefix(upstream.URL, "http://")
	rig.p = New(st, storeNetworks(st), dialTestHub{addr: addr}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(rig.p.Close)
	return rig
}

func (r *keyScopeRig) chat(t *testing.T, key, model string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	r.p.Handler()(rec, req)
	return rec.Code, rec.Body.String()
}

func (r *keyScopeRig) models(t *testing.T, key string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	r.p.ModelsHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestScopedKeyReachesOnlyItsDeployments(t *testing.T) {
	rig := newKeyScopeRig(t)
	open, err := rig.st.CreateAPIKey("open", 0)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := rig.st.CreateScopedAPIKey("scoped", 0, store.APIKeyScope{Restricted: true, DeploymentIDs: []uint{rig.alpha}})
	if err != nil {
		t.Fatal(err)
	}

	for _, model := range []string{"alpha", "beta"} {
		if code, body := rig.chat(t, open, model); code != http.StatusOK {
			t.Fatalf("open key, %s = %d %s", model, code, body)
		}
	}
	if code, body := rig.chat(t, scoped, "alpha"); code != http.StatusOK {
		t.Fatalf("scoped key, alpha = %d %s", code, body)
	}
	before := rig.upstreamHits.Load()
	code, body := rig.chat(t, scoped, "beta")
	if code != http.StatusForbidden || !strings.Contains(body, "may not call model beta") || !strings.Contains(body, `"type":"gpuworks_proxy"`) {
		t.Fatalf("scoped key, beta = %d %s", code, body)
	}
	if rig.upstreamHits.Load() != before {
		t.Fatal("a refused request reached the engine")
	}
	// An unknown name stays 404 for a scoped key: scope is not existence.
	if code, _ := rig.chat(t, scoped, "gamma"); code != http.StatusNotFound {
		t.Fatalf("scoped key, unknown model = %d", code)
	}
	if got := rig.models(t, open); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("open key models = %v", got)
	}
	if got := rig.models(t, scoped); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("scoped key models = %v", got)
	}

	// An edit applies to the key's next request.
	var k store.APIKey
	if err := rig.st.DB().Where("name = ?", "scoped").First(&k).Error; err != nil {
		t.Fatal(err)
	}
	if err := rig.st.SetAPIKeyScope(k.Prefix, store.APIKeyScope{Restricted: true, DeploymentIDs: []uint{rig.beta}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := rig.chat(t, scoped, "alpha"); code != http.StatusForbidden {
		t.Fatalf("after the edit, alpha = %d", code)
	}
	if code, body := rig.chat(t, scoped, "beta"); code != http.StatusOK {
		t.Fatalf("after the edit, beta = %d %s", code, body)
	}
	if got := rig.models(t, scoped); !slices.Equal(got, []string{"beta"}) {
		t.Fatalf("models after the edit = %v", got)
	}
}

// A batch's requests carry only the key's ID; they get its scope at send
// time.
func TestInProcessRequestsKeepTheirKeyScope(t *testing.T) {
	rig := newKeyScopeRig(t)
	if _, err := rig.st.CreateScopedAPIKey("batch", 0, store.APIKeyScope{Restricted: true, DeploymentIDs: []uint{rig.alpha}}); err != nil {
		t.Fatal(err)
	}
	var k store.APIKey
	if err := rig.st.DB().Where("name = ?", "batch").First(&k).Error; err != nil {
		t.Fatal(err)
	}
	caller := uxrecord.Caller{APIKeyID: k.ID, Source: uxrecord.SourceBatch}
	send := func(model string) InProcessReply {
		return rig.p.Do(context.Background(), caller, "/v1/chat/completions", []byte(`{"model":"`+model+`","messages":[]}`))
	}
	if r := send("alpha"); r.Status != http.StatusOK || r.Err != nil {
		t.Fatalf("alpha = %+v", r)
	}
	if r := send("beta"); r.Status != http.StatusForbidden {
		t.Fatalf("beta = %d %s", r.Status, r.Body)
	}
	if err := rig.st.SetAPIKeyScope(k.Prefix, store.APIKeyScope{}); err != nil {
		t.Fatal(err)
	}
	if r := send("beta"); r.Status != http.StatusOK {
		t.Fatalf("beta after widening = %d %s", r.Status, r.Body)
	}
	// A key that does not exist is a failure to send, never an open door.
	caller.APIKeyID = 9999
	if r := send("alpha"); r.Err == nil || r.Status != 0 {
		t.Fatalf("unknown key = %+v", r)
	}
}

// Two deployments may share a served name during a rollout; a scoped key
// only ever routes among its own, even when another one could serve.
func TestScopedSelectionAmongDeploymentsSharingAName(t *testing.T) {
	st, err := store.Open("file:proxy_key_scope_shared?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("shared-node", store.HashToken("node"), "shared-node")
	if err != nil {
		t.Fatal(err)
	}
	serving, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "shared", Engine: "vllm", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "serving", DeploymentID: serving.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001}); err != nil {
		t.Fatal(err)
	}
	pending := legacySharedNameDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "shared", Engine: "vllm", State: "active"})
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"shared-node": true}}, nil, 0)
	t.Cleanup(p.Close)

	onlyPending := &store.APIKeyIdentity{Restricted: true, Deployments: map[uint]struct{}{pending.ID: {}}}
	dep, _, status, _, _ := p.selectScopedDeploymentInstance(onlyPending, "shared", "")
	if status != http.StatusServiceUnavailable || dep == nil || dep.ID != pending.ID {
		t.Fatalf("only pending: status=%d dep=%+v, want 503 from the pending deployment", status, dep)
	}
	onlyServing := &store.APIKeyIdentity{Restricted: true, Deployments: map[uint]struct{}{serving.ID: {}}}
	if dep, tgt, status, _, _ := p.selectScopedDeploymentInstance(onlyServing, "shared", ""); status != 0 || dep.ID != serving.ID || tgt.port != 30001 {
		t.Fatalf("only serving: status=%d dep=%+v", status, dep)
	}
	none := &store.APIKeyIdentity{Restricted: true}
	if dep, _, status, msg, _ := p.selectScopedDeploymentInstance(none, "shared", ""); status != http.StatusForbidden || dep != nil || !strings.Contains(msg, "shared") {
		t.Fatalf("no grant: status=%d dep=%+v msg=%q", status, dep, msg)
	}
}
