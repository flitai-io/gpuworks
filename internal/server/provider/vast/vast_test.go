package vast

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// Sample v1 instance page: one running instance with ssh access, one stopped.
const instancesBody = `{"success":true,"instances_found":2,"total_instances":2,"next_token":null,"instances":[
  {
    "id": 12345,
    "machine_id": 678,
    "label": "trainer-a",
    "gpu_name": "RTX 4090",
    "num_gpus": 2,
    "dph_total": 0.55,
    "actual_status": "running",
    "public_ipaddr": "203.0.113.10",
    "ssh_host": "ssh5.vast.ai",
    "ssh_port": 40022,
    "ports": {"22/tcp":[
      {"HostIp":"0.0.0.0","HostPort":"50226"},
      {"HostIp":"::","HostPort":"50226"}
    ]},
    "geolocation": "US, California"
  },
  {
    "id": 67890,
    "gpu_name": "A100 SXM4",
    "num_gpus": 1,
    "dph_total": 1.20,
    "cur_state": "stopped",
    "public_ipaddr": "198.51.100.7",
    "geolocation": "DE, Bavaria"
  }
]}`

func newTestDriver(t *testing.T, h http.HandlerFunc) *Driver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := New()
	d.baseURL = srv.URL + "/api/v0" // preserve the real default API prefix
	return d
}

func TestListInstances(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instances/" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key123" {
			t.Errorf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(instancesBody))
	})

	insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "key123"})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("want 2 instances, got %d", len(insts))
	}

	a := insts[0]
	if a.ExternalID != "12345" || a.Name != "trainer-a" {
		t.Errorf("inst a id/name = %q/%q", a.ExternalID, a.Name)
	}
	if a.Power != provider.PowerRunning {
		t.Errorf("inst a power = %q", a.Power)
	}
	if a.PricePerHr != 0.55 {
		t.Errorf("inst a price = %v", a.PricePerHr)
	}
	if a.GPUCount != 2 || a.GPUModel != "RTX 4090" {
		t.Errorf("inst a gpu = %d %q", a.GPUCount, a.GPUModel)
	}
	if a.Region != "US, California" {
		t.Errorf("inst a region = %q", a.Region)
	}
	if a.Kind != "container" {
		t.Errorf("inst a kind = %q", a.Kind)
	}
	if a.PublicIP != "203.0.113.10" || a.SSHPort != 50226 || a.SSHUser != "root" {
		t.Errorf("inst a ssh = %q:%d user=%q", a.PublicIP, a.SSHPort, a.SSHUser)
	}
	if len(a.SSHConnections) != 2 {
		t.Fatalf("inst a SSH connections = %+v", a.SSHConnections)
	}
	if direct, proxy := a.SSHConnections[0], a.SSHConnections[1]; direct.ID != "direct" || direct.Host != "203.0.113.10" || direct.Port != 50226 || !direct.InstallSupported || proxy.ID != "proxy" || proxy.Host != "ssh5.vast.ai" || proxy.Port != 40022 || !proxy.InstallSupported {
		t.Fatalf("inst a SSH connections = %+v", a.SSHConnections)
	}

	b := insts[1]
	if b.ExternalID != "67890" {
		t.Errorf("inst b id = %q", b.ExternalID)
	}
	if b.Power != provider.PowerStopped {
		t.Errorf("inst b power = %q", b.Power)
	}
	// No label → Name falls back to gpu_name.
	if b.Name != "A100 SXM4" {
		t.Errorf("inst b name (fallback gpu_name) = %q", b.Name)
	}
	// PublicIP is the instance address even when no SSH mapping is available.
	if b.PublicIP != "198.51.100.7" {
		t.Errorf("inst b public ip (fallback) = %q", b.PublicIP)
	}
	if b.Region != "DE, Bavaria" {
		t.Errorf("inst b region = %q", b.Region)
	}
}

func TestValidateCredentials(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"ok", http.StatusOK, false},
		{"unauthorized", http.StatusUnauthorized, true},
		{"forbidden", http.StatusForbidden, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				writeInstancePage(w, 1, 0, 0, nil)
			})
			err := d.ValidateCredentials(context.Background(), provider.Credentials{APIKey: "k"})
			if tc.wantErr && err == nil {
				t.Errorf("want error for status %d, got nil", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want no error for status %d, got %v", tc.status, err)
			}
		})
	}
}

func TestPowerState(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		writeFilteredInstanceFixture(t, w, r, instancesBody)
	})
	cases := []struct {
		id   string
		want provider.PowerState
	}{
		{"12345", provider.PowerRunning},
		{"67890", provider.PowerStopped},
	}
	for _, tc := range cases {
		got, err := d.PowerState(context.Background(), provider.Credentials{APIKey: "k"}, tc.id)
		if err != nil {
			t.Fatalf("PowerState(%s): %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("PowerState(%s) = %q, want %q", tc.id, got, tc.want)
		}
	}
	if _, err := d.PowerState(context.Background(), provider.Credentials{APIKey: "k"}, "nope"); err == nil {
		t.Errorf("PowerState(nope): want error for missing instance, got nil")
	}
}

func TestSearchOffers(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v0/bundles/" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"verified", "external", "rentable", "rented", "num_gpus", "gpu_ram", "reliability2", "inet_down"} {
			if _, ok := body[key]; !ok {
				t.Fatalf("search body missing %q: %#v", key, body)
			}
		}
		if body["allocated_storage"] != float64(30) || body["limit"] != float64(10) || body["type"] != "on-demand" {
			t.Fatalf("search body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"offers":[{"id":7654,"gpu_name":"RTX 5070 Ti","num_gpus":1,"gpu_ram":16376,"dph_total":0.12,"geolocation":"US","reliability2":0.99}]}`))
	})
	offers, err := d.SearchOffers(t.Context(), provider.Credentials{APIKey: "key"}, SearchOffersRequest{GPUCount: 1, MinGPURAMMB: 12000, MinReliability: 0.97, MinInetDown: 200, DiskGB: 30, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 1 || offers[0].ID.String() != "7654" || offers[0].GPUName != "RTX 5070 Ti" || offers[0].GPURAMMB != 16376 || offers[0].DPHTotal != 0.12 {
		t.Fatalf("offers = %+v", offers)
	}
}

func TestSearchOffersSurfacesAPIError(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"msg":"inventory unavailable"}`))
	})
	if _, err := d.SearchOffers(t.Context(), provider.Credentials{APIKey: "key"}, SearchOffersRequest{}); err == nil || !strings.Contains(err.Error(), "inventory unavailable") {
		t.Fatalf("SearchOffers error = %v", err)
	}
}

func TestCreateInstanceRequiresExplicitOffer(t *testing.T) {
	d := New()
	if _, err := d.CreateInstance(t.Context(), provider.Credentials{APIKey: "key"}, provider.CreateInstanceRequest{}); err == nil || !strings.Contains(err.Error(), "offer_id is required") {
		t.Fatalf("CreateInstance error = %v", err)
	}
}

func TestAttachSSHKey(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v0/instances/9988/ssh/" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["ssh_key"] != "ssh-rsa AAAA test" {
			t.Fatalf("body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"success":false,"msg":"SSH key already associated with instance."}`))
	})
	if err := d.AttachSSHKey(t.Context(), provider.Credentials{APIKey: "key"}, "9988", "ssh-rsa AAAA test"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateInstanceFromOffer(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v0/asks/7654/" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if body["client_id"] != "me" || body["image"] != "nvidia/cuda:test" || body["disk"] != float64(35) || body["label"] != "gpuworks-test-run" || body["runtype"] != "ssh" || body["cancel_unavail"] != true {
			t.Fatalf("create body = %#v", body)
		}
		// The agent's default port is mapped at creation, the only time Vast
		// maps ports, so the server can dial an agent installed later.
		if env, _ := body["env"].(map[string]any); len(env) != 1 || env["-p 18800:18800"] != "1" {
			t.Fatalf("create env = %#v", body["env"])
		}
		_, _ = w.Write([]byte(`{"success":true,"new_contract":9988}`))
	})
	inst, err := d.CreateInstance(t.Context(), provider.Credentials{APIKey: "key"}, provider.CreateInstanceRequest{
		Name: "gpuworks-test-run", GPUModel: "RTX 5070 Ti", GPUCount: 1, Image: "nvidia/cuda:test",
		Extra: map[string]string{"offer_id": "7654", "disk_gb": "35"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inst.ExternalID != "9988" || inst.Name != "gpuworks-test-run" || inst.GPUModel != "RTX 5070 Ti" || inst.Kind != "container" {
		t.Fatalf("created instance = %+v", inst)
	}
}

func TestCreateInstanceSurfacesRejectedRequest(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"msg":"offer no longer available"}`))
	})
	_, err := d.CreateInstance(t.Context(), provider.Credentials{APIKey: "key"}, provider.CreateInstanceRequest{Extra: map[string]string{"offer_id": "7654"}})
	if err == nil || !strings.Contains(err.Error(), "offer no longer available") {
		t.Fatalf("CreateInstance error = %v", err)
	}
}

func TestDeleteInstanceIsIdempotent(t *testing.T) {
	var calls int
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v0/instances/9988/" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if calls == 1 {
			data, _ := io.ReadAll(r.Body)
			if strings.TrimSpace(string(data)) != "{}" {
				t.Fatalf("delete body = %q", data)
			}
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	for range 2 {
		if err := d.DeleteInstance(t.Context(), provider.Credentials{APIKey: "key"}, "9988"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeleteInstanceSurfacesProviderFailure(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"msg":"contract locked"}`))
	})
	if err := d.DeleteInstance(t.Context(), provider.Credentials{APIKey: "key"}, "9988"); err == nil || !strings.Contains(err.Error(), "contract locked") {
		t.Fatalf("DeleteInstance error = %v", err)
	}
}

func TestInstallAgentNoSSH(t *testing.T) {
	d := New()
	err := d.InstallAgent(context.Background(), provider.Credentials{APIKey: "k"}, provider.InstallRequest{
		Instance: provider.Instance{ExternalID: "12345"},
	})
	if err == nil {
		t.Errorf("InstallAgent without ssh key/host: want error, got nil")
	}
}

// The provider runs the shared script install (provision.ScriptInstallSteps,
// whose download failure handling is tested there) behind what its images
// need, then the shared check that the agent runs.
func TestInstallScriptCommandRunsTheSharedScriptInstall(t *testing.T) {
	for _, opts := range []provision.ScriptOptions{{}, {InstallDir: "/data/gpuworks", NoSystemd: true}} {
		cmd := installScriptCommand("http://127.0.0.1:8800/", "tok'quoted", "server_dial", ":3389", opts)
		for _, want := range []string{"NO_PROXY", provision.ScriptInstallSteps("http://127.0.0.1:8800/", "tok'quoted", "server_dial", ":3389", opts), provision.AgentRunningCheckCommand(opts.InstallDir)} {
			if !strings.Contains(cmd, want) {
				t.Fatalf("install command missing %q: %s", want, cmd)
			}
		}
		if opts.InstallDir != "" && !strings.Contains(cmd, "--install-dir '/data/gpuworks' --no-systemd") {
			t.Fatalf("install command ignores the operator's choices: %s", cmd)
		}
	}
	if got := agentProcessCheck("__GPUWORKS_AGENT_PROCESS_OK__", "/data/gpuworks"); got.Status != "pass" || !strings.Contains(checkText(got), "/data/gpuworks") {
		t.Fatalf("agentProcessCheck success = %+v", got)
	}
	if got := agentProcessCheck("__GPUWORKS_AGENT_PROCESS_WARN__", ""); got.Status != "warn" {
		t.Fatalf("agentProcessCheck warning = %+v", got)
	}
}

func TestLoopbackInstallBypassesProxyOnlyForLoopback(t *testing.T) {
	if got := loopbackNoProxyPrefix("http://127.0.0.1:1234"); !strings.Contains(got, "NO_PROXY") {
		t.Fatalf("loopback prefix = %q", got)
	}
	if got := loopbackNoProxyPrefix("https://control.example"); got != "" {
		t.Fatalf("public URL proxy prefix = %q", got)
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := provider.Get("vast"); !ok {
		t.Errorf("vast driver not registered")
	}
	c := New().Capabilities()
	if !c.CanListInstances || c.CanPowerControl || !c.CanInstallAgent || !c.CanCreate || !c.CanDelete || !c.HasPricing {
		t.Errorf("unexpected caps: %+v", c)
	}
}

func TestAuthorizePublicKeyAttachesToTheInstance(t *testing.T) {
	calls := 0
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v0/instances/9988/ssh/" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	// Before the instance exists there is nothing to attach to.
	if check, err := d.AuthorizePublicKey(t.Context(), provider.Credentials{APIKey: "key"}, "", "ssh-ed25519 AAAA gpuworks@s"); err != nil || check.ID != "" || calls != 0 {
		t.Fatalf("no instance: check=%+v err=%v calls=%d", check, err, calls)
	}
	check, err := d.AuthorizePublicKey(t.Context(), provider.Credentials{APIKey: "key"}, "9988", "ssh-ed25519 AAAA gpuworks@s")
	if err != nil || check.Status != "pass" || check.Code != "CHECK_PLATFORM_KEY_INSTANCE_ATTACHED" || check.Message != "attached to the Vast instance" || calls != 1 {
		t.Fatalf("check=%+v err=%v calls=%d", check, err, calls)
	}
}
