package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestDistributedDeploymentRunsThroughSchedulerAndTwoFakeAgents is the fast
// product-level contract for real nnodes=2 orchestration. It deliberately uses
// the real REST API, scheduler, controller, hub, two agent loops and engine
// assembly; only GPU probing and the final container runtime are fake.
func TestDistributedDeploymentRunsThroughSchedulerAndTwoFakeAgents(t *testing.T) {
	env := testenv.New(t)
	const (
		leaderAddress   = "10.42.0.10"
		followerAddress = "10.42.0.11"
		masterPort      = 29500
		servedName      = "fake-distributed-nnode2"
	)

	leader := env.AddNode("distributed-rank-0",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("docker"), testenv.WithManagementAddress(leaderAddress))
	follower := env.AddNode("distributed-rank-1",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("docker"), testenv.WithManagementAddress(followerAddress))

	leaderRuns := make(chan runtime.WorkloadSpec, 1)
	followerRuns := make(chan runtime.WorkloadSpec, 1)
	leader.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(leaderRuns, spec) })
	follower.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(followerRuns, spec) })
	env.WaitNodeOnline(t, leader.Name)
	env.WaitNodeOnline(t, follower.Name)

	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main",
		Name: "qwen3-4b-fake", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for rank, node := range []*testenv.Node{leader, follower} {
		row, getErr := env.Store().NodeByUUID(node.UUID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if _, upsertErr := env.Store().UpsertModelFile(&store.ModelFile{
			ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent,
			Path: testenv.ModelDir(t, fmt.Sprintf("fake-rank-%d", rank)),
		}); upsertErr != nil {
			t.Fatal(upsertErr)
		}
	}

	api := env.API(t)
	payload, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": leader.UUID,
		"node_uuids":  []string{leader.UUID, follower.UUID},
		"served_name": servedName, "engine": "vllm", "image": "gpuworks/fake-engine:test",
		"nnodes": 2, "master_addr": leaderAddress, "master_port": masterPort,
		"placement":    map[string]any{"tp": 1, "pp": 2},
		"quick_params": map[string]any{"max_model_len": 512},
	})
	resp, err := api.PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create distributed deployment status=%d body=%s", resp.StatusCode, raw)
	}
	var created dto.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.ID == 0 {
		t.Fatal("create distributed deployment returned no id")
	}

	leaderSpec := waitCapturedWorkload(t, leaderRuns, "leader")
	followerSpec := waitCapturedWorkload(t, followerRuns, "follower")
	deployment := waitDistributedDeploymentRunning(t, api, created.ID)
	assertDistributedDeploymentDTO(t, deployment, leader.UUID, follower.UUID, leaderAddress, followerAddress, masterPort)
	assertDistributedWorkload(t, deployment.Instances[0], leaderSpec, leaderAddress, masterPort)
	assertDistributedWorkload(t, deployment.Instances[1], followerSpec, followerAddress, masterPort)
	peers := []proto.PeerRank{
		{NodeRank: 0, NodeName: leader.Name, NodeUUID: leader.UUID, NodeAddr: leaderAddress},
		{NodeRank: 1, NodeName: follower.Name, NodeUUID: follower.UUID, NodeAddr: followerAddress},
	}
	assertPeersFileMounted(t, leaderSpec, 0, leaderAddress, masterPort, peers)
	assertPeersFileMounted(t, followerSpec, 1, leaderAddress, masterPort, peers)

	key, err := env.Store().CreateAPIKey("distributed-fake", 0)
	if err != nil {
		t.Fatal(err)
	}
	modelsReq, _ := http.NewRequest(http.MethodGet, env.URL()+"/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer "+key)
	modelsResp, err := http.DefaultClient.Do(modelsReq)
	if err != nil {
		t.Fatal(err)
	}
	modelsBody, _ := io.ReadAll(modelsResp.Body)
	modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK || !bytes.Contains(modelsBody, []byte(`"id":"`+servedName+`"`)) {
		t.Fatalf("distributed models status=%d body=%s", modelsResp.StatusCode, modelsBody)
	}

	chatReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"`+servedName+`","messages":[{"role":"user","content":"hi"}]}`)))
	chatReq.Header.Set("Authorization", "Bearer "+key)
	chatReq.Header.Set("Content-Type", "application/json")
	chatResp, err := http.DefaultClient.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	chatBody, _ := io.ReadAll(chatResp.Body)
	chatResp.Body.Close()
	if chatResp.StatusCode != http.StatusOK || !bytes.Contains(chatBody, []byte("hello from fake-engine")) {
		t.Fatalf("distributed chat status=%d body=%s", chatResp.StatusCode, chatBody)
	}
}

func captureWorkload(target chan<- runtime.WorkloadSpec, spec runtime.WorkloadSpec) {
	select {
	case target <- spec:
	default:
	}
}

func waitCapturedWorkload(t *testing.T, source <-chan runtime.WorkloadSpec, rank string) runtime.WorkloadSpec {
	t.Helper()
	select {
	case spec := <-source:
		return spec
	case <-time.After(30 * time.Second):
		t.Fatalf("%s fake agent did not receive a workload", rank)
		return runtime.WorkloadSpec{}
	}
}

func waitDistributedDeploymentRunning(t *testing.T, api *testenv.APIClient, id uint) dto.Deployment {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last dto.Deployment
	for time.Now().Before(deadline) {
		resp, err := api.Get(fmt.Sprintf("/api/v1/deployments/%d", id))
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				// Decode each poll into a fresh value. Zero-valued rank 0 fields use
				// omitempty in the wire DTO; reusing a slice could otherwise retain a
				// rank value decoded during an earlier, differently ordered response.
				last = dto.Deployment{}
				_ = json.NewDecoder(resp.Body).Decode(&last)
			}
			resp.Body.Close()
		}
		if len(last.Instances) == 2 {
			sort.Slice(last.Instances, func(i, j int) bool { return last.Instances[i].NodeRank < last.Instances[j].NodeRank })
			if last.RuntimeState == proto.InstRunning && last.Instances[0].State == proto.InstRunning && last.Instances[1].State == proto.InstRunning {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("distributed deployment did not reach two running ranks: %+v", last)
	return dto.Deployment{}
}

func assertDistributedDeploymentDTO(t *testing.T, deployment dto.Deployment, leaderUUID, followerUUID, leaderAddress, followerAddress string, masterPort int) {
	t.Helper()
	if deployment.NNodes != 2 || deployment.Replicas != 1 || deployment.MasterAddr != leaderAddress || deployment.MasterPort != masterPort {
		t.Fatalf("distributed deployment topology=%+v", deployment)
	}
	if !reflect.DeepEqual(deployment.NodeUUIDs, []string{leaderUUID, followerUUID}) {
		t.Fatalf("distributed node UUIDs=%v", deployment.NodeUUIDs)
	}
	if len(deployment.Instances) != 2 {
		t.Fatalf("distributed instances=%+v", deployment.Instances)
	}
	leader, follower := deployment.Instances[0], deployment.Instances[1]
	if leader.NodeUUID != leaderUUID || leader.NodeRank != 0 || leader.DistRole != "leader" || leader.Headless || leader.APIPort == 0 {
		t.Fatalf("leader DTO=%+v", leader)
	}
	if follower.NodeUUID != followerUUID || follower.NodeRank != 1 || follower.DistRole != "follower" || !follower.Headless || follower.APIPort != 0 {
		t.Fatalf("follower DTO=%+v", follower)
	}
	if leader.DistGroup == "" || leader.DistGroup != follower.DistGroup || leader.DistAttempt <= 0 || leader.DistAttempt != follower.DistAttempt {
		t.Fatalf("distributed generation leader=%+v follower=%+v", leader, follower)
	}
	if leader.Env["VLLM_HOST_IP"] != leaderAddress || follower.Env["VLLM_HOST_IP"] != followerAddress {
		t.Fatalf("rank host IPs leader=%v follower=%v", leader.Env, follower.Env)
	}
}

func assertDistributedWorkload(t *testing.T, instance dto.Instance, spec runtime.WorkloadSpec, directAddress string, masterPort int) {
	t.Helper()
	if instance.NNodes != 2 || !reflect.DeepEqual(instance.GPUIndexes, []int{0}) || !reflect.DeepEqual(spec.GPUIndexes, instance.GPUIndexes) {
		t.Fatalf("rank %d GPU/topology instance=%+v workload=%+v", instance.NodeRank, instance, spec)
	}
	if !spec.HostNetwork || spec.Image != instance.Image || !reflect.DeepEqual(spec.Entrypoint, instance.Entrypoint) || !reflect.DeepEqual(spec.Cmd, instance.Argv) {
		t.Fatalf("rank %d API/runtime mismatch instance=%+v workload=%+v", instance.NodeRank, instance, spec)
	}
	if !reflect.DeepEqual(spec.Ports, instance.Ports) || !reflect.DeepEqual(spec.DockerArgs, instance.DockerArgs) {
		t.Fatalf("rank %d ports/docker args API=%v/%v runtime=%v/%v", instance.NodeRank, instance.Ports, instance.DockerArgs, spec.Ports, spec.DockerArgs)
	}
	if spec.Env["VLLM_HOST_IP"] != directAddress {
		t.Fatalf("rank %d VLLM_HOST_IP=%q, want %q", instance.NodeRank, spec.Env["VLLM_HOST_IP"], directAddress)
	}
	for _, key := range []string{"NCCL_SOCKET_IFNAME", "GLOO_SOCKET_IFNAME"} {
		if value, ok := spec.Env[key]; !ok || value != "fake0" {
			t.Fatalf("rank %d %s=%q present=%t, want resolved fake0 interface", instance.NodeRank, key, value, ok)
		}
	}
	for flag, value := range map[string]string{
		"--tensor-parallel-size": "1", "--pipeline-parallel-size": "2",
		"--nnodes": "2", "--node-rank": fmt.Sprint(instance.NodeRank),
		"--master-addr": "10.42.0.10", "--master-port": fmt.Sprint(masterPort),
	} {
		if !containsArgPair(spec.Cmd, flag, value) {
			t.Fatalf("rank %d command missing %s %s: %v", instance.NodeRank, flag, value, spec.Cmd)
		}
	}
	if instance.NodeRank == 0 && containsString(spec.Cmd, "--headless") {
		t.Fatalf("leader command unexpectedly headless: %v", spec.Cmd)
	}
	if instance.NodeRank == 1 && !containsString(spec.Cmd, "--headless") {
		t.Fatalf("follower command missing --headless: %v", spec.Cmd)
	}
	// The model and the peers file (assertPeersFileMounted), nothing else.
	if len(spec.Mounts) != 2 || spec.Mounts[0].Target != "/model" || !spec.Mounts[0].ReadOnly || spec.Mounts[1].Target != proto.PeersFileMountPath {
		t.Fatalf("rank %d model mount=%+v", instance.NodeRank, spec.Mounts)
	}
}
