package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

type seenRequest struct {
	method, requestURI, body string
}

// tokenizeUpstream answers like vLLM: tokenize and detokenize at the root,
// nothing under /v1 but chat.
func tokenizeUpstream(mu *sync.Mutex, seen *[]seenRequest) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, seenRequest{r.Method, r.RequestURI, string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tokenize":
			_, _ = io.WriteString(w, `{"count":2,"max_model_len":40960,"tokens":[9707,1879],"token_strs":null}`)
		case "/detokenize":
			_, _ = io.WriteString(w, `{"prompt":"Hello world"}`)
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"1"}}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"Not Found"}`)
		}
	})
}

// servedUnder makes the instance serve under an engine name other than the
// public one, so the gateway rewrites the request's model.
func servedUnder(t *testing.T, p *Proxy, engineName string) {
	t.Helper()
	desired, err := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{
		Entrypoint: []string{"vllm", "serve", engineName}, ModelMount: engineName,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.UpdateInstanceDesiredState("omni-instance", string(desired)); err != nil {
		t.Fatal(err)
	}
	p.routes.rebuild()
}

func postGateway(p *Proxy, apiKey, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	return rec
}

// assertUnaccounted checks that nothing the gateway keeps about traffic moved.
func assertUnaccounted(t *testing.T, p *Proxy, acct *Accountant, depID uint, records []uxrecord.Request) {
	t.Helper()
	if _, ok := acct.LastRequestAt(depID); ok {
		t.Error("tokenize set the deployment's last request time")
	}
	if _, samples := acct.snapshotAndClearDirty(); len(samples) != 0 {
		t.Errorf("tokenize was accounted: %+v", samples)
	}
	acct.mu.Lock()
	counters := len(acct.counters)
	acct.mu.Unlock()
	if counters != 0 {
		t.Errorf("tokenize made %d counter sets", counters)
	}
	if len(records) != 0 {
		t.Errorf("tokenize left request records: %+v", records)
	}
	if live := p.LiveRequests(depID, 10); len(live.Requests) != 0 {
		t.Errorf("tokenize is still in flight: %+v", live)
	}
	if n := p.balancer.inflight("omni-instance"); n != 0 {
		t.Errorf("the replica's lease was not given back: inflight %d", n)
	}
}

// /v1/tokenize and /v1/detokenize reach the engine's root paths with the
// engine's own model name, and come back as the engine answered, but are not
// traffic. A chat request through the same gateway is.
func TestTokenizeIsForwardedToTheEngineRootUnaccounted(t *testing.T) {
	var mu sync.Mutex
	var seen []seenRequest
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, tokenizeUpstream(&mu, &seen))
	defer closeUpstream()
	var records []uxrecord.Request
	p.SetRequestSink(func(r uxrecord.Request) { records = append(records, r) })
	servedUnder(t, p, "/runtime/model")

	rec := postGateway(p, apiKey, http.MethodPost, "/v1/tokenize", `{"model":"omni","prompt":"Hello world"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"count":2,"max_model_len":40960,"tokens":[9707,1879],"token_strs":null}` {
		t.Fatalf("tokenize = %d %s", rec.Code, rec.Body)
	}
	rec = postGateway(p, apiKey, http.MethodPost, "/v1/detokenize", `{"model":"omni","tokens":[9707,1879]}`)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"prompt":"Hello world"}` {
		t.Fatalf("detokenize = %d %s", rec.Code, rec.Body)
	}
	want := []seenRequest{
		{http.MethodPost, "/tokenize", `{"model":"/runtime/model","prompt":"Hello world"}`},
		{http.MethodPost, "/detokenize", `{"model":"/runtime/model","tokens":[9707,1879]}`},
	}
	if len(seen) != len(want) {
		t.Fatalf("the engine saw %+v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("request %d = %+v, want %+v", i, seen[i], want[i])
		}
	}
	assertUnaccounted(t, p, acct, depID, records)

	rec = postGateway(p, apiKey, http.MethodPost, "/v1/chat/completions", `{"model":"omni","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body)
	}
	if _, ok := acct.LastRequestAt(depID); !ok {
		t.Error("a chat request did not set the last request time")
	}
	if len(records) != 1 {
		t.Errorf("records = %d after a chat request", len(records))
	}
}

// A deployment with tokenize turned off answers 404 from the gateway: the
// engine is never asked and the replica picked for the request is given
// back. The switch is read from the route snapshot, not the store.
func TestTokenizeTurnedOffIsNotForwarded(t *testing.T) {
	var mu sync.Mutex
	var seen []seenRequest
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, tokenizeUpstream(&mu, &seen))
	defer closeUpstream()
	var records []uxrecord.Request
	p.SetRequestSink(func(r uxrecord.Request) { records = append(records, r) })
	if err := p.store.SetDeploymentTokenize(depID, false); err != nil {
		t.Fatal(err)
	}
	p.routes.rebuild()

	for _, path := range []string{"/v1/tokenize", "/v1/detokenize"} {
		rec := postGateway(p, apiKey, http.MethodPost, path, `{"model":"omni","prompt":"x","tokens":[1]}`)
		want := `{"error":{"message":"deployment omni does not expose POST ` + path + `: tokenize is turned off for it","type":"not_found_error","code":"TOKENIZE_DISABLED","param":null,"params":{"method":"POST","model":"omni","path":"` + path + `"}}}` + "\n"
		if rec.Code != http.StatusNotFound || rec.Body.String() != want {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("the engine was asked: %+v", seen)
	}
	assertUnaccounted(t, p, acct, depID, records)

	// The store alone changing, without the change event, does not reach a
	// request: the snapshot answers. Its rebuild does.
	if err := p.store.DB().Exec("UPDATE deployments SET tokenize_disabled = ? WHERE id = ?", false, depID).Error; err != nil {
		t.Fatal(err)
	}
	if rec := postGateway(p, apiKey, http.MethodPost, "/v1/tokenize", `{"model":"omni","prompt":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("before the rebuild = %d %s", rec.Code, rec.Body)
	}
	p.routes.rebuild()
	if rec := postGateway(p, apiKey, http.MethodPost, "/v1/tokenize", `{"model":"omni","prompt":"x"}`); rec.Code != http.StatusOK || len(seen) != 1 {
		t.Fatalf("after the rebuild = %d %s seen=%+v", rec.Code, rec.Body, seen)
	}
}

// Only POST is taken; a request the gateway cannot route is refused the way
// any /v1 request is, and none of it is accounted.
func TestTokenizeRefusals(t *testing.T) {
	var mu sync.Mutex
	var seen []seenRequest
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, tokenizeUpstream(&mu, &seen))
	defer closeUpstream()
	var records []uxrecord.Request
	p.SetRequestSink(func(r uxrecord.Request) { records = append(records, r) })
	p.routes.rebuild()

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := postGateway(p, apiKey, method, "/v1/tokenize", `{"model":"omni","prompt":"x"}`)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost ||
			rec.Body.String() != `{"error":{"message":"/v1/tokenize only accepts POST","type":"invalid_request_error","code":"METHOD_NOT_ALLOWED","param":null,"params":{"method":"POST","path":"/v1/tokenize"}}}`+"\n" {
			t.Fatalf("%s = %d allow=%q %s", method, rec.Code, rec.Header().Get("Allow"), rec.Body)
		}
	}
	if rec := postGateway(p, "", http.MethodPost, "/v1/tokenize", `{"model":"omni","prompt":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a key = %d", rec.Code)
	}
	if rec := postGateway(p, apiKey, http.MethodPost, "/v1/tokenize", `{"prompt":"x"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "must specify a model") {
		t.Fatalf("without a model = %d %s", rec.Code, rec.Body)
	}
	if rec := postGateway(p, apiKey, http.MethodPost, "/v1/tokenize", `{"model":"nope","prompt":"x"}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no deployment serves model nope") {
		t.Fatalf("unknown model = %d %s", rec.Code, rec.Body)
	}
	// Asleep: the same 503 a chat request gets, and nothing is sent.
	if err := p.store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "omni-instance", State: proto.InstRunning, SleepLevel: 1, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
	p.routes.rebuild()
	if rec := postGateway(p, apiKey, http.MethodPost, "/v1/detokenize", `{"model":"omni","tokens":[1]}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "asleep") {
		t.Fatalf("asleep = %d %s", rec.Code, rec.Body)
	}
	if len(seen) != 0 {
		t.Fatalf("the engine was asked: %+v", seen)
	}
	assertUnaccounted(t, p, acct, depID, records)
}
