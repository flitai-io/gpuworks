package inferenceapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv"
)

// GET /v1/key through the real router, key authentication and route table:
// the key's answer says the deployment refuses a declared request when busy,
// and polling it, as a gateway in front of GPUWorks does, is not traffic. The
// deployment's request records hold the poller key's two chats and nothing
// for its polls.
//
//	go test ./internal/integrationtest/inferenceapi/ -run TestKeyInfoThroughTheRouterIsNotTraffic -count=1
func TestKeyInfoThroughTheRouterIsNotTraffic(t *testing.T) {
	engine := &heldEngine{entered: make(chan struct{}, 8), release: make(chan struct{})}
	engine.letAll()
	env := testenv.New(t)
	depID := deployBusyModel(t, env, engine)
	key, err := env.Store().CreateAPIKey("poller", 0)
	if err != nil {
		t.Fatal(err)
	}
	getKey := func() (features, models string) {
		req, _ := http.NewRequest(http.MethodGet, env.URL()+"/v1/key", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Object   string          `json:"object"`
			Features json.RawMessage `json:"features"`
			Models   json.RawMessage `json:"models"`
		}
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &body) != nil || body.Object != "key" {
			t.Fatalf("GET /v1/key = %d %s, want the key's answer", resp.StatusCode, data)
		}
		return string(body.Features), string(body.Models)
	}
	// The route table refreshes asynchronously.
	wantModels := `[{"id":"busy-model","priority_scheduling":"all","busy_reject":true}]`
	var models string
	if !env.EventuallyNoFail(30*time.Second, func() bool {
		_, models = getKey()
		return models == wantModels
	}) {
		t.Fatalf("GET /v1/key models = %s, want %s", models, wantModels)
	}
	if features, _ := getKey(); features != `["priority_scheduling_header","busy_reject"]` {
		t.Fatalf("features = %s", features)
	}
	// records are the key names of the deployment's request records.
	records := func() []string {
		resp, err := env.API(t).Get(fmt.Sprintf("/api/v1/monitor/deployments/%d/requests?view=history&range=1h", depID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var history struct {
			Requests []struct {
				Key string `json:"api_key_name"`
			} `json:"requests"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&history) != nil {
			t.Fatalf("request history = %d", resp.StatusCode)
		}
		names := make([]string, 0, len(history.Requests))
		for _, r := range history.Requests {
			names = append(names, r.Key)
		}
		return names
	}
	chat := func() {
		if got := chatDeclared(t, env, key, false); got.status != http.StatusOK {
			t.Fatalf("chat = %d %s", got.status, got.body)
		}
	}
	chat()
	env.Eventually(t, 30*time.Second, func() bool { return len(records()) == 1 })
	for range 5 {
		getKey()
	}
	chat()
	env.Eventually(t, 30*time.Second, func() bool { return len(records()) >= 2 })
	got := records()
	if len(got) != 2 {
		t.Fatalf("request records = %q, want the two chats only", got)
	}
	for _, name := range got {
		if name != "poller" {
			t.Fatalf("request records = %q, want the two chats only", got)
		}
	}
}
