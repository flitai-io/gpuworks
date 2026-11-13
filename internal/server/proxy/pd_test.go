package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// portHub routes a tunnel by its destination port, so each fake instance is
// its own upstream server.
type portHub struct {
	mu     sync.Mutex
	routes map[string]string // "127.0.0.1:port" -> upstream address
}

func (h *portHub) OpenTunnel(_ context.Context, _ string, dst string) (net.Conn, error) {
	h.mu.Lock()
	addr, ok := h.routes[dst]
	h.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	return net.Dial("tcp", addr)
}
func (h *portHub) Online(string) bool { return true }

// fakePrefill answers like vLLM's NixlConnector producer (evidence
// wire/vllm-0.25.1-nixl.json): a one-token completion carrying the
// kv_transfer_params the decoder needs. mode switches in the failures.
type fakePrefill struct {
	mu       sync.Mutex
	mode     string // "", "500", "400", "no_params", "no_cache_figure"
	bodies   []map[string]any
	traces   []string // the traceparent of each call
	engineID string
	// cached is the prompt its prefix cache served; vLLM with
	// --enable-prompt-tokens-details reports it (captured on gpu3: 0 on a
	// new prompt, 880 of 888 on a repeat).
	cached int
}

func (f *fakePrefill) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.traces = append(f.traces, r.Header.Get("traceparent"))
	mode := f.mode
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch mode {
	case "500":
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"engine dead"}}`)
		return
	case "400":
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"max_tokens too large","type":"BadRequestError"}}`)
		return
	}
	answer := map[string]any{
		"id": "chatcmpl-p", "object": "chat.completion", "model": body["model"],
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "The"}, "finish_reason": "length"}},
		"usage": map[string]any{"prompt_tokens": 1036, "completion_tokens": 1, "total_tokens": 1037,
			"prompt_tokens_details": map[string]any{"cached_tokens": f.cached, "created_cache_tokens": 1036 - f.cached}},
		// vLLM with --enable-per-request-metrics answers its own account of
		// the one-token copy.
		"metrics": map[string]any{"queue_time_ms": 1.5, "time_to_first_token_ms": 640.2, "generation_time_ms": 0},
	}
	if mode == "no_cache_figure" {
		delete(answer["usage"].(map[string]any), "prompt_tokens_details")
	}
	if mode != "no_params" {
		answer["kv_transfer_params"] = map[string]any{
			"do_remote_prefill": true, "do_remote_decode": false,
			"remote_block_ids": [][]int{{1, 2, 3}}, "remote_engine_id": f.engineID,
			"remote_request_id": "chatcmpl-p-req", "remote_host": "10.0.0.1", "remote_port": 30001, "tp_size": 1, "remote_num_tokens": 1036,
		}
	} else {
		answer["kv_transfer_params"] = nil
	}
	_ = json.NewEncoder(w).Encode(answer)
}

// fakeDecode answers like a vLLM decoder and records what it was sent. Its
// usage counts the KV it pulled from a prefill instance as cached, so a
// handed-off request reports the whole prompt cached (captured on gpu3:
// 19 of 19 for a prompt never seen before); a request it computes alone
// reports its own cache, nothing here. A stream carries usage when asked
// for it, a plain reply always.
type fakeDecode struct {
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
	traces []string // the traceparent of each call
}

func (f *fakeDecode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.traces = append(f.traces, r.Header.Get("traceparent"))
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	cached := 0
	if kv, _ := body["kv_transfer_params"].(map[string]any); kv["do_remote_prefill"] == true {
		cached = 1036
	}
	usage := fmt.Sprintf(`{"prompt_tokens":1036,"total_tokens":1038,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":%d,"created_cache_tokens":0}}`, cached)
	if body["stream"] != true {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-d","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"length"}],"usage":`+usage+`}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"length\"}]}\n\n")
	if options, _ := body["stream_options"].(map[string]any); options["include_usage"] == true {
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-d","choices":[],"usage":`+usage+"}\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

type pdHarness struct {
	proxy    *Proxy
	acct     *Accountant
	st       *store.Store
	dep      *store.Deployment
	decode   *fakeDecode
	prefills []*fakePrefill
	hub      *portHub
	// key authenticates the harness's /v1 requests: the gateway requires one
	// unless an administrator opens it, and a test about routing should not
	// also be a test of the auth switch.
	key string
}

func newPDHarness(t testing.TB, prefills int, fallback string) *pdHarness {
	t.Helper()
	return newPDHarnessMode(t, "vllm-nixl", prefills, fallback)
}

func newPDHarnessMode(t testing.TB, mode string, prefills int, fallback string) *pdHarness {
	t.Helper()
	st, err := store.Open("file:proxy_pd_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/pd", Revision: "main", Name: "pd", Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-pd", store.HashToken("node"), "node-pd")
	if err != nil {
		t.Fatal(err)
	}
	fb := ""
	if fallback != "" {
		fb = fmt.Sprintf(`,"fallback":%q`, fallback)
	}
	units := make([]string, prefills)
	for i := range units {
		units[i] = `{"node_uuid":"node-pd"}`
	}
	placement := fmt.Sprintf(`{"pd":{"mode":%q%s,"roles":{"prefill":{"units":[%s]},"decode":{"units":[{"node_uuid":"node-pd"}]}}}}`, mode, fb, strings.Join(units, ","))
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "pd", Engine: "vllm", State: "active", Placement: placement})
	if err != nil {
		t.Fatal(err)
	}
	h := &pdHarness{st: st, dep: dep, decode: &fakeDecode{}, hub: &portHub{routes: map[string]string{}}}
	serve := func(handler http.Handler, port int, role, uuid string) {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		h.hub.routes[fmt.Sprintf("127.0.0.1:%d", port)] = strings.TrimPrefix(server.URL, "http://")
		expl := fmt.Sprintf(`{"pd":{"role":%q,"address":"10.0.0.9","port":%d}}`, role, port+100)
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: port, Ports: fmt.Sprintf("[%d]", port), Role: role, ScheduleExpl: expl}); err != nil {
			t.Fatal(err)
		}
	}
	serve(h.decode, 31000, "decode", "decode-0")
	for i := 0; i < prefills; i++ {
		f := &fakePrefill{engineID: fmt.Sprintf("engine-%d", i), cached: 512}
		h.prefills = append(h.prefills, f)
		serve(f, 32000+i, "prefill", fmt.Sprintf("prefill-%d", i))
	}
	key, err := st.CreateAPIKey("pd-harness", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.key = key
	h.proxy = New(st, storeNetworks(st), h.hub, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(h.proxy.Close)
	h.acct = NewAccountant(func([]tsdb.Sample) {}, nil)
	h.proxy.SetAccountant(h.acct)
	return h
}

func (h *pdHarness) do(t testing.TB, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.key)
	h.proxy.Handler()(rec, req)
	return rec
}

const pdChat = `{"model":"pd","messages":[{"role":"user","content":"tell me about Rome"}],"stream":true,"stream_options":{"include_usage":true},"max_tokens":64,"min_tokens":2}`

func TestPDTwoStageHandsPrefillParamsToDecode(t *testing.T) {
	h := newPDHarness(t, 1, "")
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if len(h.prefills[0].bodies) != 1 || len(h.decode.bodies) != 1 {
		t.Fatalf("calls: prefill %d decode %d", len(h.prefills[0].bodies), len(h.decode.bodies))
	}
	p := h.prefills[0].bodies[0]
	if p["max_tokens"] != float64(1) || p["stream"] != false || p["stream_options"] != nil || p["min_tokens"] != nil {
		t.Fatalf("prefill body = %v", p)
	}
	if kv, _ := p["kv_transfer_params"].(map[string]any); kv["do_remote_decode"] != true {
		t.Fatalf("prefill kv params = %v", p["kv_transfer_params"])
	}
	d := h.decode.bodies[0]
	kv, _ := d["kv_transfer_params"].(map[string]any)
	if kv["remote_engine_id"] != "engine-0" || kv["remote_request_id"] != "chatcmpl-p-req" || kv["do_remote_prefill"] != true {
		t.Fatalf("decode kv params = %v", d["kv_transfer_params"])
	}
	// The decoder gets the client's request otherwise unchanged.
	if d["max_tokens"] != float64(64) || d["stream"] != true || d["min_tokens"] != float64(2) {
		t.Fatalf("decode body = %v", d)
	}
	if fallbacks := h.acct.PDFallbacks(h.dep.ID); len(fallbacks) != 0 {
		t.Fatalf("fallbacks = %v", fallbacks)
	}
}

func TestPDClientKVTransferParamsNeverReachAnEngine(t *testing.T) {
	h := newPDHarness(t, 1, "")
	forged := `{"model":"pd","prompt":"x","max_tokens":4,"kv_transfer_params":{"do_remote_prefill":true,"remote_block_ids":[1]}}`
	if rec := h.do(t, "/v1/completions", forged); rec.Code != http.StatusOK {
		t.Fatalf("response = %d", rec.Code)
	}
	kv, _ := h.decode.bodies[0]["kv_transfer_params"].(map[string]any)
	if kv["remote_engine_id"] != "engine-0" || kv["remote_block_ids"] == nil {
		t.Fatalf("decode got %v", h.decode.bodies[0]["kv_transfer_params"])
	}
	if pkv, _ := h.prefills[0].bodies[0]["kv_transfer_params"].(map[string]any); len(pkv) != 1 {
		t.Fatalf("prefill got %v", h.prefills[0].bodies[0]["kv_transfer_params"])
	}
	// A path that is not two-stage goes to the decoder directly, and the
	// forged field is neutralised there too.
	if rec := h.do(t, "/v1/embeddings", `{"model":"pd","input":"x","kv_transfer_params":{"remote_host":"evil"}}`); rec.Code != http.StatusOK {
		t.Fatalf("embeddings = %d", rec.Code)
	}
	if len(h.prefills[0].bodies) != 1 {
		t.Fatal("a non two-stage path reached the prefill instance")
	}
	last := h.decode.bodies[len(h.decode.bodies)-1]
	if value, present := last["kv_transfer_params"]; !present || value != nil {
		t.Fatalf("embeddings kv params = %v (present=%v)", value, present)
	}
	if h.decode.paths[len(h.decode.paths)-1] != "/v1/embeddings" {
		t.Fatalf("paths = %v", h.decode.paths)
	}
}

func TestPDFallbackToDecode(t *testing.T) {
	for _, c := range []struct {
		name, mode, reason string
		prefills           int
	}{
		{"server error", "500", pdFallbackServerError, 1},
		{"no params", "no_params", pdFallbackNoKVParams, 1},
		{"no prefill ready", "starting", pdFallbackNoPrefill, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPDHarness(t, c.prefills, "")
			h.prefills[0].mode = c.mode
			if c.mode == "starting" {
				if err := h.st.SetInstanceState("prefill-0", proto.InstStarting, ""); err != nil {
					t.Fatal(err)
				}
				h.proxy.routes.rebuild()
			}
			rec := h.do(t, "/v1/chat/completions", pdChat)
			if rec.Code != http.StatusOK || len(h.decode.bodies) != 1 {
				t.Fatalf("response = %d, decode calls %d", rec.Code, len(h.decode.bodies))
			}
			if kv, present := h.decode.bodies[0]["kv_transfer_params"]; present && kv != nil {
				t.Fatalf("fallback request carried kv params: %v", kv)
			}
			if got := h.acct.PDFallbacks(h.dep.ID); got[c.reason] != 1 {
				t.Fatalf("fallbacks = %v, want %s", got, c.reason)
			}
		})
	}
}

func TestPDPrefillClientErrorIsReturnedAsIs(t *testing.T) {
	h := newPDHarness(t, 1, "")
	h.prefills[0].mode = "400"
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "max_tokens too large") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if len(h.decode.bodies) != 0 {
		t.Fatal("a 4xx from prefill reached the decoder")
	}
	if got := h.acct.PDFallbacks(h.dep.ID); len(got) != 0 {
		t.Fatalf("a client error counted as fallback: %v", got)
	}
}

func TestPDStrictFallbackAnswers503(t *testing.T) {
	h := newPDHarness(t, 1, "fail")
	h.prefills[0].mode = "500"
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("response = %d %v", rec.Code, rec.Header())
	}
	if len(h.decode.bodies) != 0 {
		t.Fatal("strict fallback reached the decoder")
	}
	if got := h.acct.PDFallbacks(h.dep.ID); got[pdFallbackServerError] != 1 {
		t.Fatalf("fallbacks = %v", got)
	}
}

func TestPDUnreachablePrefillTriesTheNextOne(t *testing.T) {
	h := newPDHarness(t, 2, "")
	// The first prefill instance's port stops answering.
	h.hub.mu.Lock()
	delete(h.hub.routes, "127.0.0.1:32000")
	h.hub.mu.Unlock()
	for i := 0; i < 3; i++ {
		rec := h.do(t, "/v1/chat/completions", strings.Replace(pdChat, "Rome", fmt.Sprintf("Rome %d", i), 1))
		if rec.Code != http.StatusOK {
			t.Fatalf("response = %d", rec.Code)
		}
	}
	for _, body := range h.decode.bodies {
		kv, _ := body["kv_transfer_params"].(map[string]any)
		if kv["remote_engine_id"] != "engine-1" {
			t.Fatalf("decode kv = %v", body["kv_transfer_params"])
		}
	}
	if got := h.acct.PDFallbacks(h.dep.ID); len(got) != 0 {
		t.Fatalf("fallbacks = %v", got)
	}
	// With every prefill instance gone the request still completes.
	h.hub.mu.Lock()
	delete(h.hub.routes, "127.0.0.1:32001")
	h.hub.mu.Unlock()
	if rec := h.do(t, "/v1/chat/completions", pdChat); rec.Code != http.StatusOK {
		t.Fatalf("response = %d", rec.Code)
	}
	if got := h.acct.PDFallbacks(h.dep.ID); got[pdFallbackUnreachable] != 1 {
		t.Fatalf("fallbacks = %v", got)
	}
}

func TestPDSamePrefixSticksToOnePrefill(t *testing.T) {
	h := newPDHarness(t, 2, "")
	for i := 0; i < 4; i++ {
		if rec := h.do(t, "/v1/chat/completions", pdChat); rec.Code != http.StatusOK {
			t.Fatalf("response = %d", rec.Code)
		}
	}
	if a, b := len(h.prefills[0].bodies), len(h.prefills[1].bodies); a*b != 0 || a+b != 4 {
		t.Fatalf("same prefix split across prefill instances: %d / %d", a, b)
	}
}

func TestJSONFieldRewrite(t *testing.T) {
	for _, c := range []struct {
		in, want string
		insert   bool
	}{
		{`{"model":"a","kv_transfer_params":{"x":[1,{"y":"}"}]},"n":1}`, `{"model":"a","kv_transfer_params":V,"n":1}`, false},
		{`{"kv_transfer_params": null ,"model":"a"}`, `{"kv_transfer_params": V ,"model":"a"}`, false},
		{`{"model":"a","kv_transfer_params":true}`, `{"model":"a","kv_transfer_params":V}`, false},
		{`{"meta":{"kv_transfer_params":1},"model":"a"}`, `{"kv_transfer_params":V,"meta":{"kv_transfer_params":1},"model":"a"}`, true},
		{` {"model":"a"}`, ` {"kv_transfer_params":V,"model":"a"}`, true},
		{`{}`, `{"kv_transfer_params":V}`, true},
		{`{"model":"a"}`, `{"model":"a"}`, false},
	} {
		var out bytes.Buffer
		if err := rewriteJSONField(strings.NewReader(c.in), &out, "kv_transfer_params", []byte("V"), c.insert); err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if out.String() != c.want {
			t.Fatalf("%s -> %s, want %s", c.in, out.String(), c.want)
		}
	}
	found, err := readerContains(io.MultiReader(strings.NewReader(strings.Repeat("x", 70000)+`"kv_trans`), strings.NewReader(`fer_params"`)), []byte(`"kv_transfer_params"`))
	if err != nil || !found {
		t.Fatalf("needle across a read boundary: %v %v", found, err)
	}
}

func TestPrefillBody(t *testing.T) {
	out, err := prefillBody([]byte(`{"model":"m","max_completion_tokens":50,"stream":true,"stream_options":{},"min_completion_tokens":3,"kv_transfer_params":{"evil":1}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(out, &doc)
	if doc["max_tokens"] != float64(1) || doc["max_completion_tokens"] != float64(1) || doc["stream"] != false || doc["stream_options"] != nil || doc["min_completion_tokens"] != nil {
		t.Fatalf("prefill body = %s", out)
	}
	if kv := doc["kv_transfer_params"].(map[string]any); len(kv) != 1 || kv["do_remote_decode"] != true {
		t.Fatalf("kv = %v", kv)
	}
	if _, err := prefillBody([]byte(`[1]`), ""); err == nil {
		t.Fatal("a non-object body was accepted")
	}
}

func TestPlaygroundPinsOneInstanceAndV1DoesNot(t *testing.T) {
	h := newPDHarness(t, 1, "")
	pinned := func(handler http.HandlerFunc, path, instance string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(pdChat))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(instanceHeader, instance)
		req.Header.Set("Authorization", "Bearer "+h.key)
		handler(rec, req)
		return rec
	}
	// Pinned to the prefill instance on the internal entry: it answers alone,
	// with no two-stage handoff and no decoder involved.
	rec := pinned(h.proxy.PlaygroundHandler(nil), "/api/v1/playground/v1/chat/completions", "prefill-0")
	if rec.Code != http.StatusOK || len(h.prefills[0].bodies) != 1 || len(h.decode.bodies) != 0 {
		t.Fatalf("pinned = %d, prefill %d decode %d", rec.Code, len(h.prefills[0].bodies), len(h.decode.bodies))
	}
	if kv, present := h.prefills[0].bodies[0]["kv_transfer_params"]; present && kv != nil {
		t.Fatalf("a pinned request carried kv params: %v", kv)
	}
	if rec := pinned(h.proxy.PlaygroundHandler(nil), "/api/v1/playground/v1/chat/completions", "nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown instance = %d", rec.Code)
	}
	// /v1 ignores the header and routes through the two stages.
	if rec := pinned(h.proxy.Handler(), "/v1/chat/completions", "prefill-0"); rec.Code != http.StatusOK || len(h.decode.bodies) != 1 || len(h.prefills[0].bodies) != 2 {
		t.Fatalf("/v1 with a pin header = %d, prefill %d decode %d", rec.Code, len(h.prefills[0].bodies), len(h.decode.bodies))
	}
}

func TestParseTransferCountersSumsRanks(t *testing.T) {
	text := `# TYPE vllm:nixl_bytes_transferred histogram
vllm:nixl_bytes_transferred_sum{engine="0",model_name="qwen"} 1.1927552e+08
vllm:nixl_bytes_transferred_sum{engine="1",model_name="qwen"} 100
vllm:nixl_bytes_transferred_count{engine="0",model_name="qwen"} 1.0
vllm:nixl_num_failed_transfers_total{engine="0",model_name="qwen"} 2.0
vllm:prompt_tokens_by_source_total{engine="0",model_name="qwen",source="external_kv_transfer"} 1036.0
`
	got := parseTransferCounters(strings.NewReader(text))
	if got.bytes != 119275620 || got.failed != 2 {
		t.Fatalf("counters = %+v", got)
	}
	if empty := parseTransferCounters(strings.NewReader("vllm:num_requests_running 1\n")); empty.bytes != 0 || empty.failed != 0 {
		t.Fatalf("no NIXL counters = %+v", empty)
	}
}

func TestPDScaledToNoPrefillServesFromDecodersWithoutFallback(t *testing.T) {
	h := newPDHarness(t, 0, "fail")
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusOK || len(h.decode.bodies) != 1 {
		t.Fatalf("response = %d decode calls %d", rec.Code, len(h.decode.bodies))
	}
	if got := h.acct.PDFallbacks(h.dep.ID); len(got) != 0 {
		t.Fatalf("an intended decode-only deployment counted fallbacks: %v", got)
	}
}

// orderSleep writes a sleep intent into an instance's desired state, as the
// store's sleep orders do, and rebuilds the route snapshot.
func (h *pdHarness) orderSleep(t *testing.T, uuid string, level int) {
	t.Helper()
	h.orderSleepUnseen(t, uuid, level)
	h.proxy.routes.rebuild()
}

// orderSleepUnseen writes the intent without the route snapshot seeing it,
// as between an order and the snapshot's asynchronous rebuild.
func (h *pdHarness) orderSleepUnseen(t *testing.T, uuid string, level int) {
	t.Helper()
	raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true, DesiredSleep: level})
	if err := h.st.DB().Model(&store.Instance{}).Where("uuid = ?", uuid).Update("spec", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}

// An order takes a prefill instance out of the prefill stage even from a
// route read before the order: the pick reads the chosen instance's intent.
func TestPDPrefillPickPassesOverAnInstanceOrderedToSleepSinceTheRouteWasRead(t *testing.T) {
	h := newPDHarness(t, 2, "fail")
	route, err := pdRouteFor(h.dep)
	if err != nil {
		t.Fatal(err)
	}
	route.prefill = []candidate{
		{instanceUUID: "prefill-0", nodeUUID: "node-pd", port: 32000},
		{instanceUUID: "prefill-1", nodeUUID: "node-pd", port: 32001},
	}
	h.orderSleepUnseen(t, "prefill-0", 1)
	for i := 0; i < 32; i++ {
		l := h.proxy.pickPrefill(route, "", "node-pd", nil)
		if l == nil || l.c.instanceUUID != "prefill-1" {
			t.Fatalf("pick %d = %+v, want prefill-1", i, l)
		}
		l.release(outcomeCanceled)
	}
	h.orderSleepUnseen(t, "prefill-1", 1)
	if l := h.proxy.pickPrefill(route, "", "node-pd", nil); l != nil {
		t.Fatalf("with every prefill ordered to sleep the pick = %+v", l.c)
	}
}

// A prefill instance ordered to sleep takes no prefill stage; with every
// prefill instance asleep the decoders answer alone, which the mode allows and
// which is not counted as a fallback; with the decoder asleep the model is.
func TestPDSleepingPrefillIsLikePrefillScaledDown(t *testing.T) {
	h := newPDHarness(t, 2, "fail")
	h.orderSleep(t, "prefill-0", 1)
	for i := 0; i < 3; i++ {
		if rec := h.do(t, "/v1/chat/completions", strings.Replace(pdChat, "Rome", fmt.Sprintf("Rome %d", i), 1)); rec.Code != http.StatusOK {
			t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
		}
	}
	if len(h.prefills[0].bodies) != 0 || len(h.prefills[1].bodies) != 3 {
		t.Fatalf("prefill calls with prefill-0 asleep: %d / %d", len(h.prefills[0].bodies), len(h.prefills[1].bodies))
	}
	h.orderSleep(t, "prefill-1", 1)
	if rec := h.do(t, "/v1/chat/completions", pdChat); rec.Code != http.StatusOK || len(h.decode.bodies) != 4 {
		t.Fatalf("every prefill asleep: %d %s, decode calls %d", rec.Code, rec.Body.String(), len(h.decode.bodies))
	}
	if len(h.prefills[1].bodies) != 3 {
		t.Fatalf("a sleeping prefill took a stage: %d", len(h.prefills[1].bodies))
	}
	if got := h.acct.PDFallbacks(h.dep.ID); len(got) != 0 {
		t.Fatalf("an ordered prefill sleep counted fallbacks: %v", got)
	}
	h.orderSleep(t, "decode-0", 1)
	if rec := h.do(t, "/v1/chat/completions", pdChat); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "asleep") {
		t.Fatalf("decode asleep: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPDDrainingInstancesTakeNoNewWork(t *testing.T) {
	h := newPDHarness(t, 2, "")
	// The placement shrinks to one prefill unit: the newer instance no unit
	// claims is draining and gets no new prefill, while its handoff count is
	// what the controller waits on.
	placement := `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"node-pd"}]},"decode":{"units":[{"node_uuid":"node-pd"}]}}}}`
	if err := h.st.DB().Model(&store.Deployment{}).Where("id = ?", h.dep.ID).Update("placement", placement).Error; err != nil {
		t.Fatal(err)
	}
	h.proxy.routes.rebuild()
	for i := 0; i < 4; i++ {
		if rec := h.do(t, "/v1/chat/completions", strings.Replace(pdChat, "Rome", fmt.Sprintf("Rome %d", i), 1)); rec.Code != http.StatusOK {
			t.Fatalf("response = %d", rec.Code)
		}
	}
	if len(h.prefills[0].bodies) != 4 || len(h.prefills[1].bodies) != 0 {
		t.Fatalf("prefill calls: %d / %d", len(h.prefills[0].bodies), len(h.prefills[1].bodies))
	}
	if h.proxy.InFlight("prefill-1") != 0 || h.proxy.InFlight("prefill-0") != 0 {
		t.Fatal("finished requests left work in flight")
	}
}

func TestHandoffsCountUntilFirstByte(t *testing.T) {
	var h handoffs
	done := h.begin("p")
	h.begin("p")()
	if h.count("p") != 1 {
		t.Fatalf("count = %d", h.count("p"))
	}
	done()
	done()
	if h.count("p") != 0 {
		t.Fatalf("count after done = %d", h.count("p"))
	}
}

func TestPDConcurrentWirePairsPrefillAndDecodeByRoom(t *testing.T) {
	h := newPDHarnessMode(t, "sglang-nixl", 1, "")
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	// The prefill call runs in the background; wait for it to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.prefills[0].mu.Lock()
		n := len(h.prefills[0].bodies)
		h.prefills[0].mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.prefills[0].mu.Lock()
	defer h.prefills[0].mu.Unlock()
	if len(h.prefills[0].bodies) != 1 || len(h.decode.bodies) != 1 {
		t.Fatalf("calls: prefill %d decode %d", len(h.prefills[0].bodies), len(h.decode.bodies))
	}
	p, d := h.prefills[0].bodies[0], h.decode.bodies[0]
	for _, key := range []string{"bootstrap_host", "bootstrap_port", "bootstrap_room"} {
		if p[key] == nil || fmt.Sprint(p[key]) != fmt.Sprint(d[key]) {
			t.Fatalf("%s: prefill %v decode %v", key, p[key], d[key])
		}
	}
	if d["bootstrap_host"] != "10.0.0.9" || d["bootstrap_port"] != float64(32100) {
		t.Fatalf("bootstrap endpoint = %v:%v", d["bootstrap_host"], d["bootstrap_port"])
	}
	if p["stream"] != false || d["stream"] != true || d["max_tokens"] != float64(64) {
		t.Fatalf("stream flags: prefill %v decode %v", p["stream"], d["stream"])
	}
}

func TestPDConcurrentWireRefusesWithoutPrefill(t *testing.T) {
	h := newPDHarnessMode(t, "sglang-nixl", 1, "")
	if err := h.st.SetInstanceState("prefill-0", proto.InstStarting, ""); err != nil {
		t.Fatal(err)
	}
	h.proxy.routes.rebuild()
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || len(h.decode.bodies) != 0 {
		t.Fatalf("response = %d, decode calls %d", rec.Code, len(h.decode.bodies))
	}
}

func TestPDRouteForRefusesAPlacementItCannotRoute(t *testing.T) {
	if route, err := pdRouteFor(&store.Deployment{Placement: `{"node_uuid":"a"}`}); route != nil || err != nil {
		t.Fatalf("ordinary deployment = %+v, %v", route, err)
	}
	route, err := pdRouteFor(&store.Deployment{Placement: `{"pd":{"mode":"vllm-nixl","roles":{"decode":{"units":[{"node_uuid":"b"}]}}}}`})
	if err != nil || route == nil || route.mode.Name != "vllm-nixl" {
		t.Fatalf("PD deployment = %+v, %v", route, err)
	}
	// Either would otherwise be routed as ordinary replicas, prefill
	// instances included.
	for _, raw := range []string{`{"pd":{"mode":"no-such-mode","roles":{}}}`, `{"pd":{"mode":"vllm-nixl","bogus":1}}`} {
		if route, err := pdRouteFor(&store.Deployment{Placement: raw}); err == nil {
			t.Fatalf("pdRouteFor(%s) = %+v without an error", raw, route)
		}
	}
}

// A pinned request must be refused on the same judgement the rest of the
// gateway uses. The sleep order becomes effective at once — before the engine
// reports the new level — because a request queued behind an order can hang,
// so in that window the reported level is still 0 while the gateway already
// refuses the replica. Reading the level alone forwarded the pinned request to
// an engine on its way to sleep and accounted it as awake, while the
// deployment row for the same replica read asleep.
func TestPinnedRequestRefusesAnOrderedSleepBeforeTheEngineReports(t *testing.T) {
	h := newPDHarness(t, 1, "")
	// An instance holds a sleep intent only when it carries a desired state, so
	// give the harness's ranks one, then order sleep and leave the reported
	// level at 0: exactly the drain window, the case this feature exists for.
	if err := h.st.UpdateInstanceDesiredState("prefill-0", `{"uuid":"prefill-0","desired_state":"running","sleepable":true}`); err != nil {
		t.Fatal(err)
	}
	if err := h.st.OrderDeploymentSleep(h.dep.ID, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	inst, err := h.st.InstanceByUUID("prefill-0")
	if err != nil {
		t.Fatal(err)
	}
	if inst.SleepIntent() != 1 || inst.SleepLevel != 0 {
		t.Fatalf("fixture intent=%d level=%d, want an order in flight (1, 0)", inst.SleepIntent(), inst.SleepLevel)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(pdChat))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(instanceHeader, "prefill-0")
	h.proxy.PlaygroundHandler(nil)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("pinned request under a sleep order = %d, want 503: the gateway refuses this replica",
			rec.Code)
	}
	if len(h.prefills[0].bodies) != 0 {
		t.Fatalf("forwarded %d requests to an engine on its way to sleep", len(h.prefills[0].bodies))
	}
	// The refusal must name sleep, not the generic "not awake and running":
	// the operator's next action is to wake it, which is a different fix.
	if body := rec.Body.String(); !strings.Contains(body, "sleep") {
		t.Fatalf("refusal = %q, want it to name the sleep order so the operator knows to wake it", body)
	}
	// The pinned path must account the refusal the way the load balancing path
	// accounts the identical refusal. Counting it as a plain 5xx made the
	// metrics read as a broken deployment at the same moment the deployment row
	// read asleep, and sent an operator to look for a fault that was not there.
	h.acct.mu.Lock()
	c := h.acct.counters[h.dep.ID]
	h.acct.mu.Unlock()
	if c == nil || c.classTotals[classAsleep] != 1 {
		t.Fatalf("classAsleep = %+v, want 1: a pinned refusal for sleep is the asleep_503 the load balancing path counts", c)
	}
	if c.classTotals[class5xx] != 0 {
		t.Fatalf("an asleep pinned refusal must not also count as class5xx: %+v", c.classTotals)
	}
}

// vLLM's MooncakeConnector answers the prefill copy without
// kv_transfer_params: the gateway names the prefill instance to the decoder
// itself, by the engine id and bootstrap server its launch derives from the
// same KV endpoint, under one transfer id on both sides.
func TestPDSynthesizedHandoffNamesThePrefillInstance(t *testing.T) {
	h, _ := newBootstrapHarnessMode(t, "vllm-ppu-mooncake", 1, "")
	rec := h.do(t, "/v1/chat/completions", pdChat)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if len(h.prefills[0].bodies) != 1 || len(h.decode.bodies) != 1 {
		t.Fatalf("calls: prefill %d decode %d", len(h.prefills[0].bodies), len(h.decode.bodies))
	}
	p, _ := h.prefills[0].bodies[0]["kv_transfer_params"].(map[string]any)
	d, _ := h.decode.bodies[0]["kv_transfer_params"].(map[string]any)
	id, _ := p["transfer_id"].(string)
	if p["do_remote_decode"] != true || !strings.HasPrefix(id, "gw-") {
		t.Fatalf("prefill kv params = %v", p)
	}
	// The engine id is the one the prefill's workers registered with its
	// bootstrap server, not one derived from its address.
	want := map[string]any{"do_remote_prefill": true, "remote_engine_id": "mooncake-engine-0", "remote_bootstrap_addr": "http://10.0.0.9:32100", "transfer_id": id}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("decode kv params = %v, want %v", d, want)
	}
	// Each request gets its own transfer id.
	h.do(t, "/v1/chat/completions", pdChat)
	if next, _ := h.prefills[0].bodies[1]["kv_transfer_params"].(map[string]any); next["transfer_id"] == id {
		t.Fatalf("transfer id reused: %v", next)
	}
	if fallbacks := h.acct.PDFallbacks(h.dep.ID); len(fallbacks) != 0 {
		t.Fatalf("fallbacks = %v", fallbacks)
	}
	// A prefill instance that fails still fails the request: there is no
	// decoder fallback in this mode.
	h.prefills[0].mode = "500"
	if rec := h.do(t, "/v1/chat/completions", pdChat); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed prefill = %d %s", rec.Code, rec.Body.String())
	}
}
