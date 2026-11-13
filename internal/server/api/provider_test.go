package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	_ "gpuworks.ai/gpuworks/internal/server/provider/all"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

type apiProviderTestDriver struct{}

func (apiProviderTestDriver) Name() string { return "api-test-provider" }

func (apiProviderTestDriver) Capabilities() provider.Capabilities {
	return provider.Capabilities{CanListInstances: true, CanInstallAgent: true}
}

func (apiProviderTestDriver) ValidateCredentials(context.Context, provider.Credentials) error {
	return nil
}

func (apiProviderTestDriver) ListInstances(context.Context, provider.Credentials) ([]provider.Instance, error) {
	return nil, nil
}

func (apiProviderTestDriver) PowerState(context.Context, provider.Credentials, string) (provider.PowerState, error) {
	return provider.PowerUnknown, nil
}

func (apiProviderTestDriver) PowerOn(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (apiProviderTestDriver) PowerOff(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (apiProviderTestDriver) CreateInstance(context.Context, provider.Credentials, provider.CreateInstanceRequest) (provider.Instance, error) {
	return provider.Instance{}, provider.ErrUnsupported
}

func (apiProviderTestDriver) DeleteInstance(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (apiProviderTestDriver) ResizeInstance(context.Context, provider.Credentials, string, provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

func (apiProviderTestDriver) InstallAgent(context.Context, provider.Credentials, provider.InstallRequest) error {
	return nil
}

type apiInstallProviderDriver struct {
	req provider.InstallRequest
}

func (d *apiInstallProviderDriver) Name() string { return "api-install-provider" }

func (d *apiInstallProviderDriver) Capabilities() provider.Capabilities {
	return provider.Capabilities{CanListInstances: true, CanInstallAgent: true}
}

func (d *apiInstallProviderDriver) ValidateCredentials(context.Context, provider.Credentials) error {
	return nil
}

func (d *apiInstallProviderDriver) ListInstances(context.Context, provider.Credentials) ([]provider.Instance, error) {
	return []provider.Instance{{ExternalID: "inst-1", Name: "inst", PublicIP: "203.0.113.20"}}, nil
}

func (d *apiInstallProviderDriver) PowerState(context.Context, provider.Credentials, string) (provider.PowerState, error) {
	return provider.PowerUnknown, nil
}

func (d *apiInstallProviderDriver) PowerOn(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *apiInstallProviderDriver) PowerOff(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *apiInstallProviderDriver) CreateInstance(context.Context, provider.Credentials, provider.CreateInstanceRequest) (provider.Instance, error) {
	return provider.Instance{}, provider.ErrUnsupported
}

func (d *apiInstallProviderDriver) DeleteInstance(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

func (d *apiInstallProviderDriver) ResizeInstance(context.Context, provider.Credentials, string, provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

func (d *apiInstallProviderDriver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	d.req = req
	return nil
}

func (d *apiInstallProviderDriver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	d.req = req
	return provider.InstallReport{Checks: []provider.InstallCheck{{
		Name: "Agent process", Status: "warn", Detail: "unit may need daemon-reload",
	}}}, nil
}

type apiPrecheckErrorProviderDriver struct {
	apiInstallProviderDriver
}

func (d *apiPrecheckErrorProviderDriver) Name() string { return "api-precheck-error-provider" }

func (d *apiPrecheckErrorProviderDriver) ListInstances(context.Context, provider.Credentials) ([]provider.Instance, error) {
	return []provider.Instance{{
		ExternalID: "inst-1", Name: "inst", PublicIP: "127.0.0.1", SSHPort: 1, SSHUser: "root",
	}}, nil
}

func TestProviderKindsEndpointReturnsSchema(t *testing.T) {
	h := apiHandlers{d: Deps{Providers: provider.NewService(nil, nil)}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers/kinds", nil)
	rec := httptest.NewRecorder()

	h.providerKinds(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var infos []provider.KindInfo
	if err := unmarshalList(rec.Body.Bytes(), &infos); err != nil {
		t.Fatal(err)
	}
	var runpod *provider.KindInfo
	for i := range infos {
		if infos[i].Kind == "runpod" {
			runpod = &infos[i]
			break
		}
	}
	if runpod == nil {
		t.Fatalf("runpod schema missing: %+v", infos)
	}
	if runpod.Label[localization.English] == "" || runpod.Label[localization.Chinese] == "" || !runpod.Capabilities.CanListInstances {
		t.Fatalf("runpod schema incomplete: %+v", *runpod)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"runpod","label":{"en":"RunPod","zh":"RunPod"},"homepage":"https://www.runpod.io",`) {
		t.Fatalf("runpod homepage missing from wire JSON: %s", rec.Body.String())
	}
	// RunPod pods are dialed through a TCP port they expose; AutoDL Pro
	// instances open HTTP ports only, so the server cannot dial them.
	if !runpod.ServerDialSupported || runpod.DefaultServerDialListen != "" {
		t.Fatalf("runpod server_dial metadata = supported=%v listen=%q", runpod.ServerDialSupported, runpod.DefaultServerDialListen)
	}
	for _, info := range infos {
		if info.Kind == "autodl" && info.ServerDialSupported {
			t.Fatalf("autodl offers server_dial: %+v", info)
		}
	}
	if !hasCredentialField(runpod.CredentialFields, "api_key", true, true) {
		t.Fatalf("runpod api_key credential field missing: %+v", runpod.CredentialFields)
	}
	if !hasCredentialField(runpod.CredentialFields, "endpoint", false, false) {
		t.Fatalf("runpod endpoint credential field missing: %+v", runpod.CredentialFields)
	}
	var compshare *provider.KindInfo
	for i := range infos {
		if infos[i].Kind == "compshare" {
			compshare = &infos[i]
			break
		}
	}
	if compshare == nil || !compshare.ServerDialSupported || compshare.DefaultServerDialListen != ":3389" {
		t.Fatalf("compshare server_dial metadata = %+v", compshare)
	}
}

func TestAddProviderAcceptsEndpointAndExtra(t *testing.T) {
	provider.Register(apiProviderTestDriver{})
	st, err := store.Open("file:api_provider_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-test")
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{
		Store:     st,
		Providers: provider.NewService(st, box),
	}}

	body := []byte(`{
		"kind":"api-test-provider",
		"name":"api-test",
		"endpoint":"https://provider.example.test",
		"api_key":"key",
		"extra":{"ssh_password":"secret"}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.addProvider(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetProvider(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := box.Decrypt(p.CredsCipher)
	if err != nil {
		t.Fatal(err)
	}
	var creds provider.Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatal(err)
	}
	if creds.Endpoint != "https://provider.example.test" {
		t.Fatalf("Endpoint = %q", creds.Endpoint)
	}
	if creds.Extra["ssh_password"] != "secret" {
		t.Fatalf("Extra[ssh_password] = %q", creds.Extra["ssh_password"])
	}
}

func TestListProvidersIncludesCountsAndTimestamps(t *testing.T) {
	st, err := store.Open("file:api_provider_list_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProvider(&store.Provider{Name: "provider", Kind: "runpod", CredsCipher: "cipher"}); err != nil {
		t.Fatal(err)
	}
	p2, err := st.CreateProvider(&store.Provider{Name: "provider-2", Kind: "zstack", CredsCipher: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: p2.ID, ExternalID: "inst-1"}); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil)
	rec := httptest.NewRecorder()

	h.listProviders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		Name          string `json:"name"`
		InstanceCount int    `json:"instance_count"`
		CreatedAt     string `json:"created_at"`
		UpdatedAt     string `json:"updated_at"`
	}
	if err := unmarshalList(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].CreatedAt == "" || rows[0].UpdatedAt == "" {
		t.Fatalf("timestamps missing in rows = %+v", rows)
	}
	if !strings.HasSuffix(rows[0].CreatedAt, "Z") || !strings.HasSuffix(rows[0].UpdatedAt, "Z") {
		t.Fatalf("timestamps are not UTC RFC3339 values: %+v", rows[0])
	}
	if rows[0].Name != "provider-2" || rows[0].InstanceCount != 1 {
		t.Fatalf("first row = %+v", rows[0])
	}
	if rows[1].Name != "provider" || rows[1].InstanceCount != 0 {
		t.Fatalf("second row = %+v", rows[1])
	}
	// Each row names the platform its kind connects, so the console never classifies kinds itself.
	for _, want := range []string{`"kind":"zstack","category":"private_cloud",`, `"kind":"runpod","category":"public_cloud",`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("wire JSON lacks %s: %s", want, rec.Body.String())
		}
	}
}

func TestUpdateProviderAcceptsEndpointAndExtra(t *testing.T) {
	provider.Register(apiProviderTestDriver{})
	st, err := store.Open("file:api_provider_update_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-update-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-test-provider", "api-test", provider.Credentials{
		Endpoint: "https://old.example.test",
		APIKey:   "old-key",
		Extra:    map[string]string{"ssh_password": "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{
		Store:     st,
		Providers: svc,
	}}

	body := []byte(`{
		"name":"api-test-renamed",
		"api_key":"new-key"
	}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/1", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))

	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var res struct {
		ID   uint   `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.ID != p.ID || res.Name != "api-test-renamed" || res.Kind != "api-test-provider" {
		t.Fatalf("response = %+v", res)
	}
	got, err := st.GetProvider(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := box.Decrypt(got.CredsCipher)
	if err != nil {
		t.Fatal(err)
	}
	var creds provider.Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatal(err)
	}
	if creds.Endpoint != "https://old.example.test" {
		t.Fatalf("Endpoint = %q", creds.Endpoint)
	}
	if creds.APIKey != "new-key" {
		t.Fatalf("APIKey = %q", creds.APIKey)
	}
	if creds.Extra["ssh_password"] != "secret" {
		t.Fatalf("Extra[ssh_password] = %q", creds.Extra["ssh_password"])
	}
}

func TestProviderPowerUnsupportedReturns501(t *testing.T) {
	provider.Register(apiProviderTestDriver{})
	st, err := store.Open("file:api_provider_power_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-power-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-test-provider", "api-test", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/instances/ext-1/power", bytes.NewReader([]byte(`{"action":"off"}`)))
	rec := httptest.NewRecorder()

	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{
		"id":         fmt.Sprint(p.ID),
		"externalID": "ext-1",
	}))
	h.powerProviderInstance(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body=%s provider=%d", rec.Code, rec.Body.String(), p.ID)
	}
}

func TestInstallAgentOnInstanceServerDialUsesProviderToken(t *testing.T) {
	drv := &apiInstallProviderDriver{}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_install_server_dial_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-server-dial")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-install-provider", "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	var dialAddr, dialSecret string
	h := apiHandlers{d: Deps{
		Store:     st,
		Providers: svc,
		ServerDialAgent: func(ctx context.Context, addr, secret string) error {
			dialAddr = addr
			dialSecret = secret
			return nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"server_url":"https://proxy.example/control",
		"ssh_key_pem":"ssh-key",
		"connect_mode":"server_dial",
		"server_dial_listen":":19092"
	}`)))
	req.Host = "gpuworks.example"
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()

	h.installAgentOnInstance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if drv.req.ConnectMode != "server_dial" || drv.req.ServerDialListen != ":19092" {
		t.Fatalf("driver install request = %+v", drv.req)
	}
	// The server dials this agent: it is given no server address, whatever
	// the console sent.
	if drv.req.ServerURL != "" {
		t.Fatalf("server URL = %q, want none for server_dial", drv.req.ServerURL)
	}
	if dialAddr != "203.0.113.20:19092" {
		t.Fatalf("dialAddr = %q", dialAddr)
	}
	if dialSecret == "" || dialSecret == "global-token" {
		t.Fatalf("dialSecret = %q", dialSecret)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 || nodes[0].Token != dialSecret {
		t.Fatalf("server_dial secret should be the pending node token: nodes=%+v err=%v", nodes, err)
	}
	var body struct {
		ServerURL         string `json:"server_url"`
		ConnectMode       string `json:"connect_mode"`
		ServerDialAddress string `json:"server_dial_address"`
		Checks            []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ConnectMode != "server_dial" || body.ServerDialAddress != "203.0.113.20:19092" {
		t.Fatalf("response = %+v", body)
	}
	if len(body.Checks) < 3 || body.Checks[len(body.Checks)-1].Name != "server_dial" || body.Checks[len(body.Checks)-1].Status != "ok" {
		t.Fatalf("checks = %+v", body.Checks)
	}
	if body.Checks[1].Name != "install agent" || body.Checks[1].Status != "warn" {
		t.Fatalf("install check = %+v", body.Checks[1])
	}
	if body.ServerURL != "" {
		t.Fatalf("response server URL = %q, want none for server_dial", body.ServerURL)
	}
	if nodes, err := st.ListNodes(); err != nil || len(nodes) != 1 || nodes[0].ServerURL != "" {
		t.Fatalf("a server_dial node recorded a server URL: %+v, %v", nodes, err)
	}
	// Omitting server_url is an error: the server does not guess the
	// address an agent dials.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"connect_mode":"agent_dial"
	}`)))
	req2.Header.Set("X-Forwarded-Host", "proxy.example:8443")
	req2 = req2.WithContext(chiRouteContext(req2.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec2 := httptest.NewRecorder()
	h.installAgentOnInstance(rec2, req2)
	if rec2.Code != http.StatusBadRequest || !strings.Contains(rec2.Body.String(), `"code":"SERVER_URL_REQUIRED"`) {
		t.Fatalf("missing server URL status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestProviderInstancesReturnsSSHConnections(t *testing.T) {
	st, err := store.Open("file:api_provider_ssh_connections?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	p := &store.Provider{Name: "vast", Kind: "vast"}
	if err := st.DB().Create(p).Error; err != nil {
		t.Fatal(err)
	}
	connections := `[{
		"id":"direct","type":"direct","label":"直连","host":"203.0.113.8","port":50226,"user":"root","install_supported":true
	},{
		"id":"proxy","type":"proxy","label":"平台代理","host":"ssh.example","port":38408,"user":"root","install_supported":true
	}]`
	if err := st.UpsertProviderInstance(&store.ProviderInstance{ProviderID: p.ID, ExternalID: "vast-1", PublicIP: "203.0.113.8", SSHPort: 50226, SSHUser: "root", SSHConnections: connections}); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers/1/instances", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()
	h.providerInstances(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body []struct {
		PublicIP       string                   `json:"public_ip"`
		SSHConnections []provider.SSHConnection `json:"ssh_connections"`
	}
	if err := unmarshalList(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body[0].PublicIP != "203.0.113.8" || len(body[0].SSHConnections) != 2 || body[0].SSHConnections[0].ID != "direct" || body[0].SSHConnections[1].ID != "proxy" {
		t.Fatalf("instances response = %+v", body)
	}
	if body[0].SSHConnections[0].Label[localization.English] != "直连" {
		t.Fatalf("legacy single-string SSH label was not preserved as a fallback: %+v", body[0].SSHConnections[0].Label)
	}
}

func chiRouteContext(ctx context.Context, params map[string]string) context.Context {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return context.WithValue(ctx, chi.RouteCtxKey, rctx)
}

func hasCredentialField(fields []provider.CredentialField, name string, secret, required bool) bool {
	for _, f := range fields {
		if f.Name == name {
			return f.Secret == secret && f.Required == required
		}
	}
	return false
}

// apiRestartProviderDriver can open a port for a server-dialed agent only by
// restarting the instance, as RunPod does.
type apiRestartProviderDriver struct {
	apiInstallProviderDriver
	allowed []bool
}

func (d *apiRestartProviderDriver) Name() string { return "api-restart-provider" }

func (d *apiRestartProviderDriver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	d.allowed = append(d.allowed, req.AllowInstanceRestart)
	if !req.AllowInstanceRestart {
		return provider.InstallReport{}, &provider.InstanceRestartError{Reason: "the provider adds keys by restarting", Need: "ssh_key"}
	}
	return provider.InstallReport{Checks: []provider.InstallCheck{{Name: "Agent process", Status: "ok"}}}, nil
}

// Until the operator agrees to the restart the install answers 409 with a
// code the console asks about, and leaves no node behind; agreeing installs.
func TestInstallAgentOnInstanceAsksBeforeRestartingTheInstance(t *testing.T) {
	drv := &apiRestartProviderDriver{}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_install_restart_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-restart")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-restart-provider", "restart", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Sync(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc, ServerDialAgent: func(context.Context, string, string) error { return nil }}}
	install := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", strings.NewReader(body))
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
		rec := httptest.NewRecorder()
		h.installAgentOnInstance(rec, req)
		return rec
	}
	rec := install(`{"external_id":"inst-1","connect_mode":"server_dial","server_dial_address":"203.0.113.20:15756","ssh_key_pem":"k"}`)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusConflict || out["code"] != ErrorInstanceRestartRequired {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if params, _ := out["params"].(map[string]any); params["need"] != "ssh_key" {
		t.Fatalf("params = %v, want need=ssh_key", out["params"])
	}
	if nodes, err := st.ListNodes(); err != nil || len(nodes) != 0 {
		t.Fatalf("a refused install left nodes: %+v %v", nodes, err)
	}
	rec = install(`{"external_id":"inst-1","connect_mode":"server_dial","server_dial_address":"203.0.113.20:15756","ssh_key_pem":"k","allow_instance_restart":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("agreed install: status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(drv.allowed) != 2 || drv.allowed[0] || !drv.allowed[1] {
		t.Fatalf("driver saw AllowInstanceRestart %v", drv.allowed)
	}
}
