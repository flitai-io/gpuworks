package zstackcloud_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
	"gpuworks.ai/gpuworks/internal/testenv"
)

type gpuDeploymentTest struct {
	env           *testenv.Env
	cloud         *cloudGPUAPI
	driver        *testenv.FakeDriver
	ctx           context.Context
	removed       *atomic.Bool
	inferenceName *atomic.Value
	stateDir      string
}

const gpuDeploymentTestImage = "gpuworks/fake-engine:test"

// Real API, database, controller and agent; Cloud, SSH, GPU probing and runtime
// are fixtures. This does not exercise real GPU inference.
func newGPUDeploymentTest(t *testing.T, options cloudGPUAPIOptions) *gpuDeploymentTest {
	t.Helper()
	var removed atomic.Bool
	cloud := newCloudGPUAPI(t, options, removed.Load)
	driver := testenv.NewFakeDriver("docker")
	var inferenceName atomic.Value
	inferenceName.Store("")
	driver.OnRun(func(spec runtime.WorkloadSpec) (string, error, bool) {
		if spec.Image == gpuDeploymentTestImage {
			if !cloud.state().disabled {
				t.Error("container started before Cloud GPU reservation completed")
				return "", errors.New("Cloud GPU is not reserved"), true
			}
			removed.Store(false)
			inferenceName.Store(spec.Name)
		}
		return "", nil, false
	})
	driver.AfterRemove(func(name string) {
		if name == inferenceName.Load().(string) {
			removed.Store(true)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	var started atomic.Bool
	stateDir := t.TempDir()
	env := newCloudEnv(t, installerFunc(func(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
		if req.Target != "192.0.2.10" || req.Token == "" || req.Password != "ssh-secret" || started.Swap(true) {
			return provision.SSHInstallReport{}, errors.New("unexpected or repeated agent installation")
		}
		agent := agentapp.New(agentapp.Config{
			ServerURL: req.ServerURL, Token: req.Token, StateDir: stateDir,
			Driver: driver, ForceDriver: "docker", DataLane: true,
			Prober: testenv.NewFakeProber([]proto.GPUInfo{
				{Index: 0, UUID: "GPU-a", Name: "A100", Vendor: "nvidia", ComputeCapability: "8.0", PCIeBDF: "0000:01:00.0", MemoryTotalBytes: 80 << 30, Healthy: true},
				{Index: 1, UUID: "GPU-used", Name: "A100", Vendor: "nvidia", ComputeCapability: "8.0", PCIeBDF: "0000:02:00.0", MemoryTotalBytes: 80 << 30, Healthy: true},
			}),
			NetworkInventory: func() proto.NetInventory {
				return proto.NetInventory{
					Interfaces: []proto.NetInterface{{Name: "lo", State: "up", Addrs: []string{"127.0.0.1/8"}}},
					Management: &proto.ManagementAddress{Interface: "lo", IP: "127.0.0.1"},
				}
			},
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		go func() { agentDone <- agent.Run(ctx) }()
		return provision.SSHInstallReport{}, nil
	}))
	t.Cleanup(func() {
		cancel()
		if started.Load() {
			select {
			case err := <-agentDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("Cloud test agent failed: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Cloud test agent did not stop")
			}
		}
		driver.ResetHooks()
		driver.CloseAll()
	})
	return &gpuDeploymentTest{
		env: env, cloud: cloud, driver: driver, ctx: ctx,
		removed: &removed, inferenceName: &inferenceName, stateDir: stateDir,
	}
}

func (s *gpuDeploymentTest) assertInstalledHost(t *testing.T, nodeUUID string, wantCPU int64) {
	t.Helper()
	ledger := cloudstore.New(s.env.Store().DB())
	host, err := ledger.CloudHostByNodeUUID(nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot inventory.CloudResource
	if err := json.Unmarshal([]byte(host.Snapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if host.InitialCPUReserved == nil || *host.InitialCPUReserved != wantCPU || snapshot.CPU.Reserved == nil || *snapshot.CPU.Reserved != wantCPU || len(snapshot.GPUs) != 2 {
		t.Fatalf("incorrect installed Cloud snapshot: host=%+v snapshot=%+v", host, snapshot)
	}
	allocations := map[string]string{}
	for _, gpu := range snapshot.GPUs {
		allocations[gpu.ExternalID] = gpu.Allocation
	}
	if allocations["gpu-a"] != "Unallocated" || allocations["gpu-used"] != "Allocated" {
		t.Fatalf("installation lost Cloud GPU ownership: %v", allocations)
	}
	var gpus dto.GPUListResponse
	s.env.Eventually(t, 30*time.Second, func() bool {
		if err := json.Unmarshal(requestJSON(t, s.env, "GET", "/api/v1/gpus", "", http.StatusOK), &gpus); err != nil {
			t.Fatal(err)
		}
		return len(gpus.Items) == 2
	})
	for _, gpu := range gpus.Items {
		if gpu.NodeUUID != nodeUUID || (gpu.UUID != "GPU-a" && gpu.UUID != "GPU-used") {
			t.Fatalf("incorrect installed GPU identity: %+v", gpu)
		}
	}
	if state := s.cloud.state(); state.disables != 0 || state.enables != 0 || state.disabled || state.description != "原有备注" {
		t.Fatalf("onboarding reserved or changed Cloud resources: %+v", state)
	}
}

func (s *gpuDeploymentTest) createDeployment(t *testing.T, nodeUUID string) string {
	t.Helper()
	// Seed only model files; all tested lifecycle transitions use management APIs.
	node, err := s.env.Store().NodeByUUID(nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "cloud-protocol", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: s.stateDir}); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"served_name":"cloud-protocol","engine":"vllm","image":%q,"placement":{"tp":1,"gpus":[0]},"quick_params":{"max_model_len":512}}`, model.ID, nodeUUID, gpuDeploymentTestImage)
	var deployment dto.Deployment
	if err := json.Unmarshal(requestJSON(t, s.env, "POST", "/api/v1/deployments", payload, http.StatusCreated), &deployment); err != nil || deployment.ID == 0 {
		t.Fatalf("create deployment: %+v, %v", deployment, err)
	}
	return fmt.Sprintf("/api/v1/deployments/%d", deployment.ID)
}

func (s *gpuDeploymentTest) readDeployment(t *testing.T, path string) dto.Deployment {
	t.Helper()
	var current dto.Deployment
	if err := json.Unmarshal(requestJSON(t, s.env, "GET", path, "", http.StatusOK), &current); err != nil {
		t.Fatal(err)
	}
	return current
}

func (s *gpuDeploymentTest) waitRunning(t *testing.T, path, previous string) string {
	t.Helper()
	var current dto.Deployment
	s.env.Eventually(t, 30*time.Second, func() bool {
		current = s.readDeployment(t, path)
		return len(current.Instances) == 1 && current.Instances[0].State == proto.InstRunning && current.Instances[0].UUID != previous
	})
	return current.Instances[0].UUID
}

func (s *gpuDeploymentTest) assertReserved(t *testing.T, instance string, reservations int, description string) uint {
	t.Helper()
	ledger := cloudstore.New(s.env.Store().DB())
	state := s.cloud.state()
	leases, err := ledger.CloudResourceLeasesForInstance(instance)
	if err != nil || len(leases) != 1 || leases[0].State != cloudstore.CloudLeaseReserved || leases[0].ResourceID != "gpu-a" || !state.disabled || state.disables != reservations || state.enables != reservations-1 || state.description != description {
		t.Fatalf("GPU reservation: state=%+v leases=%+v err=%v", state, leases, err)
	}
	return leases[0].ID
}

func (s *gpuDeploymentTest) waitReleased(t *testing.T, path string, leaseID uint, releases int, description string) {
	t.Helper()
	ledger := cloudstore.New(s.env.Store().DB())
	s.env.Eventually(t, 30*time.Second, func() bool {
		lease, err := ledger.CloudResourceLeaseByID(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		return s.readDeployment(t, path).State == "stopped" && lease.State == cloudstore.CloudLeaseReleased && !s.cloud.state().disabled
	})
	state := s.cloud.state()
	containers, err := s.driver.ListContainers(s.ctx)
	if err != nil || len(containers) != 0 || !s.removed.Load() || state.enables != releases || state.disables != releases || state.description != description {
		t.Fatalf("GPU release: state=%+v containers=%v removed=%v err=%v", state, containers, s.removed.Load(), err)
	}
}

func (s *gpuDeploymentTest) blockContainerRemoval(t *testing.T) (waitForRetry, allowRemoval func()) {
	t.Helper()
	containerName := s.inferenceName.Load().(string)
	removing, resumeRemoval := make(chan struct{}), make(chan struct{})
	var removalEntered sync.Once
	s.driver.OnRemove(func(name string) (error, bool) {
		if name != containerName {
			return nil, false
		}
		removalEntered.Do(func() { close(removing) })
		select {
		case <-resumeRemoval:
			return nil, false
		case <-s.ctx.Done():
			return s.ctx.Err(), true
		}
	})
	s.driver.FailNext("Remove", errors.New("temporary container removal failure"))
	return func() {
		t.Helper()
		select {
		case <-removing:
		case <-time.After(30 * time.Second):
			t.Fatal("agent did not retry container removal")
		}
		container, err := s.driver.Status(s.ctx, containerName)
		if err != nil || !container.Exists || s.removed.Load() {
			t.Fatalf("blocked cleanup lost its container: %+v, %v", container, err)
		}
	}, func() { close(resumeRemoval) }
}
