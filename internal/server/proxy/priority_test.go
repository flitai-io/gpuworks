package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

type priorityRig struct {
	p  *Proxy
	st *store.Store
	// deployments are the rig's deployment ids by name.
	deployments map[string]uint
	mu          sync.Mutex
	// seen is what the engine got: its priority field (absent: nil) and
	// whether vLLM's header reached it.
	seen []map[string]any
	hdr  []string
	raw  []string
	// bench is the benchmark-run header the engine got, which it never should.
	bench []string
}

// newPriorityRig serves four deployments on one engine stub: "v" (vLLM with
// priority scheduling), "s" (SGLang with it), "off" (vLLM without) and
// "unlaunched" (asks for it, but its instance was launched without the flag,
// as on a target that does not allow it).
func newPriorityRig(t testing.TB) *priorityRig {
	t.Helper()
	rig := &priorityRig{deployments: map[string]uint{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		// As vLLM: a priority that is not an integer is a 400, the shape
		// the engine answers (and SGLang's server dies on a huge one).
		if v, present := body["priority"]; present {
			if f, ok := v.(float64); !ok || f != float64(int64(f)) || f > 1e15 || f < -1e15 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"1 validation error: body.priority","code":400}}`)
				return
			}
		}
		rig.mu.Lock()
		rig.seen = append(rig.seen, body)
		rig.hdr = append(rig.hdr, r.Header.Get("X-Vllm-Priority"))
		rig.raw = append(rig.raw, string(raw))
		rig.bench = append(rig.bench, r.Header.Get(loadgen.BenchmarkRunHeader))
		rig.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(upstream.Close)
	st, err := store.Open("file:proxy_priority_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-prio", store.HashToken("node"), "node-prio")
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range []struct {
		name, engine string
		on           bool
		spec         string
	}{
		{"v", "vllm", true, `{"argv":["vllm","serve","--scheduling-policy","priority"]}`},
		{"s", "sglang", true, `{"argv":["python","-m","sglang.launch_server","--enable-priority-scheduling","--default-priority-value","0"]}`},
		{"off", "vllm", false, `{"argv":["vllm","serve"]}`},
		{"unlaunched", "vllm", true, `{"argv":["vllm","serve"]}`},
	} {
		dep, err := st.CreateDeployment(&store.Deployment{Name: d.name, ModelID: model.ID, NodeID: node.ID, ServedName: d.name, Engine: d.engine, State: "active", PriorityScheduling: d.on})
		if err != nil {
			t.Fatal(err)
		}
		rig.deployments[d.name] = dep.ID
		if _, err := st.CreateInstance(&store.Instance{UUID: d.name + "-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000 + i, Ports: "[30000]", DesiredStateJSON: d.spec}); err != nil {
			t.Fatal(err)
		}
	}
	addr := strings.TrimPrefix(upstream.URL, "http://")
	rig.p = New(st, storeNetworks(st), dialTestHub{addr: addr}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(rig.p.Close)
	rig.st = st
	return rig
}

func (r *priorityRig) key(t testing.TB, binding store.APIKeyPriority) string {
	t.Helper()
	full, err := r.st.CreateAPIKey("k-"+string(binding.Policy)+binding.Class+binding.Ceiling, 0)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := r.st.ResolveAPIKey(full)
	if !ok {
		t.Fatal("key does not resolve")
	}
	keys, err := r.st.ListAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.ID == identity.KeyID {
			if err := r.st.SetAPIKeyPriority(k.Prefix, binding); err != nil {
				t.Fatal(err)
			}
		}
	}
	return full
}

// jsonEscapes turns each %u in s into a JSON unicode escape's backslash-u.
func jsonEscapes(s string) string { return strings.ReplaceAll(s, "%u", string(rune(0x5c))+"u") }

// send posts a chat to model and returns the answer and what the engine got.
func (r *priorityRig) send(t testing.TB, key, model, asked, body string) (*httptest.ResponseRecorder, map[string]any, string) {
	t.Helper()
	return r.sendTyped(t, key, model, asked, body, "")
}

func (r *priorityRig) sendTyped(t testing.TB, key, model, asked, body, contentType string) (*httptest.ResponseRecorder, map[string]any, string) {
	t.Helper()
	if body == "" {
		body = `{"model":"` + model + `","messages":[]}`
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if asked != "" {
		req.Header.Set(priorityClassHeader, asked)
	}
	rec, got, hdr, _ := r.sendRequest(t, key, req)
	return rec, got, hdr
}

// sendRequest sends req with key (and a client X-Vllm-Priority) and returns
// the answer and what the engine got: the decoded body, the header and the
// raw body.
func (r *priorityRig) sendRequest(t testing.TB, key string, req *http.Request) (*httptest.ResponseRecorder, map[string]any, string, string) {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Vllm-Priority", "-1000")
	rec := httptest.NewRecorder()
	r.p.Handler()(rec, req)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return rec, nil, "", ""
	}
	got, hdr, raw := r.seen[len(r.seen)-1], r.hdr[len(r.hdr)-1], r.raw[len(r.raw)-1]
	r.seen, r.hdr, r.raw = nil, nil, nil
	return rec, got, hdr, raw
}

func TestGatewayWritesTheEnginePriority(t *testing.T) {
	rig := newPriorityRig(t)
	high := rig.key(t, store.APIKeyPriority{Class: priority.High})
	unbound := rig.key(t, store.APIKeyPriority{})
	ceiling := rig.key(t, store.APIKeyPriority{Class: priority.Normal, Ceiling: priority.Critical})
	fixed := rig.key(t, store.APIKeyPriority{Class: priority.Low, Ceiling: priority.Critical, Policy: priority.PolicyFixed})

	for _, tc := range []struct {
		name, key, model, asked, body string
		class, contentType            string
		engine                        any // the engine's priority field; nil: absent
	}{
		{name: "vllm, key's class, client value replaced", key: high, model: "v", body: `{"model":"v","priority":99,"messages":[]}`, class: "high", engine: float64(-20)},
		{name: "sglang runs higher-first", key: high, model: "s", class: "high", engine: float64(20)},
		{name: "unbound key runs at the default", key: unbound, model: "v", class: "normal", engine: nil},
		{name: "a lower ask", key: high, model: "v", asked: "batch", class: "batch", engine: float64(40)},
		{name: "above the key's own class is lowered", key: high, model: "s", asked: "critical", class: "high", engine: float64(20)},
		{name: "up to the ceiling", key: ceiling, model: "s", asked: "critical", class: "critical", engine: float64(40)},
		{name: "fixed ignores the ask", key: fixed, model: "s", asked: "critical", class: "low", engine: float64(-20)},
		{name: "no priority scheduling: neutral, client value replaced", key: high, model: "off", body: `{"model":"off","priority":99,"messages":[]}`, class: "high", engine: float64(0)},
		{name: "no priority scheduling, no field: nothing written", key: high, model: "off", class: "high", engine: nil},
		{name: "asked for but not launched with it: neutral", key: high, model: "unlaunched", body: `{"model":"unlaunched","priority":"x","messages":[]}`, class: "high", engine: float64(0)},
		{name: "a malformed content type is still JSON", key: high, model: "off", body: `{"model":"off","priority":"crash","messages":[]}`, class: "high", engine: float64(0), contentType: "application/json; x"},
		// A key spelled with escapes is the same key to the engine's parser.
		{name: "escaped field at the default class is neutralized", key: unbound, model: "v", body: jsonEscapes(`{"model":"v","%u0070riority":-1000,"messages":[]}`), class: "normal", engine: float64(0)},
		{name: "escaped field without priority scheduling is neutralized", key: high, model: "off", body: jsonEscapes(`{"model":"off","pr%u0069ority":-1000,"messages":[]}`), class: "high", engine: float64(0)},
		{name: "escaped field replaced by the key's class", key: high, model: "v", body: jsonEscapes(`{"%u0070riority":-1000,"model":"v","messages":[]}`), class: "high", engine: float64(-20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, seen, hdr := rig.sendTyped(t, tc.key, tc.model, tc.asked, tc.body, tc.contentType)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get(priorityClassHeader); got != tc.class {
				t.Fatalf("answered class = %q, want %q", got, tc.class)
			}
			if hdr != "" {
				t.Fatalf("X-Vllm-Priority reached the engine: %q", hdr)
			}
			if got, present := seen["priority"]; (tc.engine == nil && present) || (tc.engine != nil && got != tc.engine) {
				t.Fatalf("engine priority = %v (present %v), want %v", got, present, tc.engine)
			}
			if _, leaked := seen[strings.ToLower(priorityClassHeader)]; leaked {
				t.Fatal("the class header leaked into the body")
			}
		})
	}

	// An unknown class is refused before any engine sees the request.
	rec, seen, _ := rig.send(t, high, "v", "platinum", "")
	if rec.Code != http.StatusBadRequest || seen != nil || !strings.Contains(rec.Body.String(), "priority class") {
		t.Fatalf("unknown class = %d %s seen=%v", rec.Code, rec.Body, seen)
	}
}

// A class change on this server applies to the next request, with no wait
// for the snapshot to age.
func TestGatewayPriorityFollowsClassChanges(t *testing.T) {
	rig := newPriorityRig(t)
	if err := rig.st.CreatePriorityClass(&store.PriorityClass{Name: "gold", Value: 30}); err != nil {
		t.Fatal(err)
	}
	key := rig.key(t, store.APIKeyPriority{Class: "gold"})
	if _, seen, _ := rig.send(t, key, "s", "", ""); seen["priority"] != float64(30) {
		t.Fatalf("gold = %v", seen["priority"])
	}
	v := 35
	if _, err := rig.st.UpdatePriorityClass("gold", store.PriorityClassUpdate{Value: &v}); err != nil {
		t.Fatal(err)
	}
	if _, seen, _ := rig.send(t, key, "s", "", ""); seen["priority"] != float64(35) {
		t.Fatalf("after the edit = %v", seen["priority"])
	}
	// The default moves for unbound keys too.
	unbound := rig.key(t, store.APIKeyPriority{})
	if err := rig.st.SetDefaultPriorityClass(priority.Low); err != nil {
		t.Fatal(err)
	}
	if rec, seen, _ := rig.send(t, unbound, "s", "", ""); seen["priority"] != float64(-20) || rec.Header().Get(priorityClassHeader) != "low" {
		t.Fatalf("new default = %v %q", seen["priority"], rec.Header().Get(priorityClassHeader))
	}
}

// Batch requests run at their batch's class, the batch class when it names
// none or one since deleted, whatever their key.
func TestBatchRequestsRunAtTheirBatchClass(t *testing.T) {
	rig := newPriorityRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.Critical})
	identity, _ := rig.st.ResolveAPIKey(key)
	for _, tc := range []struct {
		name, class string
		engine      float64
	}{
		{"no class: batch", "", -40},
		{"the batch's class", priority.High, 20},
		{"a class that is gone: batch", "platinum", -40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := rig.p.Do(t.Context(), uxrecord.Caller{APIKeyID: identity.KeyID, Source: uxrecord.SourceBatch}, "/v1/chat/completions", []byte(`{"model":"s","messages":[]}`), WithPriorityClass(tc.class))
			if reply.Err != nil || reply.Status != http.StatusOK {
				t.Fatalf("batch = %+v", reply)
			}
			rig.mu.Lock()
			defer rig.mu.Unlock()
			if len(rig.seen) != 1 || rig.seen[0]["priority"] != tc.engine {
				t.Fatalf("batch priority = %v", rig.seen)
			}
			rig.seen, rig.hdr, rig.raw = nil, nil, nil
		})
	}
}

// A playground request (a server-origin evaluation's too) runs at the class
// the signed-in user names, any class, and at the default without one.
func TestPlaygroundRequestsRunAtTheClassNamed(t *testing.T) {
	rig := newPriorityRig(t)
	// The playground serves only the operations a deployment declares.
	dep, err := rig.st.GetDeployment(rig.deployments["s"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.SetModelInferenceConfig(dep.ModelID, `[{"type":"openai.chat"}]`, `["text"]`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, asked, class string
		role               string
		status             int
		engine             any
	}{
		{name: "nothing named: the default", class: "normal", status: http.StatusOK, engine: nil},
		{name: "above normal", asked: "Critical", class: "critical", status: http.StatusOK, engine: float64(40)},
		{name: "below normal", asked: "batch", class: "batch", status: http.StatusOK, engine: float64(-40)},
		{name: "no such class", asked: "platinum", status: http.StatusBadRequest},
		// Only an administrator may name any class; anyone else runs at the default.
		{name: "not an administrator", asked: "critical", role: "user", class: "normal", status: http.StatusOK, engine: nil},
	} {
		if tc.role == "" {
			tc.role = "admin"
		}
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(`{"model":"s","messages":[]}`))
			req = req.WithContext(auth.WithClaims(req.Context(), auth.Claims{UserID: 1, Username: "admin", Role: tc.role}))
			req.Header.Set("Content-Type", "application/json")
			if tc.asked != "" {
				req.Header.Set(priorityClassHeader, tc.asked)
			}
			rec := httptest.NewRecorder()
			rig.p.PlaygroundHandler(nil)(rec, req)
			rig.mu.Lock()
			defer rig.mu.Unlock()
			if rec.Code != tc.status {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if tc.status != http.StatusOK {
				if len(rig.seen) != 0 {
					t.Fatal("a refused class reached the engine")
				}
				return
			}
			if got := rec.Header().Get(priorityClassHeader); got != tc.class {
				t.Fatalf("answered class = %q", got)
			}
			if len(rig.seen) != 1 || rig.seen[0]["priority"] != tc.engine {
				t.Fatalf("engine priority = %v", rig.seen)
			}
			rig.seen, rig.hdr, rig.raw = nil, nil, nil
		})
	}
	// An unknown class is refused before a deployment is chosen: a model
	// nobody serves answers the class's 400, not its own 404, so a sleeping
	// one is not woken for it either.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(`{"model":"nobody","messages":[]}`))
	req = req.WithContext(auth.WithClaims(req.Context(), auth.Claims{UserID: 1, Username: "admin", Role: "admin"}))
	req.Header.Set(priorityClassHeader, "platinum")
	rec := httptest.NewRecorder()
	rig.p.PlaygroundHandler(nil)(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown priority class") {
		t.Fatalf("unknown class to an unserved model = %d %s", rec.Code, rec.Body)
	}
}

// The snapshot of another server's changes is no older than its bound.
func TestPriorityClassSnapshotAges(t *testing.T) {
	rig := newPriorityRig(t)
	clock := time.Unix(1000, 0)
	cache := &priorityClasses{store: rig.st, now: func() time.Time { return clock }}
	if cache.get().Default.Name != priority.Normal {
		t.Fatal("first load")
	}
	// A change the generation does not see (another server's) is picked up
	// once the snapshot ages.
	if err := rig.st.SetDefaultPriorityClass(priority.High); err != nil {
		t.Fatal(err)
	}
	// Pretend the change came from another server: this process's
	// generation does not move it.
	stale := cache.snap.Load()
	cache.snap.Store(&prioritySnapshot{table: stale.table, gen: rig.st.PriorityClassGeneration(), at: stale.at})
	if cache.get().Default.Name != priority.Normal {
		t.Fatal("a fresh snapshot reloaded without cause")
	}
	clock = clock.Add(priorityClassMaxAge)
	if cache.get().Default.Name != priority.High {
		t.Fatal("an aged snapshot did not reload")
	}
}

// The class is decided once: a second pass over the same request (a replay
// after preemption) keeps it, though the header is gone by then.
func TestPriorityDecidedOnceForReplays(t *testing.T) {
	rig := newPriorityRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.High})
	identity, _ := rig.st.ResolveAPIKey(key)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"s","messages":[]}`))
	req.Header.Set(priorityClassHeader, "low")
	req = req.WithContext(withPriorityBinding(uxrecord.WithCaller(req.Context(), uxrecord.Caller{APIKeyID: identity.KeyID, Source: uxrecord.SourceAPI}), identity.Priority))
	decided, ok := rig.p.decidePriority(httptest.NewRecorder(), req)
	if !ok {
		t.Fatal("refused")
	}
	if decided.Header.Get(priorityClassHeader) != "" {
		t.Fatal("the class header was forwarded")
	}
	for range 2 {
		if got, ok := rig.p.requestPriority(decided); !ok || got.Class.Name != "low" {
			t.Fatalf("replayed decision = %+v %v", got, ok)
		}
	}
}

// An unknown class is refused before a deployment is chosen: a sleeping or
// missing deployment is neither woken nor named.
func TestUnknownClassRefusedBeforeSelection(t *testing.T) {
	rig := newPriorityRig(t)
	key := rig.key(t, store.APIKeyPriority{})
	rec, seen, _ := rig.send(t, key, "no-such-model", "platinum", "")
	if rec.Code != http.StatusBadRequest || seen != nil || !strings.Contains(rec.Body.String(), "priority class") {
		t.Fatalf("unknown class to an unknown model = %d %s", rec.Code, rec.Body)
	}
}

// Auxiliary routes (tokenize) never get a priority written; a client's is
// still neutralized.
func TestAuxiliaryRoutesGetNoPriority(t *testing.T) {
	rig := newPriorityRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.High})
	for _, body := range []string{`{"model":"v","prompt":"x"}`, `{"model":"v","prompt":"x","priority":-9}`} {
		req := httptest.NewRequest(http.MethodPost, "/v1/tokenize", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		rig.p.Handler()(rec, req)
		rig.mu.Lock()
		seen := rig.seen
		rig.seen, rig.hdr = nil, nil
		rig.mu.Unlock()
		if rec.Code != http.StatusOK || len(seen) != 1 {
			t.Fatalf("tokenize = %d %s seen %v", rec.Code, rec.Body, seen)
		}
		if v, present := seen[0]["priority"]; present && v != float64(0) {
			t.Fatalf("tokenize priority = %v", v)
		}
	}
}

// The fields only the gateway may set are owned on every way a request can
// reach an engine with a JSON body: a model named in the query or a header
// (so the body was not spooled to find it), a JSON body declared as text, and
// a body in another encoding is refused. A multipart upload and the word
// inside a message are left alone.
func TestGatewayOwnsTheBodyFieldsOnEveryPath(t *testing.T) {
	rig := newPriorityRig(t)
	high := rig.key(t, store.APIKeyPriority{Class: priority.High})
	unbound := rig.key(t, store.APIKeyPriority{})
	forged := `{"model":"v","priority":-1000,"messages":[]}`
	post := func(path, contentType, body string, header map[string]string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		return req
	}
	for _, tc := range []struct {
		name   string
		key    string
		req    *http.Request
		engine any // nil: field absent
	}{
		{"model in the query, default class", unbound, post("/v1/chat/completions?model=v", "application/json", forged, nil), float64(0)},
		{"model in the query, the key's class", high, post("/v1/chat/completions?model=v", "application/json", forged, nil), float64(-20)},
		{"model in a header", unbound, post("/v1/chat/completions", "application/json", forged, map[string]string{"X-GPUWorks-Model": "v"}), float64(0)},
		{"JSON declared as text", unbound, post("/v1/chat/completions", "text/plain", forged, nil), float64(0)},
		{"JSON declared as text, the key's class", high, post("/v1/chat/completions", "text/plain", `{"model":"v","messages":[]}`, nil), float64(-20)},
		{"the word inside a message", unbound, post("/v1/chat/completions", "application/json", `{"model":"v","messages":[{"role":"user","content":"say \"priority\": 5"}]}`, nil), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, seen, hdr, _ := rig.sendRequest(t, tc.key, tc.req)
			if rec.Code != http.StatusOK || seen == nil {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if hdr != "" {
				t.Fatalf("X-Vllm-Priority reached the engine: %q", hdr)
			}
			if got, present := seen["priority"]; (tc.engine == nil && present) || (tc.engine != nil && got != tc.engine) {
				t.Fatalf("engine priority = %v (present %v), want %v", got, present, tc.engine)
			}
		})
	}
	t.Run("the word inside a message is kept", func(t *testing.T) {
		body := `{"model":"v","messages":[{"role":"user","content":"say \"priority\": 5"}]}`
		_, _, _, raw := rig.sendRequest(t, unbound, post("/v1/chat/completions", "application/json", body, nil))
		if raw != body {
			t.Fatalf("forwarded %s, want it as sent", raw)
		}
	})
	t.Run("a body in another encoding is refused", func(t *testing.T) {
		utf16 := []byte{0xff, 0xfe}
		for _, r := range forged {
			utf16 = append(utf16, byte(r), 0)
		}
		rec, seen, _, _ := rig.sendRequest(t, unbound, post("/v1/chat/completions?model=v", "application/json", string(utf16), nil))
		if rec.Code != http.StatusBadRequest || seen != nil || !strings.Contains(rec.Body.String(), "UTF-8") {
			t.Fatalf("UTF-16 body = %d %s, reached engine %v", rec.Code, rec.Body, seen != nil)
		}
	})
	t.Run("a multipart upload is left alone", func(t *testing.T) {
		form := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nv\r\n--b\r\nContent-Disposition: form-data; name=\"priority\"\r\n\r\n5\r\n--b--\r\n"
		_, _, hdr, raw := rig.sendRequest(t, high, post("/v1/audio/transcriptions", "multipart/form-data; boundary=b", form, nil))
		if raw != form || hdr != "" {
			t.Fatalf("multipart forwarded %q (header %q), want it as sent", raw, hdr)
		}
	})
}

// A server-origin evaluation's requests come through the playground entry
// marked with their run: the record says evaluation, they run at the class
// they name as the playground's do, and the engine never sees the mark. Only
// an administrator's mark is believed.
func TestEvaluationRequestsAreRecordedAsTheirOwnSource(t *testing.T) {
	rig := newPriorityRig(t)
	dep, err := rig.st.GetDeployment(rig.deployments["s"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.SetModelInferenceConfig(dep.ModelID, `[{"type":"openai.chat"}]`, `["text"]`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sources []uxrecord.Source
	rig.p.SetRequestSink(func(r uxrecord.Request) { mu.Lock(); sources = append(sources, r.Source); mu.Unlock() })
	send := func(role, run, class string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(`{"model":"s","messages":[]}`))
		req = req.WithContext(auth.WithClaims(req.Context(), auth.Claims{UserID: 1, Username: "u", Role: role}))
		req.Header.Set("Content-Type", "application/json")
		if run != "" {
			req.Header.Set(loadgen.BenchmarkRunHeader, run)
		}
		if class != "" {
			req.Header.Set(priorityClassHeader, class)
		}
		rec := httptest.NewRecorder()
		rig.p.PlaygroundHandler(nil)(rec, req)
		return rec
	}
	if rec := send("admin", "12", "high"); rec.Code != http.StatusOK || rec.Header().Get(priorityClassHeader) != "high" {
		t.Fatalf("evaluation = %d %q", rec.Code, rec.Header().Get(priorityClassHeader))
	}
	if rec := send("user", "12", ""); rec.Code != http.StatusOK {
		t.Fatalf("user = %d", rec.Code)
	}
	if rec := send("admin", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("playground = %d", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []uxrecord.Source{uxrecord.SourceEvaluation, uxrecord.SourcePlayground, uxrecord.SourcePlayground}
	if !slices.Equal(sources, want) {
		t.Fatalf("sources = %v, want %v", sources, want)
	}
	mu.Unlock()
	// An API caller claiming a run is still an API caller, and its claim
	// goes no further either.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"s","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(loadgen.BenchmarkRunHeader, "12")
	if rec, _, _, _ := rig.sendRequest(t, rig.key(t, store.APIKeyPriority{}), req); rec.Code != http.StatusOK {
		t.Fatalf("api = %d", rec.Code)
	}
	mu.Lock()
	if sources[len(sources)-1] != uxrecord.SourceAPI {
		t.Fatalf("api source = %v", sources[len(sources)-1])
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if !slices.Equal(rig.bench, []string{"", "", "", ""}) {
		t.Fatalf("the engine got the run mark: %q", rig.bench)
	}
}
