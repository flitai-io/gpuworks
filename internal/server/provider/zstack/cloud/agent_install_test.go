package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

type cloudHostInstaller struct {
	request *provision.SSHInstallRequest
	err     error
}

func (i *cloudHostInstaller) InstallSSH(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	i.request = &req
	if i.err != nil {
		return provision.SSHInstallReport{}, i.err
	}
	return provision.SSHInstallReport{Checks: []provision.CheckResult{{ID: "agent_process", Status: "pass", Message: "systemd"}}}, nil
}

func TestInstallAgentOnHostWithoutGPUs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !validTestAccessKey(r) {
			t.Errorf("unexpected Cloud mutation or missing authorization: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/hosts":
			_, _ = w.Write([]byte(`{"inventories":[{"uuid":"host-empty","managementIp":"192.0.2.11","hypervisorType":"KVM","state":"Enabled","status":"Connected"}]}`))
		case "/v1/gpu-device/gpu-devices":
			_, _ = w.Write([]byte(`{"inventories":[]}`))
		case "/v1/resource-configurations/host-empty/kvm/reservedMemory":
			_, _ = w.Write([]byte(`{"value":"1G"}`))
		case "/v1/resource-configurations/host-empty/kvm/reservedCpu":
			_, _ = w.Write([]byte(`{"value":"0"}`))
		default:
			t.Errorf("unexpected Cloud request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for _, tc := range []struct {
		name      string
		err       error
		wantError string
	}{
		{name: "success"},
		{name: "upload failure", err: errors.New("upload failed"), wantError: "Cloud agent installation failed"},
		{name: "SSH failure", err: &provision.SSHConnectionError{Err: errors.New("connection refused")}, wantError: "Cloud SSH connection failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(":memory:", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			p, err := st.CreateProvider(&store.Provider{Name: "cloud", Kind: "zstack"})
			if err != nil {
				t.Fatal(err)
			}
			node, token, err := st.CreateNodePending("host-empty", "agent_dial")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SyncProviderInstances(p.ID, []store.ProviderInstance{{ProviderID: p.ID, ExternalID: "host-empty", NodeID: node.ID}}); err != nil {
				t.Fatal(err)
			}
			installer := &cloudHostInstaller{err: tc.err}
			driver := &Driver{store: st, installer: installer}
			report, err := driver.InstallAgentWithReport(t.Context(), provider.Credentials{
				Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret",
			}, provider.InstallRequest{
				Instance:  provider.Instance{ExternalID: "host-empty", PublicIP: "192.0.2.11", SSHUser: "root", SSHPort: 22},
				ServerURL: "https://control.example", Token: token, Password: "pw", ConnectMode: "agent_dial",
			})
			if installer.request == nil || installer.request.Target != "192.0.2.11" || installer.request.Token != token {
				t.Fatalf("host without GPUs did not reach SSH installation: request=%+v, err=%v", installer.request, err)
			}
			if tc.err != nil {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("installation failure was suppressed: %v", err)
				}
				return
			}
			if err != nil || len(report.Checks) != 1 || report.Checks[0].ID != "agent_process" || report.Checks[0].Status != "pass" {
				t.Fatalf("installation result = %+v, %v", report, err)
			}
			host, err := cloudstore.New(st.DB()).CloudHostByNodeUUID(node.UUID)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot inventory.CloudResource
			if err := json.Unmarshal([]byte(host.Snapshot), &snapshot); err != nil || snapshot.ExternalID != "host-empty" || len(snapshot.GPUs) != 0 {
				t.Fatalf("host without GPUs lost its Cloud inventory: %+v, %v", snapshot, err)
			}
		})
	}
}
