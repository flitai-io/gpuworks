package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// newSleepableRoute is a store with one sleepable vLLM deployment served as
// "model" and its one running instance, and a gateway in front of it.
func newSleepableRoute(t *testing.T, name string) (*Proxy, *store.Store, uint, string) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode(name+"-node", store.HashToken("node"), name+"-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "model", Engine: "vllm", State: "active", Sleepable: true})
	if err != nil {
		t.Fatal(err)
	}
	uuid := name + "-instance"
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true, APIPort: 30001})
	if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{name + "-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	p.routes.rebuild()
	return p, st, dep.ID, uuid
}

// While nothing that can change an instance row was announced since the
// route snapshot was built, a request is routed from the snapshot without
// reading the instance: a row changed behind the store's back is not seen.
// The moment a change is announced - a sleep level, a deployment-wide sleep
// order, a state change, a moved port - the instance is read again and
// routing follows it, with no wait for the snapshot to be rebuilt.
func TestRoutingTrustsTheSnapshotUntilAChangeIsAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		row      map[string]any
		announce func(st *store.Store, depID uint, uuid string) error
		// want is the selection after the announcement: 503 (asleep or
		// not), or routed to port.
		status int
		asleep bool
		port   int
	}{
		{"instance sleep reported", map[string]any{"sleep_level": 1}, func(st *store.Store, _ uint, uuid string) error {
			st.Bus.PublishChange("instance", uuid, "sleep")
			return nil
		}, http.StatusServiceUnavailable, true, 0},
		{"deployment-wide sleep order", map[string]any{"sleep_level": 1}, func(st *store.Store, depID uint, _ string) error {
			return st.SetDeploymentSleep(depID, 1)
		}, http.StatusServiceUnavailable, true, 0},
		{"instance stopped", map[string]any{"state": proto.InstStopped}, func(st *store.Store, _ uint, uuid string) error {
			return st.SetInstanceState(uuid, proto.InstStopped, "stopped")
		}, http.StatusServiceUnavailable, false, 0},
		{"port moved", map[string]any{"api_port": 30002}, func(st *store.Store, _ uint, uuid string) error {
			st.Bus.PublishChange("instance", uuid, "status")
			return nil
		}, 0, false, 30002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, st, depID, uuid := newSleepableRoute(t, "trust_"+strings.ReplaceAll(tc.name, " ", "_"))
			p.routes.close() // the snapshot stays as built: only the generation moves
			if _, _, status, msg, _ := p.selectStatus("model"); status != 0 {
				t.Fatalf("awake instance not routable: %d %q", status, msg)
			}
			if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", uuid).Updates(tc.row).Error; err != nil {
				t.Fatal(err)
			}
			if _, tgt, status, msg, _ := p.selectStatus("model"); status != 0 || tgt.port != 30001 {
				t.Fatalf("unannounced row change read per request: %d %q port %d", status, msg, tgt.port)
			}
			if err := tc.announce(st, depID, uuid); err != nil {
				t.Fatal(err)
			}
			_, tgt, status, msg, asleep := p.selectStatus("model")
			if status != tc.status || asleep != tc.asleep || (tc.status == 0 && tgt.port != tc.port) {
				t.Fatalf("after the announcement = %d %q asleep=%v port %d, want %d asleep=%v port %d", status, msg, asleep, tgt.port, tc.status, tc.asleep, tc.port)
			}
		})
	}
}

// The guard reads the instance when an attempt registers only if a change
// was announced since its candidate was read; one announced in between ends
// the attempt before it reaches the engine.
func TestGuardChecksAtRegistrationOnlyAfterAnAnnouncedChange(t *testing.T) {
	_, st, _, uuid := newSleepableRoute(t, "guard_registration")
	g := &instanceGuard{store: st}
	c := candidate{instanceUUID: uuid, port: 30001, routedAt: routingGeneration(st) + 1}
	var reached int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := context.Cause(req.Context()); err != nil {
			return nil, err
		}
		reached++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	send := func() error {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp, err := g.roundTrip(base, req, c)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	// Moved behind the store's back: the attempt is not re-read.
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", uuid).Update("api_port", 30002).Error; err != nil {
		t.Fatal(err)
	}
	if err := send(); err != nil || reached != 1 {
		t.Fatalf("unchanged generation: err=%v reached=%d", err, reached)
	}
	// Announced: the registration check reads the moved port and cancels.
	st.Bus.PublishChange("instance", uuid, "status")
	if err := send(); !errors.Is(err, errInstanceUnavailable) || reached != 1 {
		t.Fatalf("announced change: err=%v reached=%d, want the attempt ended", err, reached)
	}
}

// The chat input check follows an edit of the model's declared modalities on
// the next request, and a change written without announcing one within
// modalityMaxAge.
func TestChatInputFollowsModelModalityEdits(t *testing.T) {
	p, key, closeUpstream := newProxyE2E(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"choices":[]}`) }))
	defer closeUpstream()
	now := time.Now()
	p.modalities.now = func() time.Time { return now }
	var model store.Model
	if err := p.store.DB().Where("source_id = ?", "org/omni").First(&model).Error; err != nil {
		t.Fatal(err)
	}
	audio := `{"model":"omni","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGR","format":"wav"}}]}]}`
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(audio))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		p.Handler()(rec, req)
		return rec.Code
	}
	setModalities := func(modalities string) {
		t.Helper()
		ops, _ := json.Marshal([]inferenceapi.Operation{{Type: "openai.chat"}})
		if _, err := p.store.SetModelInferenceConfig(model.ID, string(ops), modalities); err != nil {
			t.Fatal(err)
		}
	}
	setModalities(`["text","audio"]`)
	if code := send(); code != http.StatusOK {
		t.Fatalf("audio declared: %d", code)
	}
	setModalities(`["text"]`)
	if code := send(); code != http.StatusBadRequest {
		t.Fatalf("audio no longer declared, next request: %d, want 400", code)
	}
	if err := p.store.DB().Model(&store.Model{}).Where("id = ?", model.ID).Update("input_modalities", `["text","audio"]`).Error; err != nil {
		t.Fatal(err)
	}
	if code := send(); code != http.StatusBadRequest {
		t.Fatalf("unannounced write within the max age: %d, want the cached 400", code)
	}
	now = now.Add(modalityMaxAge)
	if code := send(); code != http.StatusOK {
		t.Fatalf("unannounced write after the max age: %d, want 200", code)
	}
}
