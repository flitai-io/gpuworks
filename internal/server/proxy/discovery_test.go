package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPlaygroundRealtimeStripsControlPlaneOriginBeforeUpstreamHandshake(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created"}`))
	}))
	defer upstream.Close()
	st, err := store.Open(t.TempDir()+"/realtime.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/realtime", Name: "realtime", Operations: `[{"type":"openai.realtime"}]`})
	node, _ := st.RegisterNode("realtime-node", store.HashToken("node"), "realtime-node")
	dep, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "realtime", Engine: "vllm", State: "active"})
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: json.RawMessage(`{"operations":[{"type":"openai.realtime"}]}`)}})
	if _, err := st.CreateInstance(&store.Instance{UUID: "realtime", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)}); err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), dialTestHub{addr: strings.TrimPrefix(upstream.URL, "http://")}, nil, 0)
	defer p.Close()
	proxyServer := httptest.NewServer(p.PlaygroundHandler(nil))
	defer proxyServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/api/v1/playground/v1/realtime?model=realtime"
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://control-plane.example"}}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("WebSocket handshake status=%d: %v", status, err)
	}
	defer conn.CloseNow()
	_, message, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(message), "session.created") {
		t.Fatalf("message=%q err=%v", message, err)
	}
}

func TestPlaygroundDiscoveryForwardsUnmodifiedSchemaAndRejectsWrites(t *testing.T) {
	const schema = "{\n  \"openapi\": \"3.1.0\", \"paths\": {}\n}"
	var received []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(schema))
	}))
	defer upstream.Close()
	st, err := store.Open(t.TempDir()+"/discovery.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, _ := st.RegisterNode("discovery-node", store.HashToken("node"), "discovery-node")
	for _, op := range []string{"openai.audio.speech", "openai.video.generate"} {
		model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/" + op, Name: op, Operations: `[{"type":"` + op + `"}]`})
		if err != nil {
			t.Fatal(err)
		}
		dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: op, Engine: "vllm", State: "active"})
		if err != nil {
			t.Fatal(err)
		}
		desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: json.RawMessage(`{"operations":[{"type":"` + op + `"}]}`)}})
		if _, err := st.CreateInstance(&store.Instance{UUID: op, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(st, storeNetworks(st), dialTestHub{addr: strings.TrimPrefix(upstream.URL, "http://")}, nil, 0)
	defer p.Close()
	handler := p.PlaygroundHandler(nil)
	for _, tc := range []struct {
		method, path, model string
		status              int
	}{
		{"GET", "/openapi.json", "openai.audio.speech", 200},
		{"GET", "/openapi.json", "openai.video.generate", 200},
		{"GET", "/v1/audio/voices", "openai.audio.speech", 200},
		{"GET", "/v1/audio/voices", "openai.video.generate", 404},
		{"POST", "/v1/audio/voices", "openai.audio.speech", 404},
		{"DELETE", "/v1/audio/voices/x", "openai.audio.speech", 404},
		{"GET", "/openapi.json", "external:1:chat", 404},
		{"GET", "/v1/audio/voices", "external:1:chat", 404},
	} {
		before := len(received)
		r := httptest.NewRequest(tc.method, "/api/v1/playground"+tc.path+"?model="+tc.model+"&variant=preview", nil)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s model=%s status=%d body=%s", tc.method, tc.path, tc.model, w.Code, w.Body)
		}
		if tc.status == 200 {
			if w.Body.String() != schema || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("response modified: %s %s", w.Header(), w.Body)
			}
			if len(received) != before+1 || received[before] != tc.method+" "+tc.path+"?variant=preview" {
				t.Fatalf("upstream request=%v", received)
			}
		} else if len(received) != before {
			t.Fatal("unauthorized request reached upstream")
		}
	}
}
