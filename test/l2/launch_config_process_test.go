//go:build multiproc

package l2

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// ADR 0020 across the real process boundary. The in-process tests prove the
// write boundary materializes one launch configuration; these prove the same
// configuration is what a separately built server persists, what it hands to a
// separately built agent, and what a restarted server makes of a row written
// before the boundary existed.

type launchConfigInstance struct {
	UUID         string   `json:"uuid"`
	DeploymentID uint     `json:"deployment_id"`
	State        string   `json:"state"`
	Entrypoint   []string `json:"entrypoint"`
	Argv         []string `json:"argv"`
}

type launchConfigDeployment struct {
	ID          uint           `json:"id"`
	QuickParams map[string]any `json:"quick_params"`
	ExtraArgs   string         `json:"extra_args"`
	Snapshot    struct {
		ExtraArgs []string `json:"extra_args"`
	} `json:"catalog_selection_snapshot"`
}

// TestLaunchConfigMaterializedThroughRealServer submits the one request shape
// the refactor changed - a form that states the utilization and an extra_args
// text that repeats a field the dictionary owns - and follows the resulting
// configuration out of the API response, into the instance's desired state, and
// across the wire into the workload a real agent process was asked to run. The
// second request states nothing and must reach the engine with nothing.
func TestLaunchConfigMaterializedThroughRealServer(t *testing.T) {
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = serverLog, serverLog
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
		func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
	adminToken := login(t, base)
	seed, nodeToken := createNode(t, base, adminToken, "launch-config-process")

	recordPath := filepath.Join(t.TempDir(), "workload.json")
	agentState := filepath.Join(t.TempDir(), "agent-state")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken,
		"--state", agentState)
	agent.Env = append(os.Environ(),
		"GPUWORKS_FAKE_GPU=A100x2,81920",
		// The recording runtime reports every image present and writes the
		// WorkloadSpec it was asked to run, so the argv that crossed the wire
		// is readable without a container runtime.
		"GPUWORKS_FAKE_RUNTIME_RECORD="+recordPath,
		"GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	node := waitNode(t, base, adminToken, func(candidate processNode) bool {
		return candidate.UUID == seed.UUID && candidate.State == "online" && candidate.GPUCount == 2
	})

	modelDir := filepath.Join(t.TempDir(), "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"),
		[]byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":8192,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	localBody, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "path": modelDir, "name": "launch-config-model"})
	localResp, localData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models/local", localBody)
	if localResp.StatusCode != http.StatusCreated {
		t.Fatalf("register local model status=%d body=%s", localResp.StatusCode, localData)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(localData, &model); err != nil {
		t.Fatal(err)
	}

	// The form states the utilization, and extra_args repeats max_model_len with
	// the same value the field carries: a duplicate, not a conflict.
	createBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": node.UUID, "name": "launch-config", "served_name": "launch-config",
		"engine": "vllm", "image": "gpuworks/fake-engine:test",
		"quick_params": map[string]any{"max_model_len": 4096, "gpu_memory_utilization": 0.9},
		"extra_args":   "--max-model-len 4096 --foo",
		// Each deployment is pinned to its own card: both stay running at the
		// same time so the two command lines can be compared side by side.
		"placement": map[string]any{"tp": 1, "gpus": []int{0}},
	})
	createResp, createData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status=%d body=%s", createResp.StatusCode, createData)
	}
	var created launchConfigDeployment
	if err := json.Unmarshal(createData, &created); err != nil {
		t.Fatalf("decode deployment: %v (%s)", err, createData)
	}
	if got := fmt.Sprint(created.QuickParams["gpu_memory_utilization"]); got != "0.9" {
		t.Errorf("stored gpu_memory_utilization=%v, want the stated 0.9: %v", got, created.QuickParams)
	}
	// The dictionary's enum defaults are stated by the server now (plan §4.2):
	// the stored row carries them, and the adapters spell none of them.
	if got := fmt.Sprint(created.QuickParams["dtype"]); got != "auto" {
		t.Errorf("stored dtype=%v, want the dictionary default auto: %v", got, created.QuickParams)
	}
	if got := fmt.Sprint(created.QuickParams["max_model_len"]); got != "4096" {
		t.Errorf("stored max_model_len=%v, want 4096: %v", got, created.QuickParams)
	}
	if strings.TrimSpace(created.ExtraArgs) != "--foo" {
		t.Errorf("extra_args=%q, want only the residual --foo: the dictionary flag must be absorbed, not duplicated", created.ExtraArgs)
	}
	if created.ID == 0 {
		t.Fatalf("created deployment has no id: %s", createData)
	}

	instance := waitLaunchConfigInstance(t, base, adminToken, created.ID)
	argv := append(append([]string(nil), instance.Entrypoint...), instance.Argv...)
	assertLaunchConfigArgv(t, "instance desired state", argv)

	// The same argv has to be what the agent process actually received. The
	// record is written by the runtime driver inside the agent, so reading it
	// proves the configuration crossed the process boundary unchanged.
	recorded := waitRecordedLaunchWorkload(t, recordPath, instance.UUID)
	recordedArgv := append(append([]string(nil), recorded.Entrypoint...), recorded.Cmd...)
	assertLaunchConfigArgv(t, "recorded workload", recordedArgv)
	if !slices.Equal(recordedArgv, argv) {
		t.Fatalf("the agent was asked to run a different command than the instance stores:\ndesired=%v\nrecorded=%v", argv, recordedArgv)
	}
	// The container names the agent that launched it, so several agents
	// sharing one docker daemon can be told apart.
	wantLabels := map[string]string{
		"gpuworks.server": base, "gpuworks.node": node.UUID, "gpuworks.state-dir": agentState,
		"gpuworks.instance": instance.UUID, "gpuworks.deployment": strconv.FormatUint(uint64(created.ID), 10),
	}
	if !maps.Equal(recorded.Labels, wantLabels) {
		t.Fatalf("recorded container labels = %v, want %v", recorded.Labels, wantLabels)
	}

	// A deployment that states no parameters gets the dictionary's own
	// defaults - the exact payload the deploy form's controls always showed -
	// and nothing beyond them. The fake engine's image names no vLLM release,
	// so the per-request metrics switch is left to the engine, not defaulted. (The upgrade guarantee for stored rows lives in
	// the legacy materialization path: re-materializing a row invents nothing.)
	bareBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": node.UUID, "name": "launch-config-bare", "served_name": "launch-config-bare",
		"engine": "vllm", "image": "gpuworks/fake-engine:test",
		"quick_params": map[string]any{},
		"placement":    map[string]any{"tp": 1, "gpus": []int{1}},
	})
	bareResp, bareData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", bareBody)
	if bareResp.StatusCode != http.StatusCreated {
		t.Fatalf("create bare deployment status=%d body=%s", bareResp.StatusCode, bareData)
	}
	var bare launchConfigDeployment
	if err := json.Unmarshal(bareData, &bare); err != nil {
		t.Fatalf("decode bare deployment: %v (%s)", err, bareData)
	}
	bareWant := map[string]string{
		"dtype": "auto", "kv_cache_dtype": "auto", "performance_mode": "balanced",
		"gpu_memory_utilization": "0.9", "enable_cache_report": "true",
	}
	if len(bare.QuickParams) != len(bareWant) {
		t.Errorf("a request that stated nothing stored %v, want exactly the dictionary defaults %v", bare.QuickParams, bareWant)
	}
	for key, want := range bareWant {
		if got := fmt.Sprint(bare.QuickParams[key]); got != want {
			t.Errorf("bare %s=%v, want %v: %v", key, got, want, bare.QuickParams)
		}
	}
	bareInstance := waitLaunchConfigInstance(t, base, adminToken, bare.ID)
	bareArgv := append(append([]string(nil), bareInstance.Entrypoint...), bareInstance.Argv...)
	for _, flag := range []string{"--dtype", "--kv-cache-dtype", "--enable-prefix-caching", "--enable-chunked-prefill"} {
		if got := len(launchConfigFlagValues(bareArgv, flag)); got != 0 {
			t.Errorf("a deployment that stated nothing carries %s; argv=%v", flag, bareArgv)
		}
	}
	// The visible defaults the form always sent do reach the engine.
	for _, flag := range []string{"--gpu-memory-utilization", "--enable-prompt-tokens-details"} {
		if got := len(launchConfigFlagValues(bareArgv, flag)); got != 1 {
			t.Errorf("a deployment that stated nothing must carry %s exactly once; argv=%v", flag, bareArgv)
		}
	}

	// The same flag with a different value is the one new failure ADR 0020
	// introduces: it is refused rather than silently resolved.
	conflictBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": node.UUID, "name": "launch-config-conflict", "served_name": "launch-config-conflict",
		"engine": "vllm", "image": "gpuworks/fake-engine:test",
		"quick_params": map[string]any{"max_model_len": 4096},
		"extra_args":   "--max-model-len 8192",
	})
	conflictResp, conflictData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/deployments", conflictBody)
	if conflictResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("conflicting create status=%d body=%s, want 400", conflictResp.StatusCode, conflictData)
	}
	var conflict struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(conflictData, &conflict); err != nil {
		t.Fatalf("decode conflict error: %v (%s)", err, conflictData)
	}
	if conflict.Code != "LAUNCH_CONFIG_CONFLICT" {
		t.Fatalf("conflict code=%q, want LAUNCH_CONFIG_CONFLICT: %s", conflict.Code, conflictData)
	}
	if conflict.Params["key"] != "max_model_len" || conflict.Params["flag"] != "--max-model-len" ||
		fmt.Sprint(conflict.Params["field_value"]) != "4096" || fmt.Sprint(conflict.Params["extra_value"]) != "8192" {
		t.Fatalf("conflict params=%v, want both values named: %s", conflict.Params, conflictData)
	}
	// A refused request creates nothing.
	listResp, listData := authedDo(t, http.MethodGet, base, adminToken, "/api/v1/deployments", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list deployments status=%d body=%s", listResp.StatusCode, listData)
	}
	var listed []struct {
		Name string `json:"name"`
	}
	if err := apiwire.UnmarshalList(listData, &listed); err != nil {
		t.Fatalf("decode deployment list: %v (%s)", err, listData)
	}
	for _, deployment := range listed {
		if deployment.Name == "launch-config-conflict" {
			t.Fatalf("the refused deployment was created anyway: %s", listData)
		}
	}
}

func assertLaunchConfigArgv(t *testing.T, source string, argv []string) {
	t.Helper()
	if got := launchConfigFlagValues(argv, "--gpu-memory-utilization"); len(got) != 1 || got[0] != "0.9" {
		t.Errorf("%s: --gpu-memory-utilization=%v, want exactly one 0.9; argv=%v", source, got, argv)
	}
	if got := launchConfigFlagValues(argv, "--max-model-len"); len(got) != 1 || got[0] != "4096" {
		t.Errorf("%s: --max-model-len=%v, want exactly one 4096 (the duplicate in extra_args is dropped, not appended); argv=%v", source, got, argv)
	}
	if got := len(launchConfigFlagValues(argv, "--foo")); got != 1 {
		t.Errorf("%s: the residual --foo appears %d times, want once; argv=%v", source, got, argv)
	}
	if got := len(launchConfigFlagValues(argv, "--dtype")); got != 0 {
		t.Errorf("%s: no dtype was stated and none must be emitted; argv=%v", source, argv)
	}
}

// launchConfigFlagValues returns one entry per occurrence of flag, carrying the
// value that followed it in either the "--flag value" or "--flag=value" form
// (empty for a bare boolean flag).
func launchConfigFlagValues(argv []string, flag string) []string {
	var values []string
	for index, token := range argv {
		switch {
		case token == flag:
			value := ""
			if index+1 < len(argv) && !strings.HasPrefix(argv[index+1], "--") {
				value = argv[index+1]
			}
			values = append(values, value)
		case strings.HasPrefix(token, flag+"="):
			values = append(values, strings.TrimPrefix(token, flag+"="))
		}
	}
	return values
}

func waitLaunchConfigInstance(t *testing.T, base, token string, deploymentID uint) launchConfigInstance {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, data := authedDo(t, http.MethodGet, base, token, "/api/v1/instances", nil)
		var instances []launchConfigInstance
		if err := apiwire.UnmarshalList(data, &instances); err != nil {
			t.Fatalf("decode instance list: %v (%s)", err, data)
		}
		for _, instance := range instances {
			if instance.DeploymentID == deploymentID && len(instance.Argv) > 0 {
				return instance
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, detail := authedDo(t, http.MethodGet, base, token, "/api/v1/deployments/"+itoa(deploymentID), nil)
	t.Fatalf("deployment %d never produced an instance with a desired command: %s", deploymentID, detail)
	return launchConfigInstance{}
}

func waitRecordedLaunchWorkload(t *testing.T, recordPath, instanceUUID string) agentruntime.WorkloadSpec {
	t.Helper()
	want := "gpuworks-" + instanceUUID
	if len(instanceUUID) > 12 {
		want = "gpuworks-" + instanceUUID[:12]
	}
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(recordPath)
		if err == nil {
			var workload agentruntime.WorkloadSpec
			if json.Unmarshal(data, &workload) == nil {
				if workload.Name == want {
					return workload
				}
				last = workload.Name
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent never recorded the workload for instance %s (last recorded name %q): %s", instanceUUID, last, recordPath)
	return agentruntime.WorkloadSpec{}
}

// TestLaunchConfigLegacyRowsNormalizedOnRealStartup is the upgrade path: a
// deployment row written before ADR 0020 describes the same launch in three
// places at once, and the reader that merged them at launch time is gone. The
// row here is written straight into the database the server binary created, so
// what is under test is the real startup pass rather than a fixture handed to
// the pure function.
func TestLaunchConfigLegacyRowsNormalizedOnRealStartup(t *testing.T) {
	serverBin := buildBinary(t, "./cmd/gpuworks")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir := t.TempDir()
	serverLogPath := filepath.Join(t.TempDir(), "server.log")
	serverLog, err := os.Create(serverLogPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverLog.Close() })
	startServer := func() *exec.Cmd {
		command := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
		command.Stdout, command.Stderr = serverLog, serverLog
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
			func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized }, serverLogPath)
		return command
	}

	server := startServer()
	running := true
	stop := func() {
		if running {
			stopProcess(server)
			running = false
		}
	}
	t.Cleanup(stop)
	adminToken := login(t, base)
	// A node and a model the row can point at, so the row the startup pass
	// walks is a complete deployment rather than a dangling reference.
	node, _ := createNode(t, base, adminToken, "launch-config-legacy")
	modelBody, _ := json.Marshal(map[string]any{
		"source": "huggingface", "source_id": "gpuworks/legacy-launch-config", "revision": "main",
		"name": "gpuworks/legacy-launch-config", "browser_metadata": true, "weight_bytes": 7,
		"config_raw": `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"max_position_embeddings":8192}`,
	})
	modelResp, modelData := authedDo(t, http.MethodPost, base, adminToken, "/api/v1/models", modelBody)
	if modelResp.StatusCode != http.StatusCreated {
		t.Fatalf("create model status=%d body=%s", modelResp.StatusCode, modelData)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(modelData, &model); err != nil {
		t.Fatal(err)
	}
	stop()

	// The pre-refactor shape: a typed field, an extra_args text restating the
	// same key with another value (extra_args won back then), and a catalog
	// snapshot whose args hold two more dictionary-owned flags, one of them
	// written as an alias.
	const legacyQuick = `{"max_model_len":4096}`
	const legacyExtra = "--max-model-len 8192 --foo"
	const legacySnapshot = `{"spec_name":"legacy","extra_args":["--dtype","half","--bar"]}`
	deploymentID := insertLegacyDeploymentRow(t, dataDir, node.UUID, model.ID, legacyQuick, legacyExtra, legacySnapshot)

	server = startServer()
	running = true
	normalized := readLaunchConfigDeployment(t, base, adminToken, deploymentID)
	if got := fmt.Sprint(normalized.QuickParams["max_model_len"]); got != "8192" {
		t.Errorf("max_model_len=%v, want 8192: the row's own precedence (extra_args wins) must be preserved, not re-decided", got)
	}
	if got := normalized.QuickParams["dtype"]; got != "float16" {
		t.Errorf("dtype=%v, want float16: the snapshot's --dtype half must be split into the field and canonicalized", got)
	}
	if _, filled := normalized.QuickParams["gpu_memory_utilization"]; filled {
		t.Errorf("gpu_memory_utilization=%v: normalization restructures a legacy row and must not add a value it never stored, or the upgrade changes how it starts", normalized.QuickParams["gpu_memory_utilization"])
	}
	if strings.TrimSpace(normalized.ExtraArgs) != "--foo" {
		t.Errorf("extra_args=%q, want --foo: only the tokens the dictionary does not describe stay in the text", normalized.ExtraArgs)
	}
	if !slices.Equal(normalized.Snapshot.ExtraArgs, []string{"--bar"}) {
		t.Errorf("snapshot extra_args=%v, want [--bar]", normalized.Snapshot.ExtraArgs)
	}
	afterFirst := readLaunchConfigColumns(t, dataDir, deploymentID)
	stop()

	// Running again must be a no-op, including at the row level: a startup that
	// rewrites every deployment on every boot would churn the table and its
	// change events forever.
	server = startServer()
	running = true
	afterSecond := readLaunchConfigColumns(t, dataDir, deploymentID)
	if afterSecond != afterFirst {
		t.Fatalf("a second startup rewrote an already normalized row:\nfirst=%+v\nsecond=%+v", afterFirst, afterSecond)
	}
	again := readLaunchConfigDeployment(t, base, adminToken, deploymentID)
	if fmt.Sprint(again.QuickParams) != fmt.Sprint(normalized.QuickParams) || again.ExtraArgs != normalized.ExtraArgs {
		t.Fatalf("a second startup changed the configuration:\nfirst=%+v\nsecond=%+v", normalized, again)
	}
}

type launchConfigColumns struct {
	QuickParams string
	ExtraArgs   string
	Snapshot    string
	UpdatedAt   string
}

// insertLegacyDeploymentRow writes the deployment straight into the server's
// SQLite file. Going through the API would materialize the configuration on the
// way in, which is precisely the shape this test must not have.
func insertLegacyDeploymentRow(t *testing.T, dataDir, nodeUUID string, modelID uint, quickParams, extraArgs, snapshot string) uint {
	t.Helper()
	db := openLaunchConfigDB(t, dataDir)
	var nodeID uint
	if err := db.QueryRow(`SELECT id FROM nodes WHERE uuid = ?`, nodeUUID).Scan(&nodeID); err != nil {
		t.Fatalf("resolve node row: %v", err)
	}
	// The timestamp layout is the one the store's own driver writes.
	now := time.Now().UTC().Format("2006-01-02 15:04:05.999999999-07:00")
	result, err := db.Exec(`INSERT INTO deployments
		(name, model_id, template_id, template_spec_source, template_spec_name, template_scenario,
		 template_deviated, template_snapshot, node_id, node_ids, served_name, engine, image,
		 quick_params, extra_args, env, container_overrides, replicas, v_ram_limit_bytes,
		 partition_backend, sm_limit_pct, sleepable, desired_sleep, placement, dist_group, dist_role,
		 n_nodes, node_rank, master_addr, master_port, desired_state, created_at, updated_at)
		VALUES (?, ?, 0, '', '', '', 0, ?, ?, ?, ?, 'vllm', 'gpuworks/fake-engine:test',
		 ?, ?, '', '', 1, 0, '', 0, 0, 0, '', '', '', 0, 0, '', 0, 'stopped', ?, ?)`,
		"legacy-launch-config", modelID, snapshot, nodeID, fmt.Sprintf("[%d]", nodeID),
		"legacy-launch-config", quickParams, extraArgs, now, now)
	if err != nil {
		t.Fatalf("insert legacy deployment: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil || id == 0 {
		t.Fatalf("legacy deployment id=%d err=%v", id, err)
	}
	return uint(id)
}

func readLaunchConfigColumns(t *testing.T, dataDir string, deploymentID uint) launchConfigColumns {
	t.Helper()
	db := openLaunchConfigDB(t, dataDir)
	var out launchConfigColumns
	if err := db.QueryRow(`SELECT quick_params, extra_args, template_snapshot, updated_at FROM deployments WHERE id = ?`,
		deploymentID).Scan(&out.QuickParams, &out.ExtraArgs, &out.Snapshot, &out.UpdatedAt); err != nil {
		t.Fatalf("read deployment %d: %v", deploymentID, err)
	}
	return out
}

func openLaunchConfigDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	// glebarez/go-sqlite is the driver the server itself uses; opening the same
	// file with another implementation is what the repository forbids.
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "gpuworks.db"))
	if err != nil {
		t.Fatalf("open store database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readLaunchConfigDeployment(t *testing.T, base, token string, id uint) launchConfigDeployment {
	t.Helper()
	resp, data := authedDo(t, http.MethodGet, base, token, "/api/v1/deployments/"+itoa(id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET deployment %d status=%d body=%s", id, resp.StatusCode, data)
	}
	var out launchConfigDeployment
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode deployment %d: %v (%s)", id, err, data)
	}
	return out
}
