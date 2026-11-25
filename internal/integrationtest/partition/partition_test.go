package partition

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

func TestDomesticPartitionBackendsCrossAPIControllerAgentAndRuntime(t *testing.T) {
	tests := []struct {
		name, vendor, framework, version, model, archID, backend string
		// runtimeDriver is the name the node's agent reports for its runtime.
		// It is a node fact the capability rules read, so each case names the
		// runtime its backend is actually verified on rather than letting a
		// later status report disagree with the test.
		runtimeDriver string
		prepare       func(*testing.T)
		assert        func(*testing.T, runtime.WorkloadSpec)
	}{
		{
			name: "ascend", vendor: "ascend", framework: "cann", version: "9.0.0", model: "Ascend 910B4", archID: "910B4", backend: accelerator.BackendAscendLogical,
			assert: func(t *testing.T, spec runtime.WorkloadSpec) {
				assertMount(t, spec, "/opt/gpuworks/partition/libvruntime.so")
				assertMount(t, spec, "/etc/ld.so.preload")
				assertMount(t, spec, "/etc/enpu/vcann-rt/npu_info.config")
				if spec.Env["ENPU_DSMI_HOOK"] != "1" || spec.Env["ASCEND_VISIBLE_DEVICES"] != "0" || !strings.Contains(spec.Env["LD_LIBRARY_PATH"], "/usr/local/Ascend/driver/lib64/driver") {
					t.Fatalf("Ascend environment = %+v", spec.Env)
				}
			},
		},
		{
			// PPU derives a ready capability on Docker only
			// (accelerator.ppuLogicalBackend.RuntimeConstraint), so the node
			// reports docker; the driver itself stays the in-process fake.
			name: "ppu", vendor: "ppu", framework: "hggc", version: "13.0", model: "PPU-ZW810E", archID: "Beethoven", backend: accelerator.BackendPPULogical, runtimeDriver: "docker",
			prepare: func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "ppu", "linux-arm64", "hggc-13.0")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"hggc_quota.so", "hgml_dlsym_hook.so"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("test-runtime-asset"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("GPUWORKS_PARTITION_ASSETS", root)
			},
			assert: func(t *testing.T, spec runtime.WorkloadSpec) {
				assertMount(t, spec, "/opt/gpuworks/partition/hggc_quota.so")
				assertMount(t, spec, "/opt/gpuworks/partition/hgml_dlsym_hook.so")
				if spec.Env["LD_PRELOAD"] != "/opt/gpuworks/partition/hgml_dlsym_hook.so:/opt/gpuworks/partition/hggc_quota.so" || spec.Env["HGGC_DEVICE_MEMORY_LIMIT_0"] != "8192" || spec.Env["NVIDIA_VISIBLE_DEVICES"] != "void" {
					t.Fatalf("PPU environment = %+v", spec.Env)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepare != nil {
				tt.prepare(t)
			}
			env := testenv.New(t)
			runtimeDriver := tt.runtimeDriver
			if runtimeDriver == "" {
				runtimeDriver = "docker"
			}
			node := env.AddNode("partition-"+tt.name,
				testenv.WithAccelerator(tt.vendor, tt.framework, tt.version),
				testenv.WithGPUs(1, testenv.GPUSpec{Model: tt.model, VRAMMB: 32768, Vendor: tt.vendor, ArchID: tt.archID}),
				testenv.WithDriverName(runtimeDriver),
			)
			env.WaitNodeOnline(t, "partition-"+tt.name)
			waitForAssetReport(t, env, node.UUID, tt.backend)
			if tt.vendor == "ppu" {
				// The L1 process runs on the developer's ARM64 workstation. Keep
				// the wire-observed PPU assets, but model the backend's declared
				// x86_64 server profile before deriving capability.
				reportPPUX8664Status(t, env, node.UUID, tt.model, tt.archID)
			}
			waitForProbeReport(t, env, node.UUID, tt.backend, proto.PartitionProbePassed)
			assertNodeAPICapability(t, env, node.UUID, tt.backend, partitioncap.StatusReady)

			row, err := env.Store().NodeWithGPUs(node.UUID)
			if err != nil {
				t.Fatal(err)
			}
			model := registerPresentModel(t, env, row, tt.name)
			runSeen := make(chan runtime.WorkloadSpec, 1)
			node.Driver.AfterRun(func(spec runtime.WorkloadSpec) {
				select {
				case runSeen <- spec:
				default:
				}
			})
			body, _ := json.Marshal(map[string]any{
				"name": "partition-" + tt.name, "served_name": "partition-" + tt.name,
				"model_id": model.ID, "node_uuid": node.UUID, "image": "validated/" + tt.name + ":test",
				"partition_backend": tt.backend, "vram_limit_gib": 8,
				"quick_params": map[string]any{"max_model_len": 256},
			})
			resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("create status=%d body=%s", resp.StatusCode, raw)
			}
			var deployment dto.Deployment
			if err := json.NewDecoder(resp.Body).Decode(&deployment); err != nil {
				t.Fatal(err)
			}
			if deployment.PartitionBackend != tt.backend {
				t.Fatalf("deployment backend=%q want %q", deployment.PartitionBackend, tt.backend)
			}
			var spec runtime.WorkloadSpec
			select {
			case spec = <-runSeen:
			case <-time.After(30 * time.Second):
				t.Fatal("agent did not deliver partitioned workload to runtime")
			}
			tt.assert(t, spec)
			if tt.vendor == "ascend" && len(node.Prober.ShareCalls()) < 2 {
				t.Fatalf("device-share calls = %v, want probe and deployment", node.Prober.ShareCalls())
			}
			if tt.vendor == "ascend" {
				env.Eventually(t, 10*time.Second, func() bool {
					instances, err := env.Store().InstancesForDeployment(deployment.ID)
					if err != nil || len(instances) != 1 {
						return false
					}
					events, err := env.Store().InstanceEvents(instances[0].ID, 0)
					if err != nil {
						return false
					}
					for _, event := range events {
						if strings.Contains(event.Message, "device-share enabled") || strings.Contains(event.Data, "device-share enabled") {
							return true
						}
					}
					return false
				})
			}
		})
	}
}

func TestDomesticAutomaticImageMismatchRemainsAdvisory(t *testing.T) {
	env := testenv.New(t)
	// The embedded catalog contains CANN 8.x and 9.x Ascend images. Model a
	// newer host major so the fixture exercises the advisory compatibility path.
	node := env.AddNode("blocked-image", testenv.WithAccelerator("ascend", "cann", "10.0.99"), testenv.WithGPUs(1, testenv.GPUSpec{Model: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4"}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "blocked-image")
	waitForAssetReport(t, env, node.UUID, accelerator.BackendAscendLogical)
	row, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	model := registerPresentModel(t, env, row, "blocked")
	body, _ := json.Marshal(map[string]any{"name": "blocked", "served_name": "blocked", "model_id": model.ID, "node_uuid": node.UUID})
	resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(raw), "gpuworks/gpuworks-runner:cann") {
		t.Fatalf("advisory automatic image status=%d body=%s", resp.StatusCode, raw)
	}
	rows, err := env.Store().ListDeployments()
	if err != nil || len(rows) != 1 {
		t.Fatalf("advisory request persistence=%+v err=%v", rows, err)
	}
}

func TestSparsePhysicalGPUIndexSurvivesProbeStoreSchedulerAndAscendInjection(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("sparse-ascend",
		testenv.WithAccelerator("ascend", "cann", ""),
		testenv.WithGPUs(5, testenv.GPUSpec{Model: "Ascend 910B4", VRAMMB: 32768, Vendor: "ascend", ArchID: "910B4"}),
		testenv.WithGPUIndexes(2, 4, 5, 6, 7),
		testenv.WithDriverName("docker"),
	)
	env.WaitNodeOnline(t, "sparse-ascend")
	waitForProbeReport(t, env, node.UUID, accelerator.BackendAscendLogical, proto.PartitionProbePassed)
	row, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	wantIndexes := map[int]bool{2: true, 4: true, 5: true, 6: true, 7: true}
	for _, gpu := range row.GPUs {
		delete(wantIndexes, gpu.Index)
	}
	if len(row.GPUs) != 5 || len(wantIndexes) != 0 {
		t.Fatalf("stored GPU indexes = %+v, missing=%v", row.GPUs, wantIndexes)
	}
	model := registerPresentModel(t, env, row, "sparse")
	runSeen := make(chan runtime.WorkloadSpec, 1)
	node.Driver.AfterRun(func(spec runtime.WorkloadSpec) {
		select {
		case runSeen <- spec:
		default:
		}
	})
	body, _ := json.Marshal(map[string]any{
		"name": "sparse-ascend", "served_name": "sparse-ascend",
		"model_id": model.ID, "node_uuid": node.UUID, "image": "validated/ascend:test",
		"partition_backend": accelerator.BackendAscendLogical, "vram_limit_gib": 8,
		"placement":    map[string]any{"tp": 1, "gpus": []int{4}},
		"quick_params": map[string]any{"max_model_len": 256},
	})
	resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create sparse deployment status=%d body=%s", resp.StatusCode, raw)
	}
	var workload runtime.WorkloadSpec
	select {
	case workload = <-runSeen:
	case <-time.After(30 * time.Second):
		t.Fatal("agent did not deliver sparse-index workload")
	}
	if len(workload.GPUIndexes) != 1 || workload.GPUIndexes[0] != 4 || workload.Env["ASCEND_VISIBLE_DEVICES"] != "4" {
		t.Fatalf("runtime GPU identity = indexes=%v env=%v", workload.GPUIndexes, workload.Env)
	}
	configPath := ""
	for _, mount := range workload.Mounts {
		if mount.Target == "/etc/enpu/vcann-rt/npu_info.config" {
			configPath = mount.Source
			break
		}
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read injected sparse config %q: %v", configPath, err)
	}
	if !strings.Contains(string(config), "physical-npu-id=4\n") {
		t.Fatalf("sparse config = %q", config)
	}
	instances, err := env.Store().ListInstances()
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	var persistedIndexes []int
	var desired proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(instances[0].GPUIndexes), &persistedIndexes); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &desired); err != nil {
		t.Fatal(err)
	}
	if len(persistedIndexes) != 1 || persistedIndexes[0] != 4 || len(desired.GPUIndexes) != 1 || desired.GPUIndexes[0] != 4 {
		t.Fatalf("persisted scheduler identity = row=%v spec=%v", persistedIndexes, desired.GPUIndexes)
	}
}

// A node whose dGPU capability reads ready, with a card that cannot serve.
// The two are independent: the backend being installed and configured says
// nothing about the health of the card it would partition.
//
// The card says so at the level that means it (accelerator.HealthError with
// the finding behind it). It used to say Warning with the Healthy boolean
// set false beside it, which is the disagreement the card vocabulary no
// longer has: a warning serves, and the agent fills that boolean from the
// level (TestAWarningGPUStillTakesWork below holds the other half).
func TestUnusableGPUReportsReadyCapabilityButIsNotScheduled(t *testing.T) {
	bad := false
	env := testenv.New(t)
	node := env.AddNode("warning-gpu", testenv.WithAccelerator("ascend", "cann", "9.0.0"), testenv.WithGPUs(1, testenv.GPUSpec{Model: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", Healthy: &bad}), testenv.WithDriverName("docker"))
	node.Prober.SetGPUs([]proto.GPUInfo{{Index: 0, UUID: "warning-0", Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: false, HealthStatus: accelerator.HealthError, HealthCode: accelerator.HealthMemoryUncorrectable}})
	env.WaitNodeOnline(t, "warning-gpu")
	waitForAssetReport(t, env, node.UUID, accelerator.BackendAscendLogical)
	assertNodeAPICapability(t, env, node.UUID, accelerator.BackendAscendLogical, partitioncap.StatusReady)
	runSeen := make(chan struct{}, 1)
	node.Driver.AfterRun(func(runtime.WorkloadSpec) {
		select {
		case runSeen <- struct{}{}:
		default:
		}
	})
	row, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	model := registerPresentModel(t, env, row, "warning")
	body, _ := json.Marshal(map[string]any{"name": "warning", "served_name": "warning", "model_id": model.ID, "node_uuid": node.UUID, "image": "validated/ascend:test", "partition_backend": accelerator.BackendAscendLogical, "vram_limit_gib": 8})
	resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("warning GPU deployment should be admitted for asynchronous scheduling status=%d body=%s", resp.StatusCode, raw)
	}
	select {
	case <-runSeen:
		t.Fatal("a card at Error was delivered to the runtime")
	case <-time.After(750 * time.Millisecond):
	}
	instances, err := env.Store().ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 0 {
		t.Fatalf("a card at Error was scheduled: %+v", instances)
	}
}

// The other half: a warning is not a refusal. A card the vendor tool
// flagged - rows pending repair, say - still serves, and is simply used
// after the cards at OK. Twelve PPU cards on 192.0.2.210 are in this
// state (seven at Warning), and a platform that would not place on them
// would leave most of that host idle over a finding its own vocabulary
// calls schedulable.
func TestAWarningGPUStillTakesWork(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("warning-serves", testenv.WithAccelerator("ascend", "cann", "9.0.0"), testenv.WithGPUs(1, testenv.GPUSpec{Model: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4"}), testenv.WithDriverName("docker"))
	node.Prober.SetGPUs([]proto.GPUInfo{{Index: 0, UUID: "warning-serves-0", Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: true, HealthStatus: accelerator.HealthWarning, HealthCode: accelerator.HealthMemoryRepairPending}})
	env.WaitNodeOnline(t, "warning-serves")
	waitForAssetReport(t, env, node.UUID, accelerator.BackendAscendLogical)
	runSeen := make(chan struct{}, 1)
	node.Driver.AfterRun(func(runtime.WorkloadSpec) {
		select {
		case runSeen <- struct{}{}:
		default:
		}
	})
	row, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	// The finding is kept and shown; it is the level that decides.
	if len(row.GPUs) != 1 || row.GPUs[0].HealthStatus != accelerator.HealthWarning || row.GPUs[0].HealthCode != accelerator.HealthMemoryRepairPending {
		t.Fatalf("stored card = %+v, want the warning and its finding kept", row.GPUs)
	}
	if !row.GPUs[0].CanTakeWork() {
		t.Fatal("a card at Warning was refused work")
	}
	model := registerPresentModel(t, env, row, "warnserves")
	body, _ := json.Marshal(map[string]any{"name": "warnserves", "served_name": "warnserves", "model_id": model.ID, "node_uuid": node.UUID, "image": "validated/ascend:test", "partition_backend": accelerator.BackendAscendLogical, "vram_limit_gib": 8})
	resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("deployment on a warning card status=%d body=%s", resp.StatusCode, raw)
	}
	select {
	case <-runSeen:
	case <-time.After(15 * time.Second):
		t.Fatal("a card at Warning never reached the runtime")
	}
}

func registerPresentModel(t *testing.T, env *testenv.Env, node *store.Node, suffix string) *store.Model {
	t.Helper()
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "partition/" + suffix, Revision: "main", Name: "partition-" + suffix, Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "model"+suffix)}); err != nil {
		t.Fatal(err)
	}
	return model
}

func waitForAssetReport(t *testing.T, env *testenv.Env, uuid, backend string) {
	t.Helper()
	env.Eventually(t, 15*time.Second, func() bool {
		node, err := env.Store().NodeWithGPUs(uuid)
		return err == nil && strings.Contains(node.PartitionAssets, backend)
	})
}

func waitForProbeReport(t *testing.T, env *testenv.Env, uuid, backend, status string) {
	t.Helper()
	env.Eventually(t, 20*time.Second, func() bool {
		node, err := env.Store().NodeWithGPUs(uuid)
		if err != nil {
			return false
		}
		var reports []proto.PartitionProbeResult
		if json.Unmarshal([]byte(node.PartitionProbes), &reports) != nil {
			return false
		}
		for _, report := range reports {
			if report.Backend == backend && report.Status == status && !report.CheckedAt.IsZero() {
				return true
			}
		}
		return false
	})
}

func TestOldAgentWithoutPartitionProbesFallsBackToUpgradeReason(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("probe-old-agent", testenv.WithAccelerator("ascend", "cann", ""), testenv.WithGPUs(1, testenv.GPUSpec{Model: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4"}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "probe-old-agent")
	waitForProbeReport(t, env, node.UUID, accelerator.BackendAscendLogical, proto.PartitionProbePassed)
	// The in-process agent is a current one and keeps reporting probes: on
	// connect, after its first state sync, on catalog changes. The hub persists
	// those reports from a queue, so a report taken off it can still commit
	// after the old-agent status below and restore the probes. Stopping the
	// agent ends new reports and drops queued ones; the old-agent status is
	// then rewritten until it is what the node row holds, at most once more
	// for a report that was already being applied.
	env.StopNode(t, "probe-old-agent")
	row, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	var assets []proto.AssetReport
	if err := json.Unmarshal([]byte(row.PartitionAssets), &assets); err != nil {
		t.Fatal(err)
	}
	ok := true
	oldAgent := proto.NodeStatus{Sys: proto.SysInfo{Hostname: row.Hostname, Arch: row.Arch, GPUVendor: "ascend", RuntimeDriver: "docker", Runtimes: []string{"docker"}, PartitionAssets: assets, PartitionProbes: nil}, GPUs: []proto.GPUInfo{{Index: 0, UUID: "old-0", Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: true}}, GPUProbeOK: &ok}
	var updated *store.Node
	env.Eventually(t, 10*time.Second, func() bool {
		if err := env.Store().UpdateNodeStatus(node.UUID, oldAgent); err != nil {
			t.Fatal(err)
		}
		current, err := env.Store().NodeWithGPUs(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		// An old agent's nil probes persist as null; anything else is a
		// report of the stopped agent that committed after this write.
		if strings.TrimSpace(current.PartitionProbes) != "null" || len(current.GPUs) != 1 || current.GPUs[0].UUID != "old-0" {
			return false
		}
		updated = current
		return true
	})
	capabilities := partitioncap.ForGPU(updated, &updated.GPUs[0])
	if len(capabilities) != 1 || capabilities[0].Status != partitioncap.StatusUnavailable || capabilities[0].ReasonCode != messages.ErrorPartitionAgentUpgradeRequired {
		t.Fatalf("old-agent capabilities = %+v", capabilities)
	}
}

// assertNodeAPICapability waits for the API to derive status for backend. The
// capability is derived from node facts the agent keeps reporting, so a single
// sample can read a snapshot taken before the facts this test set up have all
// landed; on timeout it reports the capabilities it last saw.
func assertNodeAPICapability(t *testing.T, env *testenv.Env, uuid, backend, status string) {
	t.Helper()
	var last []proto.PartitionCapability
	var missing bool
	ok := env.EventuallyNoFail(20*time.Second, func() bool {
		resp, err := env.API(t).Get("/api/v1/nodes")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var nodes []dto.Node
		if err := apiwire.ReadList(resp.Body, &nodes); err != nil {
			t.Fatalf("decode /api/v1/nodes: %v", err)
		}
		missing = true
		for _, node := range nodes {
			if node.UUID != uuid || len(node.GPUs) == 0 {
				continue
			}
			missing = false
			last = node.GPUs[0].PartitionCapabilities
			for _, capability := range last {
				if capability.Backend == backend && capability.Status == status {
					return true
				}
			}
		}
		return false
	})
	if ok {
		return
	}
	if missing {
		t.Fatalf("node %s missing from API", uuid)
	}
	t.Fatalf("GPU capabilities=%+v, want %s %s", last, backend, status)
}

func reportPPUX8664Status(t *testing.T, env *testenv.Env, uuid, model, archID string) {
	t.Helper()
	node, err := env.Store().NodeWithGPUs(uuid)
	if err != nil {
		t.Fatal(err)
	}
	var reports []proto.AssetReport
	if err := json.Unmarshal([]byte(node.PartitionAssets), &reports); err != nil {
		t.Fatal(err)
	}
	probeOK := true
	// The cards keep the identity the node's own prober reports: an instance
	// is bound to its cards by that identity, so inventing one here would
	// describe a node whose agent cannot find the cards it is told to use.
	cards := make([]proto.GPUInfo, 0, len(node.GPUs))
	for _, g := range node.GPUs {
		cards = append(cards, proto.GPUInfo{
			Index: g.Index, UUID: g.UUID, PCIeBDF: g.PCIeBDF, Name: model, Vendor: "ppu",
			ArchID: archID, MemoryTotalBytes: 32 << 30, Healthy: true,
		})
	}
	// Only the architecture is modelled here: the PPU profile declares x86_64
	// and an ARM64 workstation reports its own. Every other fact comes from the
	// node's own reports, the runtime name included — writing one here would be
	// undone by the next status report, which is what made this case flaky
	// (todo T29).
	if err := env.Store().UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: node.Hostname, OS: node.OS, Arch: "x86_64", CPUCores: node.CPUCores, MemTotalKB: node.MemTotalKB, DiskTotalKB: node.DiskTotalKB, GPUVendor: "ppu", Framework: "hggc", FrameworkVersion: "13.0", RuntimeDriver: node.RuntimeDriver, Runtimes: []string{node.RuntimeDriver}, PartitionAssets: reports, PartitionProbes: []proto.PartitionProbeResult{{Backend: accelerator.BackendPPULogical, GPUIndex: new(int), Status: proto.PartitionProbePassed, CheckedAt: time.Now()}}}, GPUs: cards, GPUProbeOK: &probeOK}); err != nil {
		t.Fatal(err)
	}
}

func assertMount(t *testing.T, spec runtime.WorkloadSpec, target string) {
	t.Helper()
	for _, mount := range spec.Mounts {
		if mount.Target == target {
			return
		}
	}
	t.Fatalf("mount %s missing from %+v", target, spec.Mounts)
}
