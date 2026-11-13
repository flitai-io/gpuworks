package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type connectionToolEnv struct{ reader aihelper.APIReader }

func (e connectionToolEnv) API() aihelper.APIReader { return e.reader }
func (connectionToolEnv) GPUTopology(string) (any, error) {
	return nil, errors.New("connection diagnosis must not query GPU topology")
}

func TestAIConnectionToolPreservesCallerPermissions(t *testing.T) {
	st, signer, _ := monitorTestToken(t)
	node, _, err := st.CreateNodePending("permission-check", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { probes++; return false }})
	a := &apiHandlers{mux: router.(*chi.Mux)}
	member, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "member", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, token, status string }{{"anonymous", "", "401"}, {"member", member, "403"}} {
		t.Run(tc.name, func(t *testing.T) {
			ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
			if tc.token != "" {
				ask.Header.Set("Authorization", "Bearer "+tc.token)
			}
			_, err := connectionToolResult(t, a.apiReaderFor(ask), `{"node_uuid":"`+node.UUID+`"}`)
			if err == nil || !strings.Contains(err.Error(), tc.status) {
				t.Fatalf("diagnostic did not preserve HTTP %s: %v", tc.status, err)
			}
		})
	}
	if probes != 0 {
		t.Fatal("unauthorized assistant ran a node probe")
	}
}

func TestAIConnectionToolPreservesPlatformTokenScope(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	node, _, err := rig.st.CreateNodePending("platform-scope-report", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	a := &apiHandlers{mux: rig.h.(*chi.Mux), d: Deps{Store: rig.st}}
	for _, scope := range []string{"read", "write"} {
		t.Run(scope, func(t *testing.T) {
			token := rig.mint(scope)
			path := "/api/v1/nodes/" + node.UUID + "/connection-diagnostics"
			direct := rig.do(token, http.MethodPost, path, "")
			ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
			ask.Header.Set("Authorization", "Bearer "+token)
			result, err := connectionToolResult(t, a.apiReaderFor(ask), `{"node_uuid":"`+node.UUID+`"}`)
			if scope == "read" {
				if direct.Code != http.StatusForbidden || err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "PLATFORM_TOKEN_READ_ONLY") {
					t.Fatalf("read scope changed: direct=%d tool=%v", direct.Code, err)
				}
				return
			}
			if direct.Code != http.StatusOK || err != nil || !strings.Contains(result, `"outcome":"credentials_unavailable"`) {
				t.Fatalf("write scope diverged from the existing route: direct=%d tool=%v %s", direct.Code, err, result)
			}
		})
	}
}

func TestAIConnectionToolRejectsTargetsAndExtraArguments(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	node, _, err := st.CreateNodePending("fixed-operation", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { probes++; return false }})
	a := &apiHandlers{mux: router.(*chi.Mux)}
	ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
	ask.Header.Set("Authorization", "Bearer "+token)
	reader := a.apiReaderFor(ask)
	for _, args := range []string{
		`null`, `[]`, `{}`, `{"node_uuid":""}`, `{"node_uuid":"` + node.UUID + `/../../recovery/probe"}`,
		`{"node_uuid":"` + node.UUID + `?url=https://arbitrary.example"}`,
		`{"node_uuid":"` + strings.Repeat("A", 32) + `"}`,
		`{"node_uuid":"` + node.UUID + `","url":"https://arbitrary.example"}`,
		`{"node_uuid":"` + node.UUID + `","method":"DELETE"}`,
		`{"node_uuid":"` + node.UUID + `"} {}`,
	} {
		if _, err := connectionToolResult(t, reader, args); err == nil {
			t.Fatalf("arbitrary diagnostic input accepted: %s", args)
		}
	}
	diagnostics, ok := reader.(interface {
		NodeConnectionDiagnostics(context.Context, string) (int, []byte, error)
	})
	if !ok {
		t.Fatal("the asking-user reader does not support the fixed connection probe")
	}
	for _, uuid := range []string{"", "abc", node.UUID + "/../../recovery/probe", node.UUID + "?url=x", strings.Repeat("x", 32)} {
		if _, _, err := diagnostics.NodeConnectionDiagnostics(context.Background(), uuid); err == nil {
			t.Fatalf("reader accepted arbitrary target %q", uuid)
		}
	}
	if probes != 0 {
		t.Fatal("invalid arguments ran a node probe")
	}
	if _, _, err := reader.Get(context.Background(), "/api/v1/nodes/"+node.UUID+"/connection-diagnostics", nil); err == nil {
		t.Fatal("generic GET reader opened a POST route")
	}
	if _, err := connectionToolResult(t, reader, `{"node_uuid":"`+strings.Repeat("0", 32)+`"}`); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("nonexistent node did not retain HTTP 404: %v", err)
	}
}

func TestAIConnectionToolForwardsCookieAndLogsTheFixedOperation(t *testing.T) {
	st, _, router := newSessionRouter(t)
	node, _, err := st.CreateNodePending("cookie-report", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	var logged bytes.Buffer
	a := &apiHandlers{mux: router.(*chi.Mux), d: Deps{Store: st, Log: slog.New(slog.NewJSONHandler(&logged, nil))}}
	ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
	ask.AddCookie(cookie)
	borrowed := chi.NewRouteContext()
	borrowed.RoutePath = "/api/v1/ai/ask"
	ask = ask.WithContext(context.WithValue(ask.Context(), chi.RouteCtxKey, borrowed))
	tool, ok := aihelper.LookupTool("get_node_connection_diagnostics")
	if !ok {
		t.Fatal("connection tool missing")
	}
	if _, err := tool.Call(ask.Context(), connectionToolEnv{a.apiReaderFor(ask)}, json.RawMessage(`{"node_uuid":"`+node.UUID+`"}`)); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(logged.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["user"] != "admin" || record["method"] != "POST" || record["route"] != "/api/v1/nodes/{uuid}/connection-diagnostics" || record["status"] != float64(200) {
		t.Fatalf("fixed probe audit lost caller or operation: %v", record)
	}
	if strings.Contains(logged.String(), cookie.Value) {
		t.Fatal("management session leaked to audit log")
	}
}

func TestAIConnectionToolRetainsSafeRuntimeEvidenceAndReadOnlyState(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	node, _, err := st.CreateNodePending("safe-report", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"server_url":          "https://saved.example/" + node.Token,
		"last_recovery_error": "--password=private-secret " + node.Token,
	}).Error; err != nil {
		t.Fatal(err)
	}
	before, _ := st.NodeByUUID(node.UUID)
	trigger := &recoveryTriggerStub{}
	calls := 0
	router := Router(Deps{Store: st, AuthSigner: signer, Recovery: trigger,
		Online:   func(string) bool { return true },
		NodeCaps: func(string) []string { return []string{proto.CapConnectionDiagnostics} },
		CallNode: func(_ context.Context, uuid, method string, _ any) (json.RawMessage, error) {
			calls++
			if uuid != node.UUID || method != proto.RPCNodeConnectionDiagnostics {
				t.Fatal("assistant invoked a different node operation")
			}
			return json.RawMessage(connectionRuntimeWire), nil
		},
	})
	a := &apiHandlers{mux: router.(*chi.Mux)}
	ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
	ask.Header.Set("Authorization", "Bearer "+token)
	result, err := connectionToolResult(t, a.apiReaderFor(ask), `{"node_uuid":"`+node.UUID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"outcome":"runtime_snapshot"`, "https://runtime.example/prefix", "connection.phase.dns", "not_observed"} {
		if !strings.Contains(result, want) {
			t.Fatalf("safe diagnostic evidence missing %s: %s", want, result)
		}
	}
	for _, secret := range []string{node.Token, "private-secret", "unsafe raw report", "unsafe probe", "unsafe identity", "connection.last_failure"} {
		if strings.Contains(result, secret) {
			t.Fatalf("untrusted diagnostic content reached the model: %s", secret)
		}
	}
	after, _ := st.NodeByUUID(node.UUID)
	if calls != 1 || trigger.uuid != "" || !reflect.DeepEqual(before, after) {
		t.Fatal("diagnosis changed node settings, triggered recovery or repeated the operation")
	}
}

func TestAIConnectionToolCancellationReachesTheExistingProbe(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	node, _, err := st.CreateNodePending("cancel-report", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	stopped := make(chan struct{})
	router := Router(Deps{Store: st, AuthSigner: signer,
		Online:   func(string) bool { return true },
		NodeCaps: func(string) []string { return []string{proto.CapConnectionDiagnostics} },
		CallNode: func(ctx context.Context, _, _ string, _ any) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		},
	})
	a := &apiHandlers{mux: router.(*chi.Mux)}
	ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
	ask.Header.Set("Authorization", "Bearer "+token)
	tool, ok := aihelper.LookupTool("get_node_connection_diagnostics")
	if !ok {
		t.Fatal("connection tool missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	var result string
	var callErr error
	go func() {
		result, callErr = tool.Call(ctx, connectionToolEnv{a.apiReaderFor(ask)}, json.RawMessage(`{"node_uuid":"`+node.UUID+`"}`))
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("fixed diagnostic never reached the node probe")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled assistant continued waiting")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("request cancellation did not reach the probe")
	}
	if callErr != nil && !errors.Is(callErr, context.Canceled) || callErr == nil && !strings.Contains(result, `"outcome":"cancelled"`) {
		t.Fatalf("cancelled probe was reported as healthy: %v %s", callErr, result)
	}
}
func (connectionToolEnv) CompareWithHealthyReplica(string) (any, error) {
	return nil, errors.New("connection diagnosis must not compare workloads")
}

func connectionToolResult(t *testing.T, reader aihelper.APIReader, args string) (string, error) {
	t.Helper()
	tool, ok := aihelper.LookupTool("get_node_connection_diagnostics")
	if !ok {
		t.Fatal("the assistant cannot request the existing node connection report")
	}
	return tool.Call(context.Background(), connectionToolEnv{reader}, json.RawMessage(args))
}

func TestAIConnectionToolReadsExistingPartialReportAsAdministrator(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	node, _, err := st.CreateNodePending("offline-report", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeServerURL(node.UUID, "https://saved.example"); err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { return false }})
	a := &apiHandlers{mux: router.(*chi.Mux)}
	ask := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", nil)
	ask.Header.Set("Authorization", "Bearer "+token)
	result, err := connectionToolResult(t, a.apiReaderFor(ask), `{"node_uuid":"`+node.UUID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"node_uuid":"` + node.UUID + `"`, `"saved_server_url":"https://saved.example"`, `"outcome":"offline"`, `"outcome":"credentials_unavailable"`, `"outcome":"not_checked"`} {
		if !strings.Contains(result, want) {
			t.Fatalf("partial evidence missing %s: %s", want, result)
		}
	}
	if strings.Contains(result, node.Token) {
		t.Fatal("node token reached the model")
	}
}
