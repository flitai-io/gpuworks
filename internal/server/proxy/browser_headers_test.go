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
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployed engine sees a request the platform forwards as a server-side
// client: through the public /v1 gateway and through the console playground
// alike, the browser's page origin, page URL and console session cookie do
// not reach it, while the request itself (body, content type, other headers)
// arrives unchanged. Origin-aware engines (Strata) refused the playground's
// plain POST because it carried the console origin.
func TestEngineRequestsCarryNoBrowserContext(t *testing.T) {
	type seen struct {
		header http.Header
		body   string
	}
	var mu sync.Mutex
	var got []seen
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{header: r.Header.Clone(), body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	upstreamServer := httptest.NewServer(upstream)
	defer upstreamServer.Close()
	st, err := store.Open(t.TempDir()+"/browser-headers.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/omni", Name: "omni", Operations: `[{"type":"openai.chat"}]`})
	node, _ := st.RegisterNode("browser-headers-node", store.HashToken("node"), "browser-headers-node")
	dep, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "omni", Engine: "callable", State: "active"})
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: json.RawMessage(`{"operations":[{"type":"openai.chat"}]}`)}})
	if _, err := st.CreateInstance(&store.Instance{UUID: "omni-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, Ports: "[30000]", DesiredStateJSON: string(desired)}); err != nil {
		t.Fatal(err)
	}
	apiKey, err := st.CreateAPIKey("browser-headers", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), dialTestHub{addr: strings.TrimPrefix(upstreamServer.URL, "http://")}, nil, 0)
	defer p.Close()

	const body = `{"model":"omni","messages":[{"role":"user","content":"hi"}]}`
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
		auth    string
	}{
		{name: "public gateway", handler: p.Handler(), path: "/v1/chat/completions", auth: "Bearer " + apiKey},
		{name: "console playground", handler: p.PlaygroundHandler(nil), path: "/api/v1/playground/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			got = nil
			mu.Unlock()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "https://console.example:8443")
			req.Header.Set("Referer", "https://console.example:8443/playground/chat")
			req.Header.Set("Cookie", "gpuworks_session=must-not-leak")
			req.Header.Set("X-Client-Trace", "kept")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
				t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(got))
			}
			h := got[0].header
			for _, name := range []string{"Origin", "Referer", "Cookie", "Authorization"} {
				if v := h.Get(name); v != "" {
					t.Errorf("upstream received %s=%q", name, v)
				}
			}
			if h.Get("Content-Type") != "application/json" || h.Get("X-Client-Trace") != "kept" {
				t.Errorf("request headers were not forwarded: content-type=%q x-client-trace=%q", h.Get("Content-Type"), h.Get("X-Client-Trace"))
			}
			if !strings.Contains(got[0].body, `"content":"hi"`) {
				t.Errorf("upstream body = %s", got[0].body)
			}
		})
	}
}
