package autodl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

const listResponse = `{
  "code": "Success",
  "msg": "",
  "data": {
    "max_page": 1,
    "result_total": 2,
    "list": [
      {
        "uuid": "pro-76576c61fdf1",
        "name": "train-a",
        "status": "running",
        "gpu_name": "RTX 4090",
        "gpu_spec_uuid": "v-48g",
        "req_gpu_amount": 2,
        "region_name": "西北企业区",
        "region_sign": "westDC2"
      },
      {
        "uuid": "pro-aaaabbbbcccc",
        "name": "idle-b",
        "status": "shutdown",
        "gpu_spec_uuid": "h800",
        "req_gpu_amount": 1,
        "region_sign": "westDC3"
      }
    ]
  }
}`

func newMock(t *testing.T, handler http.HandlerFunc) *Driver {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	d := New()
	d.setBaseURL(srv.URL)
	return d
}

func TestListInstancesMapping(t *testing.T) {
	var gotAuth, gotPath string
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case listPath:
			gotAuth = r.Header.Get("Authorization")
			gotPath = r.URL.Path
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			var body listRequest
			json.NewDecoder(r.Body).Decode(&body)
			if body.PageSize == 0 {
				t.Errorf("page_size not sent")
			}
			_, _ = w.Write([]byte(listResponse))
		case snapshotPath:
			if got := r.URL.Query().Get("instance_uuid"); got != "pro-76576c61fdf1" {
				t.Fatalf("snapshot instance_uuid = %q, want pro-76576c61fdf1", got)
			}
			_, _ = w.Write([]byte(`{"code":"Success","data":{"name":"train-a","status":"running","region_sign":"西北企业区","payg_price":3450,"snapshot_gpu_alias_name":"RTX 4090","req_gpu_amount":2,"proxy_host":"region-1.autodl.com","ssh_port":12345}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})

	got, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "tok-123"})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if gotAuth != "tok-123" {
		t.Errorf("Authorization = %q, want tok-123", gotAuth)
	}
	if gotPath != listPath {
		t.Errorf("path = %q, want %q", gotPath, listPath)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	a := got[0]
	if a.ExternalID != "pro-76576c61fdf1" || a.Name != "train-a" {
		t.Errorf("instance[0] id/name = %q/%q", a.ExternalID, a.Name)
	}
	if a.GPUModel != "RTX 4090" || a.GPUCount != 2 {
		t.Errorf("instance[0] gpu = %q x%d", a.GPUModel, a.GPUCount)
	}
	if a.Power != provider.PowerRunning {
		t.Errorf("instance[0] power = %q, want running", a.Power)
	}
	if a.PricePerHr != 3.45 {
		t.Errorf("instance[0] price = %v", a.PricePerHr)
	}
	if a.Region != "西北企业区" {
		t.Errorf("instance[0] region = %q", a.Region)
	}
	if a.PublicIP != "region-1.autodl.com" || a.SSHPort != 12345 || a.SSHUser != "root" {
		t.Errorf("instance[0] ssh = %s:%d %s", a.PublicIP, a.SSHPort, a.SSHUser)
	}
	if a.Kind != "container" {
		t.Errorf("instance[0] kind = %q, want container", a.Kind)
	}

	b := got[1]
	if b.Power != provider.PowerStopped {
		t.Errorf("instance[1] power = %q, want stopped", b.Power)
	}
	if b.GPUModel != "h800" { // falls back to gpu_spec_uuid
		t.Errorf("instance[1] gpu = %q, want h800", b.GPUModel)
	}
	if b.Region != "westDC3" { // falls back to region_sign
		t.Errorf("instance[1] region = %q", b.Region)
	}
	if b.PublicIP != "" || b.SSHPort != 0 || b.PricePerHr != 0 {
		t.Errorf("stopped list instance unexpectedly contains snapshot fields: %+v", b)
	}
}

func TestListInstancesTraversesAllPages(t *testing.T) {
	var pages []int
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != listPath {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var body listRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		pages = append(pages, body.PageIndex)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"code":"Success","data":{"max_page":2,"list":[{"uuid":"pro-%d","status":"shutdown"}]}}`, body.PageIndex)))
	})
	got, err := d.ListInstances(t.Context(), provider.Credentials{APIKey: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || strings.Join([]string{got[0].ExternalID, got[1].ExternalID}, ",") != "pro-1,pro-2" {
		t.Fatalf("instances = %+v", got)
	}
	if len(pages) != 2 || pages[0] != 1 || pages[1] != 2 {
		t.Fatalf("pages = %v", pages)
	}
}

func TestValidateCredentialsOK(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":"Success","data":{"list":[]}}`))
	})
	if err := d.ValidateCredentials(context.Background(), provider.Credentials{APIKey: "ok"}); err != nil {
		t.Fatalf("ValidateCredentials: %v", err)
	}
}

func TestValidateCredentialsAuthError(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"Unauthorized","msg":"bad token"}`))
	})
	if err := d.ValidateCredentials(context.Background(), provider.Credentials{APIKey: "bad"}); err == nil {
		t.Fatal("ValidateCredentials: want auth error, got nil")
	}
}

func TestValidateCredentialsAPIErrorCode(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 body but error code
		w.Write([]byte(`{"code":"AuthError","msg":"invalid token"}`))
	})
	if err := d.ValidateCredentials(context.Background(), provider.Credentials{APIKey: "bad"}); err == nil {
		t.Fatal("ValidateCredentials: want error from code, got nil")
	}
}

func TestPowerState(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != statusPath {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("instance_uuid"); got != "pro-aaaabbbbcccc" {
			t.Fatalf("status instance_uuid = %q, want pro-aaaabbbbcccc", got)
		}
		if r.ContentLength != 0 {
			t.Fatalf("status GET content length = %d, want 0", r.ContentLength)
		}
		w.Write([]byte(`{"code":"Success","data":"shutdown"}`))
	})
	ps, err := d.PowerState(context.Background(), provider.Credentials{APIKey: "t"}, "pro-aaaabbbbcccc")
	if err != nil {
		t.Fatalf("PowerState: %v", err)
	}
	if ps != provider.PowerStopped {
		t.Errorf("power = %q, want stopped", ps)
	}
}

func TestTransitionalPowerStatesRemainUnknown(t *testing.T) {
	for _, status := range []string{"starting", "stopping"} {
		if got := mapPower(status); got != provider.PowerUnknown {
			t.Errorf("mapPower(%q) = %q, want unknown", status, got)
		}
	}
}

func TestPowerControl(t *testing.T) {
	var paths []string
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "tok" {
			t.Errorf("Authorization = %q, want tok", got)
		}
		var body powerRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.InstanceUUID != "instance-1" {
			t.Errorf("instance_uuid = %q, want instance-1", body.InstanceUUID)
		}
		if r.URL.Path == powerOnPath && body.Payload != "gpu" {
			t.Errorf("power-on payload = %q, want gpu", body.Payload)
		}
		w.Write([]byte(`{"code":"Success","msg":""}`))
	})
	if err := d.PowerOn(context.Background(), provider.Credentials{APIKey: "tok"}, "instance-1"); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if err := d.PowerOff(context.Background(), provider.Credentials{APIKey: "tok"}, "instance-1"); err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	if len(paths) != 2 || paths[0] != powerOnPath || paths[1] != powerOffPath {
		t.Fatalf("power paths = %v, want [%s %s]", paths, powerOnPath, powerOffPath)
	}
}

func TestRegisteredAndCapabilities(t *testing.T) {
	drv, ok := provider.Get("autodl")
	if !ok {
		t.Fatal("autodl not registered")
	}
	c := drv.Capabilities()
	if !c.CanListInstances || !c.CanInstallAgent || !c.CanPowerControl || !c.CanCreate || !c.CanDelete {
		t.Errorf("caps = %+v", c)
	}
}

func TestCreateInstanceUsesDocumentedProAPI(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != createPath {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body createRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ReqGPUAmount != 1 || body.GPUSpecUUID != "v-32g-p" || body.ImageUUID != "base-image-l2t43iu6uk" || body.CUDAFrom != 118 || body.ExpandSystemDiskGB != 20 || body.InstanceName != "gpuworks-test-run" {
			t.Fatalf("create body = %+v", body)
		}
		if len(body.DataCenterList) != 2 || body.DataCenterList[0] != "westDC3" || body.DataCenterList[1] != "beijingDC2" {
			t.Fatalf("data centers = %v", body.DataCenterList)
		}
		_, _ = w.Write([]byte(`{"code":"Success","data":"pro-created","msg":""}`))
	})
	instance, err := d.CreateInstance(t.Context(), provider.Credentials{APIKey: "token"}, provider.CreateInstanceRequest{
		Name: "gpuworks-test-run", Region: "westDC3,beijingDC2", GPUModel: "RTX 4080S", GPUCount: 1,
		Image: "base-image-l2t43iu6uk", Extra: map[string]string{"expand_system_disk_by_gb": "20", "cuda_v_from": "118"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ExternalID != "pro-created" || instance.GPUModel != "RTX 4080S" || instance.Kind != "container" {
		t.Fatalf("created instance = %+v", instance)
	}
}

func TestSnapshotMapsDocumentedSSHAndPrice(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != snapshotPath {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("instance_uuid"); got != "pro-1" {
			t.Fatalf("snapshot instance_uuid = %q, want pro-1", got)
		}
		if r.ContentLength != 0 {
			t.Fatalf("snapshot GET content length = %d, want 0", r.ContentLength)
		}
		_, _ = w.Write([]byte(`{"code":"Success","data":{"name":"gpuworks-test-run","region_sign":"westDC3","payg_price":1970,"snapshot_gpu_alias_name":"NVIDIA RTX 4080S","req_gpu_amount":1,"ssh_command":"ssh -p 34222 root@connect.example","proxy_host":"connect.example","root_password":"pw-secret","ssh_port":34222}}`))
	})
	snapshot, err := d.Snapshot(t.Context(), provider.Credentials{APIKey: "token"}, "pro-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Instance.ExternalID != "pro-1" || snapshot.Instance.PublicIP != "connect.example" || snapshot.Instance.SSHPort != 34222 || snapshot.Instance.PricePerHr != 1.97 || snapshot.Instance.GPUModel != "NVIDIA RTX 4080S" || snapshot.RootPassword != "pw-secret" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestDeleteInstancePowersOffBeforeRelease(t *testing.T) {
	state := "running"
	var paths []string
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case statusPath:
			_, _ = w.Write([]byte(`{"code":"Success","data":"` + state + `"}`))
		case powerOffPath:
			state = "shutdown"
			_, _ = w.Write([]byte(`{"code":"Success","data":null}`))
		case releasePath:
			if state != "shutdown" {
				t.Fatal("release happened before shutdown")
			}
			var body instanceRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.InstanceUUID != "pro-1" {
				t.Fatalf("release body = %+v", body)
			}
			_, _ = w.Write([]byte(`{"code":"Success","data":null}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	if err := d.DeleteInstance(t.Context(), provider.Credentials{APIKey: "token"}, "pro-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{statusPath, powerOffPath, statusPath, releasePath}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths=%v want=%v", paths, want)
	}
}

func TestInstallAgentNoCredential(t *testing.T) {
	d := New()
	req := provider.InstallRequest{
		Instance:  provider.Instance{ExternalID: "x", PublicIP: "h", SSHPort: 22},
		ServerURL: "https://s", Token: "tk",
	}
	if err := d.InstallAgent(context.Background(), provider.Credentials{}, req); err == nil {
		t.Fatal("InstallAgent: want error when no SSH credential")
	}
}

// The provider runs the shared script install (provision.ScriptInstallSteps,
// whose download failure handling is tested there) behind what its images
// need, then the shared check that the agent runs.
func TestInstallScriptCommandRunsTheSharedScriptInstall(t *testing.T) {
	for _, opts := range []provision.ScriptOptions{{}, {InstallDir: "/data/gpuworks", NoSystemd: true}} {
		cmd := installScriptCommand("http://127.0.0.1:8800/", "tok'quoted", "agent_dial", ":19090", opts)
		for _, want := range []string{"/root/miniconda3/bin", "export PATH", "NO_PROXY", provision.ScriptInstallSteps("http://127.0.0.1:8800/", "tok'quoted", "agent_dial", ":19090", opts), provision.AgentRunningCheckCommand(opts.InstallDir)} {
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

func TestInstancePasswordComesFromTheSnapshot(t *testing.T) {
	d := newMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != snapshotPath || r.URL.Query().Get("instance_uuid") != "pro-1" {
			t.Fatalf("request = %s %s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"code":"Success","data":{"ssh_command":"ssh -p 34222 root@connect.example","proxy_host":"connect.example","root_password":"pw-secret","ssh_port":34222}}`))
	})
	password, err := d.InstancePassword(t.Context(), provider.Credentials{APIKey: "token"}, "pro-1")
	if err != nil || password != "pw-secret" {
		t.Fatalf("password=%q err=%v", password, err)
	}
}
