package zstackcloud_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	zstackcloud "gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

type installerFunc func(context.Context, provision.SSHInstallRequest) (provision.SSHInstallReport, error)

func (f installerFunc) InstallSSH(ctx context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	return f(ctx, req)
}

type installationTestDriver struct {
	*zstackcloud.Driver
	installer provision.Installer
	precheck  provision.Prechecker // nil keeps the real SSH precheck
}

func (d *installationTestDriver) NewDriver(st *store.Store, svc *provider.Service, _ provision.Installer) provider.Driver {
	if d.precheck != nil {
		svc.SetSSHPrechecker(d.precheck)
	}
	return d.Driver.NewDriver(st, svc, d.installer)
}

// newCloudEnv is a server whose Cloud driver installs with installer and
// prechecks through an sshPrecheckFixture: the hosts the Cloud fixtures list
// have no SSH server behind them.
func newCloudEnv(t *testing.T, installer provision.Installer) *testenv.Env {
	t.Helper()
	return newCloudEnvPrechecking(t, installer, &sshPrecheckFixture{})
}

// newCloudEnvWithSSHServer keeps the real SSH precheck, for a test that
// serves SSH at the hosts' address itself.
func newCloudEnvWithSSHServer(t *testing.T, installer provision.Installer) *testenv.Env {
	t.Helper()
	return newCloudEnvPrechecking(t, installer, nil)
}

// These tests run serially. The registry override lasts only through app creation;
// each configured driver retains its own installer after the prototype is restored.
func newCloudEnvPrechecking(t *testing.T, installer provision.Installer, precheck provision.Prechecker) *testenv.Env {
	t.Helper()
	driver := &installationTestDriver{Driver: zstackcloud.New(), installer: installer, precheck: precheck}
	original, ok := provider.Get(driver.Name())
	if !ok {
		t.Fatal("Cloud driver is not registered")
	}
	provider.Register(driver)
	defer provider.Register(original)
	return testenv.New(t)
}

func addSyncInstall(t *testing.T, env *testenv.Env, endpoint, hostID string) string {
	t.Helper()
	api := env.API(t)
	request, _ := json.Marshal(map[string]any{"kind": "zstack", "name": "Test Cloud", "endpoint": endpoint, "public_key": "test-access-key", "private_key": "test-access-secret"})
	req, _ := http.NewRequest(http.MethodPost, env.URL()+"/api/v1/providers", bytes.NewReader(request))
	resp, err := api.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID uint `json:"id"`
	}
	if resp.StatusCode != 201 {
		t.Fatalf("add status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	path := fmt.Sprintf("/api/v1/providers/%d", created.ID)
	for range 2 {
		req, _ = http.NewRequest(http.MethodPost, env.URL()+path+"/sync", nil)
		resp, err = api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("sync status %d: %s", resp.StatusCode, body)
		}
	}
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 0 {
		t.Fatalf("sync must not create compute nodes: nodes=%v err=%v", nodes, err)
	}
	raw := requestJSON(t, env, "GET", path+"/instances", "", http.StatusOK)
	var listed []struct {
		NodeID       uint `json:"node_id"`
		Installation any  `json:"installation"`
	}
	if err := apiwire.UnmarshalList(raw, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) == 0 || listed[0].NodeID != 0 || listed[0].Installation != nil {
		t.Fatalf("discovered inventory must remain uninstalled: %s", raw)
	}
	var gpuCount int64
	if err := env.Store().DB().Table("gpus").Count(&gpuCount).Error; err != nil || gpuCount != 0 {
		t.Fatalf("sync created GPU rows: count=%d err=%v", gpuCount, err)
	}
	request, _ = json.Marshal(map[string]any{"external_id": hostID, "password": "ssh-secret", "ssh_key_pem": "", "ssh_connection_id": "auto", "connect_mode": "agent_dial", "server_url": env.URL()})
	{
		req, _ = http.NewRequest(http.MethodPost, env.URL()+path+"/install", bytes.NewReader(request))
		resp, err = api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("install status %d: %s", resp.StatusCode, body)
		}
		if bytes.Contains(body, []byte("ssh-secret")) || bytes.Contains(body, []byte("test-access-secret")) {
			t.Fatal("install response leaked credentials")
		}
		var accepted struct {
			NodeUUID string `json:"node_uuid"`
		}
		if err := json.Unmarshal(body, &accepted); err != nil {
			t.Fatal(err)
		}

	}
	providers, err := env.Store().ListProviders()
	if err != nil || len(providers) != 1 {
		t.Fatalf("install duplicated provider: %v %v", providers, err)
	}
	return path
}

func waitForInstalledHost(t *testing.T, env *testenv.Env, providerPath, hostID string) string {
	t.Helper()
	var nodeID uint
	env.Eventually(t, 15*time.Second, func() bool {
		raw := requestJSON(t, env, "GET", providerPath+"/instances", "", http.StatusOK)
		if bytes.Contains(raw, []byte("ssh-secret")) || bytes.Contains(raw, []byte("test-access-secret")) {
			t.Fatal("instance list leaked credentials")
		}
		var instances []struct {
			ExternalID string `json:"external_id"`
			NodeID     uint   `json:"node_id"`
		}
		if err := apiwire.UnmarshalList(raw, &instances); err != nil {
			t.Fatal(err)
		}
		nodeID = 0
		for _, instance := range instances {
			if instance.ExternalID == hostID {
				nodeID = instance.NodeID
				break
			}
		}
		if nodeID == 0 {
			return false
		}
		node, err := env.Store().NodeByID(nodeID)
		return err == nil && node.State == store.NodeStateOnline && node.GPUCount > 0 && node.MemTotalKB > 0
	})
	node, err := env.Store().NodeByID(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	return node.UUID
}

func TestZStackCloudOnboardingRegistersGPUHostsWithoutReservingResources(t *testing.T) {
	var writes, installs atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validCloudAccessKey(r) {
			t.Errorf("invalid Cloud AccessKey authorization")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/v1/hosts/kvm/actions" {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"code":"ID.1002","details":"permission denied"}}`)
			return
		}
		if r.Method != http.MethodGet {
			writes.Add(1)
			http.Error(w, "onboarding must be read only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/hosts" || r.URL.Path == "/v1/gpu-device/gpu-devices" {
			q := r.URL.Query()
			fields := 3
			if r.URL.Path == "/v1/hosts" && q.Has("q") {
				fields++
				if len(q["q"]) != 1 || q.Get("q") != "hypervisorType=KVM" {
					t.Errorf("unexpected host filter: %s", r.URL.RawQuery)
				}
			}
			if q.Get("start") != "0" || q.Get("limit") != "100" || q.Get("sort") != "uuid" || len(q) != fields {
				t.Errorf("query must use ZStack REST pagination/sort parameters: %s", r.URL.RawQuery)
			}
		}
		switch r.URL.Path {
		case "/v1/hosts":
			io.WriteString(w, `{"inventories":[{"uuid":"host-gpu","name":"GPU host","managementIp":"192.0.2.10","hypervisorType":"KVM","state":"Enabled","status":"Connected","totalCpuCapacity":32,"availableCpuCapacity":24,"totalMemoryCapacity":68719476736,"availableMemoryCapacity":34359738368},{"uuid":"host-cpu","name":"CPU host","managementIp":"192.0.2.11","hypervisorType":"KVM","state":"Enabled","status":"Connected","totalCpuCapacity":16,"availableCpuCapacity":8,"totalMemoryCapacity":34359738368,"availableMemoryCapacity":17179869184}]}`)
		case "/v1/gpu-device/gpu-devices":
			io.WriteString(w, `{"inventories":[{"uuid":"gpu-a","hostUuid":"host-gpu","pciDeviceAddress":"0000:01:00.0","state":"Enabled","status":"Active","allocateStatus":"Unallocated","mode":"PCI"}]}`)
		case "/v1/resource-configurations/host-gpu/kvm/reservedMemory", "/v1/resource-configurations/host-cpu/kvm/reservedMemory":
			io.WriteString(w, `{"value":"4G","effectiveConfigs":[]}`)
		case "/v1/resource-configurations/host-gpu/kvm/reservedCpu", "/v1/resource-configurations/host-cpu/kvm/reservedCpu":
			io.WriteString(w, `{"value":"2","effectiveConfigs":[]}`)
		default:
			t.Errorf("unexpected Cloud request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cloud.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	driver := testenv.NewFakeDriver("fake")
	t.Cleanup(driver.CloseAll)
	stateDir := t.TempDir()
	installer := installerFunc(func(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
		installs.Add(1)
		if req.Target != "192.0.2.10" || req.Token == "" || req.Password != "ssh-secret" {
			t.Errorf("wrong SSH target, credential or missing node identity")
		}
		ag := agentapp.New(agentapp.Config{
			ServerURL: req.ServerURL, Token: req.Token, StateDir: stateDir,
			Driver: driver, ForceDriver: "fake",
			Prober: testenv.NewFakeProber([]proto.GPUInfo{{Index: 0, UUID: "GPU-a", Name: "A100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}}),
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		go func() { _ = ag.Run(ctx) }()
		return provision.SSHInstallReport{Host: req.Target, User: req.User, Port: req.Port}, nil
	})
	env := newCloudEnv(t, installer)
	api := env.API(t)
	path := addSyncInstall(t, env, cloud.URL, "host-gpu")
	nodeUUID := waitForInstalledHost(t, env, path, "host-gpu")
	host, err := cloudstore.New(env.Store().DB()).CloudHostByNodeUUID(nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	if host.InitialCPUReserved == nil || *host.InitialCPUReserved != 2 || host.InitialMemoryReserved == nil || *host.InitialMemoryReserved != 4<<30 {
		t.Errorf("initial Cloud baseline = cpu:%v memory:%v", host.InitialCPUReserved, host.InitialMemoryReserved)
	}
	var resource inventory.CloudResource
	if err := json.Unmarshal([]byte(host.Snapshot), &resource); err != nil {
		t.Fatal(err)
	}
	if resource.CPU.Reserved == nil || *resource.CPU.Reserved != 2 || resource.CPU.Unit != "vcpu" {
		t.Errorf("CPU baseline = %+v, want 2 vCPU", resource.CPU)
	}
	if resource.Memory.Reserved == nil || *resource.Memory.Reserved != 4<<30 {
		t.Errorf("memory baseline = %v", resource.Memory.Reserved)
	}
	node, err := api.Get("/api/v1/nodes/" + nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(node.Body)
	node.Body.Close()
	if node.StatusCode != 200 || !bytes.Contains(body, []byte(`"online"`)) {
		t.Fatalf("node: %s", body)
	}
	if writes.Load() != 0 || installs.Load() != 1 {
		t.Fatalf("Cloud writes=%d SSH installs=%d", writes.Load(), installs.Load())
	}
}

func TestZStackCloudOnboardingPreservesUsedGPUState(t *testing.T) {
	var writes, installs atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validCloudAccessKey(r) {
			t.Errorf("invalid Cloud AccessKey authorization")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/v1/hosts/kvm/actions" {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"code":"ID.1002","details":"permission denied"}}`)
			return
		}
		if r.Method != http.MethodGet {
			writes.Add(1)
			http.Error(w, "onboarding must be read only", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/v1/hosts" || r.URL.Path == "/v1/gpu-device/gpu-devices" {
			q := r.URL.Query()
			fields := 3
			if r.URL.Path == "/v1/hosts" && q.Has("q") {
				fields++
				if len(q["q"]) != 1 || q.Get("q") != "hypervisorType=KVM" {
					t.Errorf("unexpected host filter: %s", r.URL.RawQuery)
				}
			}
			if q.Get("start") != "0" || q.Get("limit") != "100" || q.Get("sort") != "uuid" || len(q) != fields {
				t.Errorf("query must use ZStack REST pagination/sort parameters: %s", r.URL.RawQuery)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hosts":
			io.WriteString(w, `{"inventories":[{"uuid":"host-gpu","name":"GPU host","managementIp":"192.0.2.20","hypervisorType":"KVM","state":"Enabled","status":"Connected","totalCpuCapacity":32,"availableCpuCapacity":24,"totalMemoryCapacity":68719476736,"availableMemoryCapacity":34359738368}]}`)
		case "/v1/gpu-device/gpu-devices":
			io.WriteString(w, `{"inventories":[{"uuid":"gpu-used","hostUuid":"host-gpu","pciDeviceAddress":"0000:01:00.0","state":"Enabled","status":"Active","allocateStatus":"Allocated","mode":"PCI","vmInstanceUuid":"vm-1"},{"uuid":"gpu-free","hostUuid":"host-gpu","pciDeviceAddress":"0000:02:00.0","state":"Enabled","status":"Active","allocateStatus":"Unallocated","mode":"PCI"}]}`)
		case "/v1/resource-configurations/host-gpu/kvm/reservedMemory":
			io.WriteString(w, `{"value":"4G","effectiveConfigs":[]}`)
		case "/v1/resource-configurations/host-gpu/kvm/reservedCpu":
			io.WriteString(w, `{"value":"2","effectiveConfigs":[]}`)
		default:
			t.Errorf("unexpected Cloud request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cloud.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	driver := testenv.NewFakeDriver("fake")
	t.Cleanup(driver.CloseAll)
	stateDir := t.TempDir()
	installer := installerFunc(func(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
		installs.Add(1)
		if req.Target != "192.0.2.20" || req.Token == "" || req.Password != "ssh-secret" {
			t.Errorf("wrong SSH target, credential or missing node identity")
		}
		ag := agentapp.New(agentapp.Config{
			ServerURL: req.ServerURL, Token: req.Token, StateDir: stateDir,
			Driver: driver, ForceDriver: "fake",
			Prober: testenv.NewFakeProber([]proto.GPUInfo{
				{Index: 0, UUID: "GPU-used", Name: "A100", Vendor: "nvidia", PCIeBDF: "0000:01:00.0", MemoryTotalBytes: 80 << 30, Healthy: true},
				{Index: 1, UUID: "GPU-free", Name: "A100", Vendor: "nvidia", PCIeBDF: "0000:02:00.0", MemoryTotalBytes: 80 << 30, Healthy: true},
			}),
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		go func() { _ = ag.Run(ctx) }()
		return provision.SSHInstallReport{Host: req.Target, User: req.User, Port: req.Port}, nil
	})
	env := newCloudEnv(t, installer)
	api := env.API(t)
	path := addSyncInstall(t, env, cloud.URL, "host-gpu")
	nodeUUID := waitForInstalledHost(t, env, path, "host-gpu")
	host, err := cloudstore.New(env.Store().DB()).CloudHostByNodeUUID(nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	var resource inventory.CloudResource
	if err := json.Unmarshal([]byte(host.Snapshot), &resource); err != nil {
		t.Fatal(err)
	}
	if len(resource.GPUs) != 2 {
		t.Fatalf("discovered GPUs = %+v", resource.GPUs)
	}
	byID := map[string]struct {
		allocation string
		state      string
	}{}
	for _, gpu := range resource.GPUs {
		byID[gpu.ExternalID] = struct {
			allocation string
			state      string
		}{allocation: gpu.Allocation, state: gpu.State}
	}
	if byID["gpu-used"].allocation != "Allocated" || byID["gpu-used"].state != "Enabled" {
		t.Fatalf("used GPU state was not preserved: %+v", byID["gpu-used"])
	}
	if byID["gpu-free"].allocation != "Unallocated" || byID["gpu-free"].state != "Enabled" {
		t.Fatalf("free GPU state was not preserved: %+v", byID["gpu-free"])
	}
	node, err := api.Get("/api/v1/nodes/" + nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Body.Close()
	var nodeView struct {
		GPUCount int `json:"gpu_count"`
		GPUs     []struct {
			UUID string `json:"uuid"`
		} `json:"gpus"`
	}
	if node.StatusCode != http.StatusOK || json.NewDecoder(node.Body).Decode(&nodeView) != nil {
		t.Fatalf("node status = %d", node.StatusCode)
	}
	if nodeView.GPUCount != 2 || len(nodeView.GPUs) != 2 {
		t.Fatalf("registered node GPUs = %+v", nodeView)
	}
	mutations, err := resourceclaim.New(env.Store(), nil).CloudResourceMutations(nodeUUID, []int{1})
	if err != nil || len(mutations) != 1 || mutations[0].ResourceID != "gpu-free" {
		t.Fatalf("registered Cloud node lost its resource mapping: %+v err=%v", mutations, err)
	}
	if writes.Load() != 0 || installs.Load() != 1 {
		t.Fatalf("Cloud writes=%d SSH installs=%d", writes.Load(), installs.Load())
	}
}
