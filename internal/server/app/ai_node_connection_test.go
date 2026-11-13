package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Exercise the same Ask HTTP route and real diagnostic route as the UI. The
// scripted OpenAI endpoint records exactly which evidence reaches the model;
// it does not stand in for the platform's node API or its access checks.
func TestAIAskConnectionEvidenceUsesRealDiagnosticHTTP(t *testing.T) {
	for _, task := range []string{"node.health", "ask"} {
		t.Run(task, func(t *testing.T) {
			testAIAskConnectionEvidence(t, task)
		})
	}
}

func testAIAskConnectionEvidence(t *testing.T, task string) {
	t.Helper()
	f := newAIFixture(t, "ai_connection_http_"+strings.ReplaceAll(task, ".", "_"))
	node, _, err := f.st.CreateNodePending("unconnected", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 23, 10, 17, 24, 0, time.UTC)
	if err := f.st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"server_url": "https://old.example/control", "recovery_phase": "probing", "recovery_attempts": 10,
		"last_probe_at": at, "last_recovery_error": "no control session --token=" + node.Token,
	}).Error; err != nil {
		t.Fatal(err)
	}
	before, err := f.st.NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	llm := &scriptedLLM{turns: []string{toolCallTurn("get_node_connection_diagnostics", `{"node_uuid":"`+node.UUID+`"}`), finalTurn}}
	f.configureLLM(t, llm)
	if _, _, _, err := f.st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("ai-connection-http-signer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := api.Router(api.Deps{Store: f.st, AuthSigner: signer, AI: f.ai, Online: func(string) bool { return false }})
	login := httptest.NewRecorder()
	h.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`)))
	var session struct{ Token string }
	if login.Code != http.StatusOK || json.Unmarshal(login.Body.Bytes(), &session) != nil || session.Token == "" {
		t.Fatalf("login status=%d", login.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", strings.NewReader(`{"task":"`+task+`","node_uuid":"`+node.UUID+`","question":"为什么一直连接中？"}`))
	req.Header.Set("Authorization", "Bearer "+session.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ask status=%d body=%s", w.Code, w.Body)
	}
	if len(llm.reqs) != 2 {
		t.Fatalf("LLM turns=%d, want diagnostic tool then answer", len(llm.reqs))
	}
	reply := toolReply(llm.reqs[1])
	for _, want := range []string{`"saved_server_url":"https://old.example/control"`, `"recovery_attempts":10`, `"recovery_phase":"probing"`, `"last_probe_at":`, "2026-10-23", `"last_seen_at":null`, `"outcome":"offline"`, `"outcome":"credentials_unavailable"`, `"outcome":"not_checked"`, "manual_address_update"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("model lacks connection evidence %q: %s", want, reply)
		}
	}
	if strings.Contains(reply, node.Token) {
		t.Fatal("node credential reached LLM")
	}
	after, err := f.st.NodeByUUID(node.UUID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("asking for connection evidence changed the node")
	}
}
