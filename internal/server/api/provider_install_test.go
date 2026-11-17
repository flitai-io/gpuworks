package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInstallAgentOnInstanceContinuesAfterPrecheckError(t *testing.T) {
	drv := &apiPrecheckErrorProviderDriver{}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_install_precheck_error_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-precheck-error")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-precheck-error-provider", "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"server_url":"https://proxy.example/control",
		"password":"pw"
	}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()

	h.installAgentOnInstance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if drv.req.ServerURL != "https://proxy.example/control" {
		t.Fatalf("install should proceed after precheck error, request = %+v", drv.req)
	}
	var body struct {
		Checks          []provision.CheckResult `json:"checks"`
		InstallDetails  []provision.CheckResult `json:"install_details"`
		PrecheckDetails []provision.CheckResult `json:"precheck_details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Checks) < 3 || body.Checks[0].ID != "precheck" || body.Checks[0].Status != "fail" || body.Checks[0].Code != messages.CheckPrecheckAttention {
		t.Fatalf("precheck error should be reported without blocking install: %+v", body.Checks)
	}
	// The failed item is behind the summary row too, with its reason as
	// evidence. Preparing the platform key came first, then the SSH dial
	// that failed.
	if n := len(body.PrecheckDetails); n < 2 || body.PrecheckDetails[0].ID != "platform_key_provider" ||
		body.PrecheckDetails[n-1].ID != "ssh" || body.PrecheckDetails[n-1].Status != "fail" || body.PrecheckDetails[n-1].Code != messages.CheckSSHConnectFailed || body.PrecheckDetails[n-1].Evidence == "" {
		t.Fatalf("precheck_details = %+v", body.PrecheckDetails)
	}
	if body.Checks[0].Params["checks"] != "platform_key_provider, ssh" || !strings.Contains(body.Checks[0].Evidence, "ssh: ssh dial") {
		t.Fatalf("precheck summary should name the items that need attention: %+v", body.Checks[0])
	}
	if body.Checks[1].ID != "install_agent" || body.Checks[1].Status != "warn" || body.Checks[1].Code != messages.CheckAgentNotObserved {
		t.Fatalf("install result missing after precheck error: %+v", body.Checks)
	}
	// The steps behind the summary are returned too: here the server has no
	// platform key, which the operator must be able to see.
	if len(body.InstallDetails) == 0 || body.InstallDetails[0].ID != "platform_key_provider" || body.InstallDetails[0].Status != "skip" || body.InstallDetails[0].Code != messages.CheckPlatformKeyUnavailable {
		t.Fatalf("install_details = %+v", body.InstallDetails)
	}
	// The old keys of a check are gone from the wire.
	var raw struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, check := range raw.Checks {
		if _, ok := check["name"]; ok {
			t.Fatalf("check carries the old name key: %v", check)
		}
		if _, ok := check["detail"]; ok {
			t.Fatalf("check carries the old detail key: %v", check)
		}
	}
}

func TestInstallAgentOnInstanceRejectsInvalidServerURLBeforeCreatingNode(t *testing.T) {
	drv := &apiInstallProviderDriver{}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_install_invalid_url_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-invalid-url")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-install-provider", "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"server_url":"https://control.example?unsafe=1"
	}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()
	h.installAgentOnInstance(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("invalid server URL left pending nodes: %+v", nodes)
	}
}

// The one precheck row names only what needs attention; the rest would bury
// the warning among a dozen passing items (and, with the install directory's
// mount options, run to thousands of characters). Its status is the worst of
// the items, and a skipped item is never summarized as passed.
func TestSummarizePrecheckNamesOnlyTheItemsThatNeedAttention(t *testing.T) {
	ok := func(id string) provision.CheckResult {
		return provision.PassCheck(id, "", id+" is fine, details we do not want in the summary", nil)
	}
	for _, tc := range []struct {
		name    string
		checks  []provision.CheckResult
		wire    string
		message string
	}{
		{"no checks", nil,
			`{"id":"precheck","status":"skip","code":"CHECK_PRECHECK_EMPTY","message":"the provider precheck reported no items"}`, ""},
		{"all pass", []provision.CheckResult{ok("a"), ok("b"), provision.NACheck("c", messages.CheckNotNeededOnMac, "n/a", nil)},
			`{"id":"precheck","status":"pass","code":"CHECK_PRECHECK_PASSED","params":{"total":"3"},"message":"all 3 checks passed"}`, ""},
		{"one warning among passing items",
			[]provision.CheckResult{ok("a"), provision.WarnCheck("install_dir_space", messages.CheckInstallDirSpaceLow, "20.0 GiB free of 80.0 GiB", nil), ok("c")},
			`{"id":"precheck","status":"warn","code":"CHECK_PRECHECK_ATTENTION","params":{"checks":"install_dir_space","count":"1","total":"3"},"message":"install_dir_space: 20.0 GiB free of 80.0 GiB"}`, ""},
		{"a skipped item is not a pass",
			[]provision.CheckResult{ok("a"), provision.SkipCheck("install_dir", messages.CheckInstallDirProbeFailed, "could not inspect /opt/gpuworks", nil)},
			"", "skip"},
		{"warning and failure keep their order, evidence follows",
			[]provision.CheckResult{{ID: "fuse", Status: "warn", Code: messages.CheckCommandFailed}, ok("b"), provision.FailCheck("ssh", messages.CheckSSHConnectFailed, "no session", nil).WithEvidence("connection refused")},
			`{"id":"precheck","status":"fail","code":"CHECK_PRECHECK_ATTENTION","params":{"checks":"fuse, ssh","count":"2","total":"3"},"message":"fuse; ssh: no session","evidence":"ssh: connection refused"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizePrecheck(tc.checks)
			if tc.message != "" {
				if got.Status != tc.message {
					t.Fatalf("status = %q, want %q (%+v)", got.Status, tc.message, got)
				}
				return
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.wire {
				t.Fatalf("summary = %s\nwant      %s", raw, tc.wire)
			}
		})
	}
}

func TestAgentDialInstallCheckReportsSessionState(t *testing.T) {
	ctx := context.Background()
	if got := agentDialInstallCheck(ctx, "node-1", func(uuid string) bool { return uuid == "node-1" }); got.Status != "pass" || got.Code != messages.CheckAgentSessionEstablished {
		t.Fatalf("online check = %+v", got)
	}
	if got := agentDialInstallCheck(ctx, "node-1", nil); got.Status != "skip" || got.Code != messages.CheckAgentSessionUnobserved {
		t.Fatalf("missing hub check = %+v", got)
	}
	if got := agentDialInstallCheck(ctx, "", func(string) bool { return true }); got.Status != "skip" || got.Code != messages.CheckAgentSessionUnobserved {
		t.Fatalf("missing node UUID check = %+v", got)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if got := agentDialInstallCheck(waitCtx, "node-3", func(string) bool { return false }); got.Status != "warn" || got.Code != messages.CheckAgentSessionWaiting || got.Evidence == "" {
		t.Fatalf("cancelled wait = %+v", got)
	}
	online := make(chan struct{})
	go func() {
		time.Sleep(120 * time.Millisecond)
		close(online)
	}()
	if got := agentDialInstallCheck(ctx, "node-2", func(string) bool {
		select {
		case <-online:
			return true
		default:
			return false
		}
	}); got.Status != "pass" {
		t.Fatalf("delayed session check = %+v", got)
	}
}

type apiInstallFailingProviderDriver struct {
	apiInstallProviderDriver
}

func (d *apiInstallFailingProviderDriver) Name() string { return "api-install-failing-provider" }

func (d *apiInstallFailingProviderDriver) InstallAgentWithReport(context.Context, provider.Credentials, provider.InstallRequest) (provider.InstallReport, error) {
	return provider.InstallReport{}, errors.New("ssh: unable to authenticate; the target has not authorized the platform key")
}

// A failed install answers 424 with the reason in the body, never 502: CDNs
// and tunnels (Cloudflare) replace 502 bodies with their own error page.
func TestInstallAgentOnInstanceFailureKeepsReasonOutOfGatewayStatus(t *testing.T) {
	provider.Register(&apiInstallFailingProviderDriver{})
	st, err := store.Open("file:api_provider_install_failure_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-failure")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-install-failing-provider", "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"server_url":"https://control.example",
		"connect_mode":"agent_dial",
		"ssh_connection_id":"auto"
	}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()

	h.installAgentOnInstance(rec, req)
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"`) || !strings.Contains(rec.Body.String(), "has not authorized the platform key") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func providerInstallHandlers(t *testing.T, drv provider.Driver, dbName, kind string) (*apiHandlers, *store.Store, uint) {
	t.Helper()
	provider.Register(drv)
	st, err := store.Open("file:"+dbName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox(dbName)
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), kind, "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st, Providers: svc}}, st, p.ID
}

func postProviderInstall(h *apiHandlers, providerID uint, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", strings.NewReader(body))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(providerID)}))
	rec := httptest.NewRecorder()
	h.installAgentOnInstance(rec, req)
	return rec
}

// install_dir and no_systemd on the wire reach the driver's install request.
func TestInstallAgentOnInstanceCarriesTheDirectoryAndSupervisor(t *testing.T) {
	drv := &apiPrecheckErrorProviderDriver{}
	h, _, providerID := providerInstallHandlers(t, drv, "api_provider_install_dir_test", "api-precheck-error-provider")
	rec := postProviderInstall(h, providerID, `{"external_id":"inst-1","server_url":"https://control.example","password":"pw","install_dir":"/data/gpuworks/","no_systemd":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if drv.req.InstallDir != "/data/gpuworks" || !drv.req.NoSystemd {
		t.Fatalf("driver request dir=%q no_systemd=%v", drv.req.InstallDir, drv.req.NoSystemd)
	}

	drv.req = provider.InstallRequest{}
	rec = postProviderInstall(h, providerID, `{"external_id":"inst-1","server_url":"https://control.example","password":"pw"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if drv.req.InstallDir != "" || drv.req.NoSystemd {
		t.Fatalf("defaults: driver request dir=%q no_systemd=%v", drv.req.InstallDir, drv.req.NoSystemd)
	}
}

// A cloud install directory is an absolute path other than /: refused before
// a pending node exists.
func TestInstallAgentOnInstanceRejectsAnInvalidInstallDir(t *testing.T) {
	drv := &apiInstallProviderDriver{}
	h, st, providerID := providerInstallHandlers(t, drv, "api_provider_install_bad_dir_test", "api-install-provider")
	for _, dir := range []string{"gpuworks", "~/gpuworks", "/", "//", `/data/a\nb`} {
		body := `{"external_id":"inst-1","server_url":"https://control.example","install_dir":"` + dir + `"}`
		rec := postProviderInstall(h, providerID, body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["code"] != ErrorInstallDirInvalid {
			t.Fatalf("%q: status = %d body=%s", dir, rec.Code, rec.Body.String())
		}
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("invalid install_dir left pending nodes: %+v", nodes)
	}
}
