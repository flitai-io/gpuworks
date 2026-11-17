package zstackcloud_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// Exercise the public deployment API through the real controller and agent.
// Only Cloud, SSH installation, GPU discovery and the container runtime are fake.
func TestZStackCloudDeploymentReservesGPUUntilAgentConfirmsRemoval(t *testing.T) {
	const imageRef = "gpuworks/fake-engine:test"
	var disabled, removed atomic.Bool
	var disables, enables atomic.Int32
	var targetHostQueries, targetGPUQueries atomic.Int32
	var description, pendingDescription atomic.Value
	description.Store("原有备注")
	pendingDescription.Store("原有备注")
	var pendingState atomic.Value
	pendingState.Store("")
	inventory := cloudInventoryFixture(t)
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validCloudAccessKey(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		state := "Enabled"
		if disabled.Load() {
			state = "Disabled"
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/hosts" && strings.HasPrefix(r.URL.Query().Get("q"), "uuid="):
			if query := r.URL.Query(); len(query) != 1 || len(query["q"]) != 1 || query.Get("q") != "uuid=host-a" {
				t.Errorf("unexpected target host query: %s", r.URL)
				http.Error(w, "invalid query", http.StatusBadRequest)
				return
			}
			targetHostQueries.Add(1)
			io.WriteString(w, `{"inventories":[{"uuid":"host-a","hypervisorType":"KVM","state":"Enabled","status":"Connected"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/gpu-device/gpu-devices":
			if query := r.URL.Query(); query.Has("q") {
				if len(query) != 1 || len(query["q"]) != 1 || query.Get("q") != "uuid=gpu-a" {
					t.Errorf("unexpected target GPU query: %s", r.URL)
					http.Error(w, "invalid query", http.StatusBadRequest)
					return
				}
				targetGPUQueries.Add(1)
				fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu-a","hostUuid":"host-a","pciDeviceAddress":"0000:01:00.0","state":%q,"status":"Active","allocateStatus":"Unallocated","mode":"PCI","description":%q}]}`, state, description.Load().(string))
				return
			}
			fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu-a","hostUuid":"host-a","pciDeviceAddress":"0000:01:00.0","state":%q,"status":"Active","allocateStatus":"Unallocated","mode":"PCI"},{"uuid":"gpu-b","hostUuid":"host-b","pciDeviceAddress":"0000:02:00.0","state":"Enabled","allocateStatus":"Allocated"}]}`, state)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/pci-device/pci-devices/gpu-a":
			fmt.Fprintf(w, `{"inventories":[{"uuid":"gpu-a","state":%q,"description":%q}]}`, state, description.Load().(string))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/pci-device/pci-devices/gpu-a/actions":
			var action struct {
				Update struct {
					State       string  `json:"state"`
					Description *string `json:"description"`
				} `json:"updatePciDevice"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&action); err != nil {
				t.Errorf("invalid Cloud action: %v", err)
				http.Error(w, "invalid action", http.StatusBadRequest)
				return
			}
			if action.Update.State != "Disabled" && action.Update.State != "Enabled" {
				t.Errorf("unexpected Cloud GPU state %q", action.Update.State)
				http.Error(w, "invalid state", http.StatusBadRequest)
				return
			}
			pendingState.Store(action.Update.State)
			if action.Update.Description != nil {
				pendingDescription.Store(*action.Update.Description)
			}
			if action.Update.State == "Disabled" {
				disables.Add(1)
			} else {
				enables.Add(1)
			}
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"location":"/v1/api-jobs/update-gpu-a"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/api-jobs/update-gpu-a":
			switch pendingState.Load().(string) {
			case "Disabled":
				disabled.Store(true)
			case "Enabled":
				if !removed.Load() {
					t.Error("Cloud GPU released before the agent removed its container")
					http.Error(w, "container still exists", http.StatusConflict)
					return
				}
				disabled.Store(false)
			default:
				t.Error("queried Cloud job before submitting an action")
				http.NotFound(w, r)
				return
			}
			description.Store(pendingDescription.Load().(string))
			io.WriteString(w, `{}`)
		default:
			// The inventory fixture rejects all other writes, including CPU,
			// memory and the GPU already allocated to a Cloud VM.
			inventory.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(cloud.Close)

	driver := testenv.NewFakeDriver("fake")
	var inferenceName atomic.Value
	inferenceName.Store("")
	driver.OnRun(func(spec runtime.WorkloadSpec) (string, error, bool) {
		if spec.Image == imageRef {
			if !disabled.Load() {
				t.Error("inference container started without protecting its Cloud GPU")
				return "", fmt.Errorf("Cloud GPU is not reserved"), true
			}
			inferenceName.Store(spec.Name)
		}
		return "", nil, false
	})
	ctx, cancel := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	var started atomic.Bool
	stateDir := t.TempDir()
	env := newCloudEnv(t, installerFunc(func(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
		if req.Target != "192.0.2.10" || req.Token == "" || req.Password != "ssh-secret" {
			return provision.SSHInstallReport{}, fmt.Errorf("unexpected SSH installation request")
		}
		if started.Swap(true) {
			return provision.SSHInstallReport{}, fmt.Errorf("duplicate agent installation")
		}
		agent := agentapp.New(agentapp.Config{
			ServerURL: req.ServerURL, Token: req.Token, StateDir: stateDir,
			Driver: driver, ForceDriver: "fake", DataLane: true,
			Prober: testenv.NewFakeProber([]proto.GPUInfo{{Index: 0, UUID: "GPU-a", Name: "A100", Vendor: "nvidia", ComputeCapability: "8.0", PCIeBDF: "0000:01:00.0", MemoryTotalBytes: 80 << 30, Healthy: true}}),
			NetworkInventory: func() proto.NetInventory {
				return proto.NetInventory{
					Interfaces: []proto.NetInterface{{Name: "lo", State: "up", Addrs: []string{"127.0.0.1/8"}}},
					Management: &proto.ManagementAddress{Interface: "lo", IP: "127.0.0.1"},
				}
			},
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		go func() {
			defer close(agentDone)
			_ = agent.Run(ctx)
		}()
		return provision.SSHInstallReport{}, nil
	}))
	t.Cleanup(func() {
		cancel()
		if started.Load() {
			select {
			case <-agentDone:
			case <-time.After(10 * time.Second):
				t.Error("Cloud test agent did not stop")
			}
		}
		driver.ResetHooks()
		driver.CloseAll()
	})
	providerPath := addedProvider(t, env, cloud.URL)
	for range 2 {
		requestJSON(t, env, "POST", providerPath+"/sync", `{}`, http.StatusOK)
	}
	var nodes []dto.Node
	if err := apiwire.UnmarshalList(requestJSON(t, env, "GET", "/api/v1/nodes", "", http.StatusOK), &nodes); err != nil || len(nodes) != 0 {
		t.Fatalf("sync must not expose uninstalled compute nodes: %+v, err=%v", nodes, err)
	}
	var gpuInventory dto.GPUListResponse
	if err := json.Unmarshal(requestJSON(t, env, "GET", "/api/v1/gpus", "", http.StatusOK), &gpuInventory); err != nil || len(gpuInventory.Items) != 0 {
		t.Fatalf("sync must not expose uninstalled GPUs: %+v, err=%v", gpuInventory, err)
	}
	var installed struct {
		NodeUUID string `json:"node_uuid"`
	}
	installBody := fmt.Sprintf(`{"external_id":"host-a","password":"ssh-secret","ssh_connection_id":"auto","connect_mode":"agent_dial","server_url":%q}`, env.URL())
	if err := json.Unmarshal(requestJSON(t, env, "POST", providerPath+"/install", installBody, http.StatusOK), &installed); err != nil || installed.NodeUUID == "" {
		t.Fatalf("install must return a compute node: %+v, err=%v", installed, err)
	}
	nodeUUID := installed.NodeUUID
	env.Eventually(t, 30*time.Second, func() bool {
		var node dto.Node
		if err := json.Unmarshal(requestJSON(t, env, "GET", "/api/v1/nodes/"+nodeUUID, "", http.StatusOK), &node); err != nil {
			t.Fatal(err)
		}
		return node.State == "online" && node.GPUCount == 1 && node.MemTotalGiB > 0
	})
	// Allow the GPU list's 20-second cache to expire after agent registration.
	env.Eventually(t, 30*time.Second, func() bool {
		if err := json.Unmarshal(requestJSON(t, env, "GET", "/api/v1/gpus", "", http.StatusOK), &gpuInventory); err != nil {
			t.Fatal(err)
		}
		return len(gpuInventory.Items) == 1 && gpuInventory.Items[0].UUID == "GPU-a" && gpuInventory.Items[0].NodeUUID == nodeUUID
	})
	if disables.Load() != 0 || enables.Load() != 0 {
		t.Fatal("adding, syncing or installing Cloud must not reserve resources")
	}

	// Seed a local model so this lifecycle case never downloads public assets.
	node, err := env.Store().NodeByUUID(nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "cloud-lifecycle", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: stateDir}); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"served_name":"cloud-lifecycle","engine":"vllm","image":%q,"quick_params":{"max_model_len":512}}`, model.ID, nodeUUID, imageRef)
	var deployment dto.Deployment
	if err := json.Unmarshal(requestJSON(t, env, "POST", "/api/v1/deployments", payload, http.StatusCreated), &deployment); err != nil || deployment.ID == 0 {
		t.Fatalf("create deployment = %+v, err=%v", deployment, err)
	}
	path := fmt.Sprintf("/api/v1/deployments/%d", deployment.ID)
	env.Eventually(t, 30*time.Second, func() bool {
		if err := json.Unmarshal(requestJSON(t, env, "GET", path, "", http.StatusOK), &deployment); err != nil {
			t.Fatal(err)
		}
		return len(deployment.Instances) == 1 && deployment.Instances[0].State == proto.InstRunning
	})
	if !disabled.Load() || disables.Load() != 1 || enables.Load() != 0 {
		t.Fatalf("running deployment must reserve its GPU once: disabled=%v disables=%d enables=%d", disabled.Load(), disables.Load(), enables.Load())
	}
	if got := description.Load().(string); got != "原有备注\n被gpuworks-127.0.0.1使用中" {
		t.Fatalf("Cloud GPU ownership description = %q", got)
	}
	description.Store(description.Load().(string) + "\n运行期间补充")
	if targetHostQueries.Load() == 0 || targetGPUQueries.Load() == 0 {
		t.Fatalf("deployment must query its selected Cloud resources: host=%d GPU=%d", targetHostQueries.Load(), targetGPUQueries.Load())
	}
	instanceUUID := deployment.Instances[0].UUID
	cloudStore := cloudstore.New(env.Store().DB())
	leases, err := cloudStore.CloudResourceLeasesForInstance(instanceUUID)
	if err != nil || len(leases) != 1 || leases[0].State != cloudstore.CloudLeaseReserved {
		t.Fatalf("running instance must hold its Cloud GPU lease: %+v, err=%v", leases, err)
	}
	leaseID := leases[0].ID
	requestJSON(t, env, "DELETE", providerPath, "", http.StatusConflict)
	assertCloudProviderPaths(t, env, providerPath)
	containerName := inferenceName.Load().(string)
	if containerName == "" {
		t.Fatal("deployment did not start the requested inference container")
	}
	removing, allowRemoval := make(chan struct{}), make(chan struct{})
	var removalEntered sync.Once
	driver.OnRemove(func(name string) (error, bool) {
		if name != containerName {
			return nil, false
		}
		removalEntered.Do(func() { close(removing) })
		select {
		case <-allowRemoval:
			return nil, false
		case <-ctx.Done():
			return ctx.Err(), true
		}
	})
	driver.AfterRemove(func(name string) {
		if name == containerName {
			removed.Store(true)
		}
	})
	removeErr := errors.New("temporary container removal failure")
	driver.FailNext("Remove", removeErr)
	requestJSON(t, env, "DELETE", path, "", http.StatusAccepted)
	env.Eventually(t, 30*time.Second, func() bool {
		if err := json.Unmarshal(requestJSON(t, env, "GET", path, "", http.StatusOK), &deployment); err != nil {
			t.Fatal(err)
		}
		return len(deployment.Instances) == 1 && deployment.Instances[0].UUID == instanceUUID &&
			deployment.Instances[0].State == proto.InstRemoving &&
			strings.HasPrefix(deployment.Instances[0].Message, "teardown pending: ") &&
			strings.Contains(deployment.Instances[0].Message, removeErr.Error())
	})
	select {
	case <-removing:
	case <-time.After(30 * time.Second):
		t.Fatal("agent did not retry the failed container removal")
	}
	if err := json.Unmarshal(requestJSON(t, env, "GET", path, "", http.StatusOK), &deployment); err != nil || deployment.DesiredState != "deleting" {
		t.Fatalf("deployment must remain until teardown completes: %+v err=%v", deployment, err)
	}
	if !disabled.Load() || enables.Load() != 0 {
		t.Fatal("Cloud GPU released before agent teardown completed")
	}
	container, err := driver.Status(ctx, containerName)
	if err != nil || !container.Exists || removed.Load() {
		t.Fatalf("failed removal must retain the container: %+v removed=%v err=%v", container, removed.Load(), err)
	}
	lease, err := cloudStore.CloudResourceLeaseByID(leaseID)
	if err != nil || lease.State != cloudstore.CloudLeaseReserved {
		t.Fatalf("failed removal must retain the Cloud GPU lease: %+v err=%v", lease, err)
	}
	requestJSON(t, env, "DELETE", providerPath, "", http.StatusConflict)
	close(allowRemoval)
	api := env.API(t)
	env.Eventually(t, 30*time.Second, func() bool {
		resp, err := api.Get(path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("deletion lookup status = %d", resp.StatusCode)
		}
		return resp.StatusCode == http.StatusNotFound
	})
	if disabled.Load() || !removed.Load() || disables.Load() != 1 || enables.Load() != 1 {
		t.Fatalf("deleted deployment did not restore its GPU exactly once: disabled=%v removed=%v disables=%d enables=%d", disabled.Load(), removed.Load(), disables.Load(), enables.Load())
	}
	if got := description.Load().(string); got != "原有备注\n运行期间补充" {
		t.Fatalf("released Cloud GPU description = %q", got)
	}
	containers, err := driver.ListContainers(ctx)
	if err != nil || len(containers) != 0 {
		t.Fatalf("deleted deployment left containers: %v, err=%v", containers, err)
	}
	lease, err = cloudStore.CloudResourceLeaseByID(leaseID)
	if err != nil || lease.State != cloudstore.CloudLeaseReleased {
		t.Fatalf("successful removal must release the Cloud GPU lease: %+v err=%v", lease, err)
	}
	// Releasing the GPU is not enough: the linked compute node must also be
	// removed before deleting the connection configuration it depends on.
	requestJSON(t, env, "DELETE", providerPath, "", http.StatusConflict)
	// The in-process agent fixture has no SSH host to uninstall.
	requestJSON(t, env, "DELETE", "/api/v1/nodes/"+nodeUUID+"?force=true", "", http.StatusOK)
	requestJSON(t, env, "DELETE", providerPath, "", http.StatusOK)
	assertCloudProviderPaths(t, env)
}
