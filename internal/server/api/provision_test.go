package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type fakePrechecker struct {
	req provision.SSHPrecheckRequest
}

func (f *fakePrechecker) PrecheckSSH(ctx context.Context, req provision.SSHPrecheckRequest) (provision.SSHPrecheckReport, error) {
	f.req = req
	return provision.SSHPrecheckReport{
		Target: req.Target,
		User:   "root",
		Host:   "example.com",
		Port:   22,
		Checks: []provision.CheckResult{{ID: "os_arch", Status: "pass", Message: "Linux x86_64"}},
	}, nil
}

type fakeInstaller struct {
	req                   provision.SSHInstallRequest
	omitServerDialAddress bool
}

func enableTestCredentialEncryption(t *testing.T, st *store.Store) {
	t.Helper()
	box, err := auth.NewSecretbox("api-provision-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
}

func (f *fakeInstaller) InstallSSH(ctx context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	f.req = req
	serverDialAddress := ""
	if req.ConnectMode == "server_dial" && !f.omitServerDialAddress {
		serverDialAddress = provision.ServerDialAddress("example.com", req.ServerDialListen)
	}
	// Like the installer, the report names the directory it used.
	installDir := strings.TrimRight(strings.TrimSpace(req.InstallDir), "/")
	if installDir == "" {
		installDir = "/opt/gpuworks"
	}
	return provision.SSHInstallReport{
		Target:            req.Target,
		User:              "root",
		Host:              "example.com",
		Port:              22,
		Arch:              "amd64",
		BinaryName:        "gpuworks-agent-linux-amd64",
		InstallDir:        installDir,
		ServerDialAddress: serverDialAddress,
		Checks:            []provision.CheckResult{{ID: "start_agent", Status: "pass", Message: "started"}},
	}, nil
}

func TestPrecheckNodeEndpoint(t *testing.T) {
	fake := &fakePrechecker{}
	h := apiHandlers{d: Deps{NodePrecheck: fake}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{
		"target":"root@example.com",
		"password":"secret",
		"server_url":"https://gpuworks.example",
		"connect_mode":"server_dial"
	}`)))
	rec := httptest.NewRecorder()

	h.precheckNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.req.Target != "root@example.com" || fake.req.Password != "secret" {
		t.Fatalf("request not passed through: %+v", fake.req)
	}
	if fake.req.ConnectMode != "server_dial" {
		t.Fatalf("connect mode not passed through: %q", fake.req.ConnectMode)
	}
	var report provision.SSHPrecheckReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Host != "example.com" || len(report.Checks) != 1 || report.Checks[0].Status != "pass" {
		t.Fatalf("report = %+v", report)
	}
}

func TestInstallNodeEndpointInjectsServerURLAndToken(t *testing.T) {
	fake := &fakeInstaller{}
	st, err := store.Open("file:api_provision_token_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	enableTestCredentialEncryption(t, st)
	h := apiHandlers{d: Deps{
		NodeInstaller: fake,
		Store:         st,
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(`{
		"target":"root@example.com",
		"password":"secret",
		"server_url":"http://gpuworks.example",
		"connect_mode":"agent_dial",
		"host_address":"10.0.0.8"
	}`)))
	rec := httptest.NewRecorder()

	h.installNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.req.Target != "root@example.com" || fake.req.Password != "secret" {
		t.Fatalf("request not passed through: %+v", fake.req)
	}
	if fake.req.ServerURL != "http://gpuworks.example" {
		t.Fatalf("ServerURL = %q", fake.req.ServerURL)
	}
	if fake.req.Token == "" {
		t.Fatal("node token was not generated")
	}
	if fake.req.ConnectMode != "agent_dial" {
		t.Fatalf("ConnectMode = %q", fake.req.ConnectMode)
	}
	if fake.req.HostAddress != "10.0.0.8" {
		t.Fatalf("HostAddress = %q", fake.req.HostAddress)
	}
	if fake.req.ServerDialListen != "0.0.0.0:18800" {
		t.Fatalf("default ServerDialListen = %q", fake.req.ServerDialListen)
	}
	if fake.req.NoSystemd || fake.req.KeepSupervisor {
		t.Fatalf("an install without no_systemd must use systemd: %+v", fake.req)
	}
	var report provision.SSHInstallReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.BinaryName != "gpuworks-agent-linux-amd64" || len(report.Checks) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.ServerURL != "http://gpuworks.example" {
		t.Fatalf("server URL response = %q", report.ServerURL)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 || nodes[0].ServerDialListen != "0.0.0.0:18800" {
		t.Fatalf("persisted default server_dial listen = %+v, err=%v", nodes, err)
	}
	var identity struct {
		NodeUUID string `json:"node_uuid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	if identity.NodeUUID != nodes[0].UUID {
		t.Fatalf("install response node_uuid = %q, want %q", identity.NodeUUID, nodes[0].UUID)
	}
	if nodes[0].InstallDir != "/opt/gpuworks" {
		t.Fatalf("persisted install dir = %q, want the one the install used", nodes[0].InstallDir)
	}
}

// The wizard's "start via systemd" switch arrives as no_systemd; recovery's
// keep-the-host's-supervisor flag is server-side only.
func TestInstallNodeEndpointPassesNoSystemd(t *testing.T) {
	fake := &fakeInstaller{}
	st, err := store.Open("file:api_provision_no_systemd_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	enableTestCredentialEncryption(t, st)
	h := apiHandlers{d: Deps{NodeInstaller: fake, Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(`{
		"target":"root@example.com",
		"password":"secret",
		"server_url":"http://gpuworks.example",
		"no_systemd":true,
		"KeepSupervisor":true
	}`)))
	rec := httptest.NewRecorder()
	h.installNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !fake.req.NoSystemd || fake.req.KeepSupervisor {
		t.Fatalf("no_systemd = %v, keep = %v; want true, false", fake.req.NoSystemd, fake.req.KeepSupervisor)
	}
}

func TestServerDialInstallCheckRetrySchedule(t *testing.T) {
	for _, tc := range []struct {
		name      string
		successOn int
		wantTimes []time.Duration
		wantCode  string
	}{
		{"already listening", 1, []time.Duration{0}, messages.CheckServerDialConnected},
		{"ready on first retry", 2, []time.Duration{0, 2 * time.Second}, messages.CheckServerDialConnected},
		{"ready on second retry", 3, []time.Duration{0, 2 * time.Second, 6 * time.Second}, messages.CheckServerDialConnected},
		{"still unreachable", 0, []time.Duration{0, 2 * time.Second, 6 * time.Second}, messages.CheckServerDialNotConnected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var times []time.Duration
				result := serverDialInstallCheck(t.Context(), "agent:18800", "token", func(context.Context, string, string) error {
					times = append(times, time.Since(start))
					if len(times) == tc.successOn {
						return nil
					}
					return errors.New("agent not listening")
				})
				if !slices.Equal(times, tc.wantTimes) || result.Code != tc.wantCode {
					t.Fatalf("dial times %v, code %q; want %v, %q", times, result.Code, tc.wantTimes, tc.wantCode)
				}
			})
		})
	}
}

func TestServerDialInstallCheckCanceledDuringRetryWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(3*time.Second, cancel)
		calls := 0
		result := serverDialInstallCheck(ctx, "agent:18800", "token", func(context.Context, string, string) error {
			calls++
			return errors.New("agent not listening")
		})
		if calls != 2 || result.Code != messages.CheckServerDialNotConnected || result.Evidence != context.Canceled.Error() {
			t.Fatalf("got %d attempts and result %+v after cancellation", calls, result)
		}
	})
}

func TestInstallNodeEndpointTriggersServerDial(t *testing.T) {
	fake := &fakeInstaller{}
	st, err := store.Open("file:api_provision_serverdial_token_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	enableTestCredentialEncryption(t, st)
	var dialAddr string
	h := apiHandlers{d: Deps{
		NodeInstaller: fake,
		Store:         st,
		ServerDialAgent: func(ctx context.Context, addr, secret string) error {
			dialAddr = addr
			if secret != fake.req.Token {
				t.Fatalf("server_dial secret = %q, want generated node token", secret)
			}
			return nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(`{
		"target":"root@example.com",
		"password":"secret",
		"server_url":"http://gpuworks.example",
		"connect_mode":"server_dial",
		"server_dial_listen":":19091"
	}`)))
	rec := httptest.NewRecorder()

	h.installNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.req.ServerDialListen != ":19091" {
		t.Fatalf("ServerDialListen = %q", fake.req.ServerDialListen)
	}
	if dialAddr != "example.com:19091" {
		t.Fatalf("dialAddr = %q", dialAddr)
	}
	var report provision.SSHInstallReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.ServerDialAddress != "example.com:19091" {
		t.Fatalf("ServerDialAddress = %q", report.ServerDialAddress)
	}
	if got := report.Checks[len(report.Checks)-1]; got.ID != "server_dial" || got.Status != "pass" {
		t.Fatalf("server_dial check = %+v", got)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 || nodes[0].ServerDialAddress != "example.com:19091" {
		t.Fatalf("persisted server_dial address = %+v, err=%v", nodes, err)
	}
	redial, err := st.ServerDialNodes()
	if err != nil || len(redial) != 1 || redial[0].UUID != nodes[0].UUID {
		t.Fatalf("pending node missing from redial list = %+v, err=%v", redial, err)
	}
}

func TestInstallNodeEndpointDerivesMissingServerDialAddress(t *testing.T) {
	fake := &fakeInstaller{omitServerDialAddress: true}
	st, err := store.Open("file:api_provision_serverdial_derived_address_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	enableTestCredentialEncryption(t, st)
	var dialAddr string
	h := apiHandlers{d: Deps{
		NodeInstaller: fake,
		Store:         st,
		ServerDialAgent: func(ctx context.Context, addr, secret string) error {
			dialAddr = addr
			return nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(`{
		"target":"root@example.com",
		"password":"secret",
		"server_url":"http://gpuworks.example",
		"connect_mode":"server_dial",
		"server_dial_listen":":19091",
		"host_address":"203.0.113.8"
	}`)))
	rec := httptest.NewRecorder()
	h.installNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if dialAddr != "203.0.113.8:19091" {
		t.Fatalf("derived dial address = %q", dialAddr)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 || nodes[0].ServerDialAddress != "203.0.113.8:19091" {
		t.Fatalf("persisted derived address = %+v err=%v", nodes, err)
	}
}

func TestInstallNodeEndpointDerivesServerDialAddressFromTarget(t *testing.T) {
	fake := &fakeInstaller{omitServerDialAddress: true}
	st, err := store.Open("file:api_provision_serverdial_target_address_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	enableTestCredentialEncryption(t, st)
	var dialAddr string
	h := apiHandlers{d: Deps{
		NodeInstaller: fake,
		Store:         st,
		ServerDialAgent: func(ctx context.Context, addr, secret string) error {
			dialAddr = addr
			return nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(`{
		"target":"root@example.com",
		"password":"secret",
		"server_url":"http://gpuworks.example",
		"connect_mode":"server_dial",
		"server_dial_listen":":19091"
	}`)))
	rec := httptest.NewRecorder()
	h.installNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if dialAddr != "example.com:19091" {
		t.Fatalf("target-derived dial address = %q", dialAddr)
	}
}

func TestInstallNodeEndpointRequiresRunner(t *testing.T) {
	h := apiHandlers{d: Deps{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()

	h.installNode(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInstallNodeEndpointRejectsInvalidServerURLBeforeCreatingNode(t *testing.T) {
	fake := &fakeInstaller{}
	st, err := store.Open("file:api_provision_invalid_url_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{NodeInstaller: fake, Store: st}}
	for _, raw := range []string{"control.example", "ftp://control.example", "https://control.example?x=1"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/install", bytes.NewReader([]byte(`{"target":"root@example.com","server_url":"`+raw+`"}`)))
		rec := httptest.NewRecorder()
		h.installNode(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("server_url %q status = %d body=%s", raw, rec.Code, rec.Body.String())
		}
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("invalid server URLs left pending nodes: %+v", nodes)
	}
}

func TestPrecheckNodeEndpointRequiresRunner(t *testing.T) {
	h := apiHandlers{d: Deps{}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()

	h.precheckNode(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

type failingPrechecker struct{}

func (failingPrechecker) PrecheckSSH(context.Context, provision.SSHPrecheckRequest) (provision.SSHPrecheckReport, error) {
	return provision.SSHPrecheckReport{}, errors.New("ssh handshake: permission denied")
}

func TestPrecheckNodeEndpointReportsCallErrorAsAdvisoryCheck(t *testing.T) {
	h := apiHandlers{d: Deps{NodePrecheck: failingPrechecker{}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{"target":"root@example.com"}`)))
	rec := httptest.NewRecorder()
	h.precheckNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var report provision.SSHPrecheckReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Target != "root@example.com" || report.User != "root" || report.Host != "example.com" || report.Port != 22 || len(report.Checks) != 1 || report.Checks[0].ID != "ssh" || report.Checks[0].Status != "fail" {
		t.Fatalf("advisory precheck report = %+v", report)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{"target":"not-a-target"}`)))
	rec = httptest.NewRecorder()
	h.precheckNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unparsed target status = %d body=%s", rec.Code, rec.Body.String())
	}
	report = provision.SSHPrecheckReport{}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.User != "" || report.Host != "" || report.Port != 0 || len(report.Checks) != 1 || report.Checks[0].Status != "fail" {
		t.Fatalf("unparsed advisory report = %+v", report)
	}
}
