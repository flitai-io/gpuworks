package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type offlineNodes struct{}

func (offlineNodes) CallNode(context.Context, string, string, any) (json.RawMessage, error) {
	return nil, io.EOF
}
func (offlineNodes) Online(string) bool { return false }

type testScope struct{ st *store.Store }

func (s testScope) DeploymentNodes([]uint) ([]string, error) { return nil, nil }
func (s testScope) UnknownDeployment(ids []uint) (uint, bool) {
	for _, id := range ids {
		if _, err := s.st.GetDeployment(id); err != nil {
			return id, true
		}
	}
	return 0, false
}
func (s testScope) UnknownNode(uuids []string) (string, bool) {
	for _, u := range uuids {
		if _, err := s.st.NodeByUUID(u); err != nil {
			return u, true
		}
	}
	return "", false
}

func TestDebugSessionRoutes(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, _, err := st.CreateNodePending("gpu-01")
	if err != nil {
		t.Fatal(err)
	}
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	manager := diagnostics.NewManager(t.TempDir(), debuglog.New(slog.NewTextHandler(io.Discard, nil)), offlineNodes{}, testScope{st}, slog.New(slog.DiscardHandler))
	router := Router(Deps{Store: st, AuthSigner: signer, Diagnostics: manager})
	call := func(token, method, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1/diagnostics/debug-session", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	const limits = `"limits":{"default_duration_seconds":600,"min_duration_seconds":60,"max_duration_seconds":3600,"default_max_bytes":10737418240,"max_max_bytes":53687091200,"node_max_bytes":209715200,"request_max_bytes":16777216,"keep_days":7}`

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if code, _ := call("", method, `{}`); code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d", method, code)
		}
		if code, _ := call(member, method, `{}`); code != http.StatusForbidden {
			t.Fatalf("member %s = %d", method, code)
		}
	}
	if code, body := call(admin, http.MethodGet, ""); code != http.StatusOK || strings.TrimSpace(body) != `{"session":null,`+limits+`}` {
		t.Fatalf("before any session = %d %s", code, body)
	}
	for body, want := range map[string]string{
		`nope`:                         `"code":"DEBUG_SESSION_INVALID"`,
		`{"duration_seconds":-1}`:      `"code":"DEBUG_SESSION_INVALID"`,
		`{"duration_seconds":7200}`:    `"params":{"field":"duration","max":3600}`,
		`{"max_bytes":60000000000}`:    `"params":{"field":"max_bytes","max":53687091200}`,
		`{"deployment_ids":[99]}`:      `"code":"DIAGNOSTICS_SCOPE_UNKNOWN","error":"no such deployment","params":{"id":"99"}`,
		`{"node_uuids":["not-there"]}`: `"params":{"id":"not-there"}`,
	} {
		if code, got := call(admin, http.MethodPost, body); code != http.StatusBadRequest || !strings.Contains(got, want) {
			t.Fatalf("POST %s = %d %s", body, code, got)
		}
	}

	code, body := call(admin, http.MethodPost, `{"duration_seconds":120,"max_bytes":1048576,"node_uuids":["`+node.UUID+`"]}`)
	if code != http.StatusCreated {
		t.Fatalf("start = %d %s", code, body)
	}
	var state struct {
		Session map[string]any `json:"session"`
	}
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		t.Fatal(err)
	}
	s := state.Session
	if s["open"] != true || s["started_by"] != "admin" || s["max_bytes"] != float64(1048576) || s["ended"] != nil || s["ended_at"] != nil {
		t.Fatalf("session = %v", s)
	}
	if nodes, _ := json.Marshal(s["nodes"]); string(nodes) != `[{"name":"gpu-01","reason":"offline","state":"skipped","uuid":"`+node.UUID+`"}]` {
		t.Fatalf("nodes = %s", nodes)
	}
	if deps, _ := json.Marshal(s["deployment_ids"]); string(deps) != `[]` {
		t.Fatalf("deployment_ids = %s", deps)
	}
	if code, got := call(admin, http.MethodPost, `{}`); code != http.StatusConflict || !strings.Contains(got, `"code":"DEBUG_SESSION_OPEN"`) {
		t.Fatalf("second start = %d %s", code, got)
	}
	if code, got := call(admin, http.MethodDelete, ""); code != http.StatusOK || !strings.Contains(got, `"open":false,"ended":"stopped","ended_at":"`) {
		t.Fatalf("stop = %d %s", code, got)
	}
	if code, got := call(admin, http.MethodDelete, ""); code != http.StatusOK || !strings.Contains(got, `"ended":"stopped"`) {
		t.Fatalf("second stop answers the last session = %d %s", code, got)
	}
	if code, _ := call(admin, http.MethodPut, `{}`); code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT = %d", code)
	}

	// Without diagnostics the server says so instead of pretending.
	router = Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/debug-session", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"DIAGNOSTICS_UNAVAILABLE"`) {
		t.Fatalf("unavailable = %d %s", rec.Code, rec.Body.String())
	}
}

type apiBundleSources struct{ logFile string }

func (s apiBundleSources) OpenNodeFiles(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, io.EOF
}
func (s apiBundleSources) InstanceLogs(context.Context, string, time.Time, time.Time, io.Writer, int64) (bool, error) {
	return false, nil
}
func (s apiBundleSources) InstanceNodes([]string) ([]string, error)     { return nil, nil }
func (s apiBundleSources) DeploymentInstances([]uint) ([]string, error) { return nil, nil }
func (s apiBundleSources) Snapshot(context.Context, diagnostics.SnapshotScope) (map[string]any, error) {
	return map[string]any{"settings": map[string]string{"k": "v"}}, nil
}
func (s apiBundleSources) ServerLogFiles() []string            { return []string{s.logFile} }
func (s apiBundleSources) ServerDirs() []diagnostics.ServerDir { return nil }
func (s apiBundleSources) Version() string                     { return "v-api" }

func TestDiagnosticsBundleRoutes(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	manager := diagnostics.NewManager(t.TempDir(), debuglog.New(slog.NewTextHandler(io.Discard, nil)), offlineNodes{}, testScope{st}, slog.New(slog.DiscardHandler))
	logFile := t.TempDir() + "/gpuworks-server.log"
	_ = os.WriteFile(logFile, []byte("time="+time.Now().UTC().Format(time.RFC3339Nano)+" level=INFO msg=hello\n"), 0o600)
	manager.SetSources(apiBundleSources{logFile: logFile})
	router := Router(Deps{Store: st, AuthSigner: signer, Diagnostics: manager})
	call := func(token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/diagnostics/bundles"}, {http.MethodPost, "/api/v1/diagnostics/bundles"},
		{http.MethodGet, "/api/v1/diagnostics/bundles/x"}, {http.MethodGet, "/api/v1/diagnostics/bundles/x/download"},
	} {
		if rec := call("", r.method, r.path, `{}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d", r.method, r.path, rec.Code)
		}
		if rec := call(member, r.method, r.path, `{}`); rec.Code != http.StatusForbidden {
			t.Fatalf("member %s %s = %d", r.method, r.path, rec.Code)
		}
	}
	if rec := call(admin, http.MethodGet, "/api/v1/diagnostics/bundles", ""); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"bundles":[],"limits":{"default_window_seconds":3600,"max_window_seconds":604800,"max_nodes":64,"max_instances":64,"keep_days":7}}` {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body.String())
	}
	for body, want := range map[string]string{
		`nope`:                            `"code":"DIAGNOSTICS_BUNDLE_INVALID"`,
		`{"from":"yesterday"}`:            `"params":{"field":"window"}`,
		`{"from":"2026-11-02T00:00:00Z"}`: `"error":"give both from and to, or neither"`,
		`{"from":"2026-11-02T00:00:00Z","to":"2026-11-01T00:00:00Z"}`: `"params":{"field":"window","max":604800}`,
		`{"deployment_ids":[42]}`:                                     `"code":"DIAGNOSTICS_SCOPE_UNKNOWN"`,
	} {
		if rec := call(admin, http.MethodPost, "/api/v1/diagnostics/bundles", body); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("POST %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec := call(admin, http.MethodPost, "/api/v1/diagnostics/bundles", `{"doctor":false}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Doctor bool   `json:"doctor"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if started.State != "running" || started.Doctor || started.ID == "" {
		t.Fatalf("started = %s", rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = call(admin, http.MethodGet, "/api/v1/diagnostics/bundles/"+started.ID, "")
		if strings.Contains(rec.Body.String(), `"state":"ready"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %s", rec.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(rec.Body.String(), `"requested_by":"admin"`) || !strings.Contains(rec.Body.String(), `{"id":"server_log","path":"server/gpuworks-server.log","status":"pass","bytes":`) {
		t.Fatalf("bundle = %s", rec.Body.String())
	}
	rec = call(admin, http.MethodGet, "/api/v1/diagnostics/bundles/"+started.ID+"/download", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/gzip" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="gpuworks-diagnostics-`+started.ID+`.tar.gz"` || rec.Body.Len() == 0 {
		t.Fatalf("download = %d %v", rec.Code, rec.Header())
	}
	if rec := call(admin, http.MethodGet, "/api/v1/diagnostics/bundles/nope/download", ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"DIAGNOSTICS_BUNDLE_NOT_FOUND","error":"no such diagnostics collection ready to download","params":{"bundle":"nope"}`) {
		t.Fatalf("missing download = %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(admin, http.MethodGet, "/api/v1/diagnostics/bundles", ""); !strings.Contains(rec.Body.String(), `"id":"`+started.ID+`"`) {
		t.Fatalf("list = %s", rec.Body.String())
	}
}

// The bundle API lists each item as a check: exact keys, a code and params
// for every status but pass, the failed read's text as evidence only.
func TestDiagnosticsBundleItemWire(t *testing.T) {
	var b diagnostics.Bundle
	manifest := `{"id":"b1","items":[{"path":"server/gpuworks-server.log","status":"pass","bytes":7},` +
		`{"path":"nodes/n2","status":"skip","code":"DIAGNOSTICS_NODE_OFFLINE","bytes":0},` +
		`{"path":"db","status":"fail","code":"DIAGNOSTICS_ITEM_READ_FAILED","evidence":"database is locked","bytes":0},` +
		// An item from a manifest written before items were checks.
		`{"name":"nodes/n1/agent.log","state":"collected","bytes":9,"truncated":true}]}`
	if err := json.Unmarshal([]byte(manifest), &b); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(bundleDTO(b).Items)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"id":"server_log","path":"server/gpuworks-server.log","status":"pass","bytes":7},` +
		`{"id":"node","path":"nodes/n2","status":"skip","code":"DIAGNOSTICS_NODE_OFFLINE","params":{"path":"nodes/n2","node":"n2"},` +
		`"message":"the node is offline; nodes/n2 was not collected","bytes":0},` +
		`{"id":"records","path":"db","status":"fail","code":"DIAGNOSTICS_ITEM_READ_FAILED","params":{"path":"db"},` +
		`"message":"db could not be read","evidence":"database is locked","bytes":0},` +
		`{"id":"node_agent_log","path":"nodes/n1/agent.log","status":"warn","code":"DIAGNOSTICS_ITEM_TRUNCATED",` +
		`"params":{"path":"nodes/n1/agent.log","node":"n1"},"message":"nodes/n1/agent.log reached its size limit; only part was kept","bytes":9}]`
	if string(raw) != want {
		t.Fatalf("items =\n%s\nwant\n%s", raw, want)
	}
	var back []dto.DiagnosticsBundleItem
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if again, _ := json.Marshal(back); string(again) != want {
		t.Fatalf("round trip = %s", again)
	}
}
