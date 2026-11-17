package runpod

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// sample /pods body: one RUNNING pod with an ssh mapping, one EXITED pod.
const podsBody = `[
  {
    "id": "pod-run-1",
    "name": "trainer-a",
    "desiredStatus": "RUNNING",
    "costPerHr": 1.29,
    "gpuCount": 2,
    "gpuTypeId": "NVIDIA A100 80GB",
    "publicIp": "203.0.113.10",
    "machineId": "m-1",
    "portMappings": [
      {"privatePort": 22, "publicPort": 40022, "ip": "203.0.113.10", "type": "tcp"},
      {"privatePort": 8888, "publicPort": 41888, "type": "tcp"}
    ]
  },
  {
    "id": "pod-exit-2",
    "name": "trainer-b",
    "desiredStatus": "EXITED",
    "costPerHr": 0.44,
    "gpuCount": 1,
    "machine": {"gpuTypeId": "NVIDIA RTX 4090"}
  }
]`

func newTestDriver(t *testing.T, h http.HandlerFunc) *Driver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := New()
	d.baseURL = srv.URL // override base URL to hit the mock
	return d
}

func TestListInstances(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pods" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key123" {
			t.Errorf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(podsBody))
	})

	insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "key123"})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("want 2 instances, got %d", len(insts))
	}

	a := insts[0]
	if a.ExternalID != "pod-run-1" || a.Name != "trainer-a" {
		t.Errorf("pod a id/name = %q/%q", a.ExternalID, a.Name)
	}
	if a.Power != provider.PowerRunning {
		t.Errorf("pod a power = %q", a.Power)
	}
	if a.PricePerHr != 1.29 {
		t.Errorf("pod a price = %v", a.PricePerHr)
	}
	if a.GPUCount != 2 || a.GPUModel != "NVIDIA A100 80GB" {
		t.Errorf("pod a gpu = %d %q", a.GPUCount, a.GPUModel)
	}
	if a.Kind != "container" {
		t.Errorf("pod a kind = %q", a.Kind)
	}
	if a.PublicIP != "203.0.113.10" || a.SSHPort != 40022 || a.SSHUser != "root" {
		t.Errorf("pod a ssh = %q:%d user=%q", a.PublicIP, a.SSHPort, a.SSHUser)
	}

	b := insts[1]
	if b.Power != provider.PowerStopped {
		t.Errorf("pod b power = %q", b.Power)
	}
	if b.GPUModel != "NVIDIA RTX 4090" {
		t.Errorf("pod b gpu model (from machine) = %q", b.GPUModel)
	}
	if b.SSHPort != 0 || b.PublicIP != "" {
		t.Errorf("pod b unexpectedly has ssh %q:%d", b.PublicIP, b.SSHPort)
	}
}

func TestListInstancesMapForm(t *testing.T) {
	body := `[{"id":"p1","name":"n","desiredStatus":"RUNNING","publicIp":"198.51.100.7","portMappings":{"22":50022,"8080":50080}}]`
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if insts[0].SSHPort != 50022 || insts[0].PublicIP != "198.51.100.7" {
		t.Errorf("map-form ssh = %q:%d", insts[0].PublicIP, insts[0].SSHPort)
	}
}

func TestGetInstanceUsesDetailEndpointAndStringPort(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pods/pod-detail" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("{\"id\":\"pod-detail\",\"status\":\"RUNNING\",\"publicIp\":\"198.51.100.21\",\"runtime\":{\"ports\":[{\"private\":\"22\",\"public\":\"5022\",\"ip\":\"198.51.100.21\",\"type\":\"tcp\"}]}}"))
	})
	inst, err := d.GetInstance(context.Background(), provider.Credentials{APIKey: "k"}, "pod-detail")
	if err != nil {
		t.Fatal(err)
	}
	if inst.ExternalID != "pod-detail" || inst.PublicIP != "198.51.100.21" || inst.SSHPort != 5022 || inst.SSHUser != "root" {
		t.Fatalf("instance = %+v", inst)
	}
}

func TestListInstancesV2PrefersExposedTCPSSH(t *testing.T) {
	body := `{"pods":[{
		"id":"p53dghe0c7r6d0","name":"quintessential_gold_guanaco","status":"RUNNING",
		"gpu":{"count":1,"id":"NVIDIA RTX PRO 4000 Blackwell"},
		"cost":0.57,"dataCenterId":"EU-RO-1",
		"ssh":{
			"direct":{"host":"213.173.104.15","port":23935,"username":"root"},
			"proxy":{"host":"ssh.runpod.io","port":22,"username":"p53dghe0c7r6d0-6441232e"}
		},
		"runtime":{"ports":[{"private":22,"public":23935,"type":"tcp","ip":"213.173.104.15"}]}
	}]}`
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pods" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Write([]byte(body))
	})
	insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 {
		t.Fatalf("instances = %d", len(insts))
	}
	inst := insts[0]
	if inst.GPUModel != "NVIDIA RTX PRO 4000 Blackwell" || inst.GPUCount != 1 {
		t.Fatalf("gpu = %q x%d", inst.GPUModel, inst.GPUCount)
	}
	if inst.PublicIP != "213.173.104.15" || inst.SSHPort != 23935 || inst.SSHUser != "root" {
		t.Fatalf("ssh = %s@%s:%d", inst.SSHUser, inst.PublicIP, inst.SSHPort)
	}
	if len(inst.SSHConnections) != 2 || inst.SSHConnections[0].ID != "direct" || !inst.SSHConnections[0].InstallSupported || inst.SSHConnections[1].ID != "proxy" || inst.SSHConnections[1].InstallSupported {
		t.Fatalf("SSH connections = %+v", inst.SSHConnections)
	}
	if inst.Region != "EU-RO-1" || inst.PricePerHr != 0.57 || inst.Power != provider.PowerRunning {
		t.Fatalf("metadata = region=%q price=%v power=%q", inst.Region, inst.PricePerHr, inst.Power)
	}
}

func TestV2SSHFallsBackToRuntimeAndRejectsTerminalOnlyProxy(t *testing.T) {
	var runtimePod pod
	if err := json.Unmarshal([]byte(`{
		"id":"runtime","runtime":{"ports":[{"private":22,"public":23935,"type":"tcp","ip":"213.173.104.15"}]},
		"ssh":{"proxy":{"host":"ssh.runpod.io","port":22,"username":"runtime-proxy"}}
	}`), &runtimePod); err != nil {
		t.Fatal(err)
	}
	inst := runtimePod.toInstance()
	if inst.PublicIP != "213.173.104.15" || inst.SSHPort != 23935 || inst.SSHUser != "root" {
		t.Fatalf("runtime ssh = %+v", inst)
	}

	var proxyPod pod
	if err := json.Unmarshal([]byte(`{
		"id":"proxy","ssh":{"proxy":{"host":"ssh.runpod.io","port":22,"username":"proxy-user"}}
	}`), &proxyPod); err != nil {
		t.Fatal(err)
	}
	inst = proxyPod.toInstance()
	if inst.PublicIP != "" || inst.SSHPort != 0 || inst.SSHUser != "" {
		t.Fatalf("terminal-only proxy was exposed as programmable ssh: %+v", inst)
	}
	if len(inst.SSHConnections) != 1 || inst.SSHConnections[0].InstallSupported {
		t.Fatalf("terminal-only proxy metadata = %+v", inst.SSHConnections)
	}
}

func TestListInstancesToleratesNestedRunPodGPUShapes(t *testing.T) {
	body := `[
		{"id":"nested","name":"voice-worker","desiredStatus":"RUNNING","gpuCount":1,
		 "machine":{"gpuType":{"displayName":"NVIDIA RTX A6000"}}},
		{"id":"ids","desiredStatus":"RUNNING","gpuTypeIds":["NVIDIA RTX 4090"],
		 "runtime":{"ports":[{"privatePort":22,"publicPort":25408,"ip":"0.tcp.ap.ngrok.io","type":"tcp"}]}}
	]`
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if insts[0].Name != "voice-worker" || insts[0].GPUModel != "NVIDIA RTX A6000" {
		t.Fatalf("nested instance = %+v", insts[0])
	}
	if insts[1].Name != "ids" || insts[1].GPUModel != "NVIDIA RTX 4090" || insts[1].GPUCount != 1 || insts[1].PublicIP != "0.tcp.ap.ngrok.io" || insts[1].SSHPort != 25408 {
		t.Fatalf("ids instance = %+v", insts[1])
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
				if tc.status == http.StatusOK {
					w.Write([]byte("[]"))
				}
			})
			err := d.ValidateCredentials(context.Background(), provider.Credentials{APIKey: "k"})
			if tc.wantErr && err == nil {
				t.Errorf("want error for status %d, got nil", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for status %d: %v", tc.status, err)
			}
		})
	}
}

func TestPowerState(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   provider.PowerState
	}{
		{"RUNNING", provider.PowerRunning},
		{"EXITED", provider.PowerStopped},
	} {
		d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/pods/") {
				t.Errorf("unexpected path %q", r.URL.Path)
			}
			w.Write([]byte(`{"id":"x","desiredStatus":"` + tc.status + `"}`))
		})
		got, err := d.PowerState(context.Background(), provider.Credentials{APIKey: "k"}, "x")
		if err != nil {
			t.Fatalf("PowerState: %v", err)
		}
		if got != tc.want {
			t.Errorf("status %s -> %q, want %q", tc.status, got, tc.want)
		}
	}
}

// A pod needs no credential from the operator any more; with neither the
// platform key nor anything else the installer says so instead of asking for
// a private key.
func TestInstallAgentWithoutAnyCredentialSaysWhy(t *testing.T) {
	d := New()
	_, err := d.InstallAgentWithReport(context.Background(), provider.Credentials{}, provider.InstallRequest{
		Instance:        provider.Instance{ExternalID: "p1", PublicIP: "203.0.113.7", SSHPort: 22, SSHUser: "root"},
		ServerURL:       "https://server.example",
		Token:           "tok",
		ReadAgentBinary: func(string) ([]byte, error) { return []byte("bin"), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "no ssh credential") || strings.Contains(err.Error(), "private key is required") {
		t.Fatalf("err = %v", err)
	}
}

const platformPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlatformKeyBody gpuworks@server"

// The account already holds the operator's own keys; adding the platform
// key must keep them, since RunPod's PUT replaces the whole list.
func TestAuthorizePublicKeyAppendsToTheAccountKeys(t *testing.T) {
	var put []string
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/account/ssh-keys" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"keys":["ssh-rsa AAAAB3NzaC1yc2EOperator laptop"]}`))
		case http.MethodPut:
			var body struct {
				Keys []string `json:"keys"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PUT: %v", err)
			}
			put = body.Keys
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("method %s", r.Method)
		}
	})
	check, err := d.AuthorizePublicKey(context.Background(), provider.Credentials{APIKey: "k"}, "ignored", platformPub)
	if err != nil || check.Status != "pass" || check.Code != "CHECK_PLATFORM_KEY_ACCOUNT_ADDED" || check.Params["provider"] != "runpod" || !strings.Contains(check.Message, "added to the RunPod account") {
		t.Fatalf("check=%+v err=%v", check, err)
	}
	want := []string{"ssh-rsa AAAAB3NzaC1yc2EOperator laptop", platformPub}
	if strings.Join(put, "|") != strings.Join(want, "|") {
		t.Fatalf("PUT keys = %q, want %q", put, want)
	}
}

func TestAuthorizePublicKeySkipsAKeyAlreadyThere(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s: the key is already registered", r.Method)
		}
		// Same key, another comment.
		_, _ = w.Write([]byte(`{"keys":["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlatformKeyBody renamed"]}`))
	})
	check, err := d.AuthorizePublicKey(context.Background(), provider.Credentials{APIKey: "k"}, "", platformPub)
	if err != nil || check.Code != "CHECK_PLATFORM_KEY_ACCOUNT_PRESENT" || !strings.Contains(check.Message, "already in") {
		t.Fatalf("check=%+v err=%v", check, err)
	}
}

func TestAuthorizePublicKeyErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		get    int
		put    int
		want   string
		pubKey string
	}{
		{name: "list fails", get: http.StatusUnauthorized, want: "list SSH keys: status 401"},
		{name: "replace fails", get: http.StatusOK, put: http.StatusBadRequest, want: "replace SSH keys: status 400"},
		{name: "malformed key", pubKey: "garbage", want: "invalid public key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(tc.get)
					_, _ = w.Write([]byte(`{"keys":[]}`))
					return
				}
				w.WriteHeader(tc.put)
				_, _ = w.Write([]byte(`{"error":"bad"}`))
			})
			key := tc.pubKey
			if key == "" {
				key = platformPub
			}
			if _, err := d.AuthorizePublicKey(context.Background(), provider.Credentials{APIKey: "k"}, "", key); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInstallAgentRequiresAgentBinaryReader(t *testing.T) {
	d := New()
	err := d.InstallAgent(context.Background(), provider.Credentials{}, provider.InstallRequest{
		Instance: provider.Instance{
			ExternalID: "p1",
			PublicIP:   "203.0.113.10",
			SSHUser:    "root",
			SSHPort:    22022,
		},
		SSHKeyPEM: "configured",
	})
	if err == nil || !strings.Contains(err.Error(), "agent binary reader is not configured") {
		t.Errorf("want agent-binary-reader error, got %v", err)
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := provider.Get("runpod"); !ok {
		t.Error("runpod driver not registered")
	}
}

func TestCapabilities(t *testing.T) {
	c := New().Capabilities()
	if !c.CanListInstances || c.CanPowerControl || !c.CanInstallAgent || !c.CanCreate || !c.CanDelete || !c.HasPricing {
		t.Errorf("unexpected caps: %+v", c)
	}
}

func TestCreateInstance(t *testing.T) {
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/pods" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer key123" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers = %v", r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got["name"] != "gpuworks-test-run-1" || got["image"] != "runpod/pytorch:test" || got["disk"] != float64(40) || got["startSsh"] != true {
			t.Fatalf("create body = %#v", got)
		}
		if _, ok := got["cloud"]; ok {
			t.Fatalf("create body unexpectedly specifies cloud tier: %#v", got["cloud"])
		}
		// The agent's default port is exposed with SSH: exposing it later
		// restarts the pod.
		if ports, _ := json.Marshal(got["ports"]); string(ports) != `["22/tcp","18800/tcp"]` {
			t.Fatalf("create ports = %s", ports)
		}
		gpu, ok := got["gpu"].(map[string]any)
		if !ok || gpu["id"] != "NVIDIA RTX 4090" || gpu["count"] != float64(2) {
			t.Fatalf("create gpu = %#v", got["gpu"])
		}
		env, ok := got["env"].(map[string]any)
		if !ok || env["GPUWORKS_TEST"] != "true" || env["GPUWORKS_TEST_RUN_ID"] != "run-1" {
			t.Fatalf("create env = %#v", got["env"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"pod":{"id":"pod-new","name":"gpuworks-test-run-1","status":"RUNNING","gpu":{"count":2,"id":"NVIDIA RTX 4090"}}}`))
	})
	inst, err := d.CreateInstance(context.Background(), provider.Credentials{APIKey: "key123"}, provider.CreateInstanceRequest{
		Name: "gpuworks-test-run-1", GPUModel: "NVIDIA RTX 4090", GPUCount: 2, Image: "runpod/pytorch:test",
		Extra: map[string]string{"disk_gb": "40", "env.GPUWORKS_TEST": "true", "env.GPUWORKS_TEST_RUN_ID": "run-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inst.ExternalID != "pod-new" || inst.GPUModel != "NVIDIA RTX 4090" || inst.GPUCount != 2 {
		t.Fatalf("created instance = %+v", inst)
	}
}

func TestDeleteInstanceIsIdempotent(t *testing.T) {
	var calls int
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/pods/pod%2Fone" {
			t.Fatalf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		if calls == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	for range 2 {
		if err := d.DeleteInstance(context.Background(), provider.Credentials{APIKey: "k"}, "pod/one"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefaultEndpointUsesV2(t *testing.T) {
	d := New()
	for _, creds := range []provider.Credentials{{}, {Endpoint: defaultBaseURL}, {Endpoint: defaultBaseURL + "/"}} {
		if got := d.apiBaseURL(creds); got != defaultBaseURL {
			t.Fatalf("apiBaseURL(%q) = %q", creds.Endpoint, got)
		}
	}
	const custom = "https://runpod-proxy.example/api"
	if got := d.apiBaseURL(provider.Credentials{Endpoint: custom + "/"}); got != custom {
		t.Fatalf("custom apiBaseURL = %q", got)
	}
}

// Only a pod that has exited is stopped; one being created or restarting is
// on its way up.
func TestPodPowerStoppedOnlyWhenExited(t *testing.T) {
	for status, want := range map[string]provider.PowerState{
		"RUNNING": provider.PowerRunning, "EXITED": provider.PowerStopped, "TERMINATED": provider.PowerStopped,
		"CREATED": provider.PowerUnknown, "RESTARTING": provider.PowerUnknown, "": provider.PowerUnknown,
	} {
		p := pod{Status: status}
		if got := p.power(); got != want {
			t.Errorf("%q: power = %s, want %s", status, got, want)
		}
	}
}

// Endpoint queries belong to every provider request, after its operation path.
// Keep their raw encoding/order and escaped proxy prefix rather than composing
// a different query or decoding a slash that belongs inside one path segment.
func TestEndpointQueryAppliesToPodAndAccountKeyPaths(t *testing.T) {
	const rawQuery = "tenant=e2e-query&label=one%2Ftwo%2Bthree&tail=value/"
	for _, prefix := range []string{"/proxy-a/v2", "/proxy%2Ftenant/v2"} {
		t.Run(prefix, func(t *testing.T) {
			observed := make(chan string, 3)
			keys := []string{"ssh-ed25519 AAAAfixture existing@example"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- r.Method + " " + r.RequestURI
				if r.URL.RawQuery != rawQuery || r.Header.Get("Authorization") != "Bearer query-key" {
					t.Errorf("query/auth differs: target=%q auth=%q", r.RequestURI, r.Header.Get("Authorization"))
					http.Error(w, "query or authentication mismatch", http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.EscapedPath() {
				case "GET " + prefix + "/pods":
					io.WriteString(w, `{"pods":[],"pagination":{"nextCursor":null,"hasNextPage":false}}`)
				case "GET " + prefix + "/account/ssh-keys":
					json.NewEncoder(w).Encode(map[string]any{"keys": keys})
				case "PUT " + prefix + "/account/ssh-keys":
					var body struct {
						Keys []string `json:"keys"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode SSH key replacement: %v", err)
						http.Error(w, "invalid JSON", http.StatusBadRequest)
						return
					}
					if len(body.Keys) != 1 || body.Keys[0] != "ssh-ed25519 AAAAfixture replacement@example" {
						t.Errorf("SSH key replacement body = %#v", body)
					}
					keys = body.Keys
					json.NewEncoder(w).Encode(map[string]any{"keys": keys})
				default:
					t.Errorf("unexpected endpoint path: %s %s", r.Method, r.RequestURI)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			d := New()
			d.http = srv.Client()
			creds := provider.Credentials{APIKey: "query-key", Endpoint: "  " + srv.URL + prefix + "///?" + rawQuery + "  "}
			if instances, err := d.ListInstances(t.Context(), creds); err != nil || len(instances) != 0 {
				t.Errorf("ListInstances = %#v, %v", instances, err)
			}
			if got, err := d.ListSSHKeys(t.Context(), creds); err != nil || len(got) != 1 || got[0] != keys[0] {
				t.Errorf("ListSSHKeys = %#v, %v", got, err)
			}
			if err := d.ReplaceSSHKeys(t.Context(), creds, []string{"ssh-ed25519 AAAAfixture replacement@example"}); err != nil {
				t.Errorf("ReplaceSSHKeys: %v", err)
			}
			want := []string{"GET " + prefix + "/pods?" + rawQuery,
				"GET " + prefix + "/account/ssh-keys?" + rawQuery,
				"PUT " + prefix + "/account/ssh-keys?" + rawQuery}
			var requests []string
			for len(observed) > 0 {
				requests = append(requests, <-observed)
			}
			if strings.Join(requests, "\n") != strings.Join(want, "\n") {
				t.Errorf("outbound calls = %#v, want %#v", requests, want)
			}
		})
	}
}

// A pod's keys are the PUBLIC_KEY it was created with (an edit keeps it); a key
// the install appends to authorized_keys sits on the container disk the edit
// clears (observed 2026-10-26: the platform key added at install was gone
// after a ports edit, the rented key in PUBLIC_KEY still logged in).
func TestPlatformKeyVolatileUnlessThePodEnvCarriesIt(t *testing.T) {
	const platform = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlatformKeyMaterial gpuworks@server"
	const rented = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIRentedKeyMaterial rent@test"
	for _, tc := range []struct {
		name       string
		env        string
		status     int
		authorized bool
		noKey      bool
		want       bool
	}{
		{name: "key appended to authorized_keys only", env: rented, status: 200, authorized: true, want: true},
		{name: "key in PUBLIC_KEY", env: rented + "\n" + platform, status: 200, authorized: true, want: false},
		{name: "same key with another comment", env: strings.Replace(platform, "gpuworks@server", "other", 1), status: 200, authorized: true, want: false},
		{name: "pod unreadable", status: 500, authorized: true, want: true},
		{name: "platform key not authorized", env: rented, status: 200, authorized: false, want: false},
		{name: "no platform key", env: rented, status: 200, authorized: true, noKey: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/pods/pod-1" {
					t.Errorf("path = %q", r.URL.Path)
				}
				if tc.status != 200 {
					w.WriteHeader(tc.status)
					return
				}
				body, _ := json.Marshal(map[string]any{"id": "pod-1", "status": "RUNNING", "env": map[string]string{"PUBLIC_KEY": tc.env, "GPUWORKS_TEST": "1"}})
				_, _ = w.Write(body)
			})
			req := provider.InstallRequest{Instance: provider.Instance{ExternalID: "pod-1"}}
			if !tc.noKey {
				req.PlatformKey = &provider.PlatformKey{PublicKey: platform}
			}
			got := d.platformKeyVolatile(context.Background(), provider.Credentials{APIKey: "k"}, req, provider.InstallReport{PlatformKeyAuthorized: tc.authorized})
			if got != tc.want {
				t.Fatalf("platformKeyVolatile = %v, want %v", got, tc.want)
			}
		})
	}
}
