package api

import (
	"encoding/json"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type benchmarkTargetSnapshot struct {
	TargetKind string `json:"target_kind"`
	// Entry is gateway or instance for a managed target; Role is the PD role
	// of a directly measured instance, whose result is then not end-to-end.
	Entry          string `json:"entry,omitempty"`
	Role           string `json:"role,omitempty"`
	DeploymentID   uint   `json:"deployment_id,omitempty"`
	DeploymentName string `json:"deployment_name,omitempty"`
	InstanceUUID   string `json:"instance_uuid,omitempty"`
	ServedName     string `json:"served_name,omitempty"`
	Model          string `json:"model"`
	ModelSourceID  string `json:"model_source_id,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	LoadOrigin     string `json:"load_origin"`
	Engine         string `json:"engine,omitempty"`
	// PriorityScheduling says whether the measured instance was launched
	// with its engine's priority scheduling, so a requested class orders its
	// requests; absent for an external endpoint and runs from before it.
	PriorityScheduling *bool                  `json:"priority_scheduling,omitempty"`
	Image              string                 `json:"image,omitempty"`
	CommandMode        string                 `json:"command_mode,omitempty"`
	Entrypoint         []string               `json:"entrypoint,omitempty"`
	Argv               []string               `json:"argv,omitempty"`
	Env                map[string]string      `json:"env,omitempty"`
	DockerArgs         []string               `json:"docker_args,omitempty"`
	GPUIndexes         []int                  `json:"gpu_indexes,omitempty"`
	GPUs               []benchmarkSnapshotGPU `json:"gpus,omitempty"`
	NumGPUs            int                    `json:"num_gpus"`
	TensorParallel     int                    `json:"tensor_parallel,omitempty"`
	NodeID             uint                   `json:"node_id,omitempty"`
	NodeName           string                 `json:"node_name,omitempty"`
}

type benchmarkSnapshotGPU struct {
	Index    int    `json:"index"`
	Name     string `json:"name,omitempty"`
	ArchID   string `json:"arch_id,omitempty"`
	Vendor   string `json:"vendor,omitempty"`
	UUID     string `json:"uuid,omitempty"`
	NodeID   uint   `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
}

var benchmarkSensitiveEnvSuffixes = []string{
	"TOKEN", "PASSWORD", "SECRET", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "CREDENTIAL", "CREDENTIALS",
}

func redactBenchmarkEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		upper := strings.ToUpper(strings.TrimSpace(key))
		for _, suffix := range benchmarkSensitiveEnvSuffixes {
			if strings.HasSuffix(upper, suffix) {
				value = "<redacted>"
				break
			}
		}
		out[key] = value
	}
	return out
}

// benchmarkTensorParallel reads the deployment's tensor-parallel degree out of
// its own argv. The flag spellings come from the engine (deploy.TPFlagsForEngine)
// rather than from a list restated here: an engine that spells the dimension
// --tp would otherwise be reported at its fallback.
func benchmarkTensorParallel(engine string, argv []string, fallback int) int {
	flags := deploy.TPFlagsForEngine(engine)
	for i, arg := range argv {
		for _, flag := range flags {
			if value, found := strings.CutPrefix(arg, flag+"="); found {
				if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
					return parsed
				}
			}
			if arg == flag && i+1 < len(argv) {
				if parsed, err := strconv.Atoi(argv[i+1]); err == nil && parsed > 0 {
					return parsed
				}
			}
		}
	}
	if fallback > 0 {
		return fallback
	}
	return 1
}

func displayBenchmarkNodeName(node *store.Node) string {
	if node == nil {
		return ""
	}
	if strings.TrimSpace(node.Name) != "" {
		return node.Name
	}
	if strings.TrimSpace(node.Hostname) != "" {
		return node.Hostname
	}
	return node.UUID
}

func (a *apiHandlers) benchmarkTargetSnapshot(targetKind, entry, baseURL, model, loadOrigin string, numGPUs int, dep *store.Deployment, leader *store.Instance, instances []store.Instance) (string, error) {
	snapshot := benchmarkTargetSnapshot{TargetKind: targetKind, Entry: entry, BaseURL: baseURL, Model: model, LoadOrigin: loadOrigin, NumGPUs: numGPUs}
	if entry == benchmarkEntryInstance && leader != nil {
		snapshot.Role = leader.Role
	}
	if dep == nil || leader == nil {
		raw, err := json.Marshal(snapshot)
		return string(raw), err
	}
	snapshot.DeploymentID = dep.ID
	snapshot.DeploymentName = dep.Name
	snapshot.InstanceUUID = leader.UUID
	snapshot.ServedName = dep.ServedName
	if storedModel, err := a.d.Store.GetModel(dep.ModelID); err == nil {
		snapshot.ModelSourceID = storedModel.SourceID
	}
	var spec proto.InstanceDesiredState
	if strings.TrimSpace(leader.DesiredStateJSON) != "" {
		if err := json.Unmarshal([]byte(leader.DesiredStateJSON), &spec); err != nil {
			return "", err
		}
	}
	if spec.Engine == "" {
		spec.Engine = dep.Engine
	}
	if spec.Image == "" {
		spec.Image = leader.Image
		if spec.Image == "" {
			spec.Image = dep.Image
		}
	}
	if len(spec.GPUIndexes) == 0 && leader.GPUIndexes != "" {
		if err := json.Unmarshal([]byte(leader.GPUIndexes), &spec.GPUIndexes); err != nil {
			return "", err
		}
	}
	snapshot.Engine = spec.Engine
	scheduling := deploy.SpecRunsPriority(leader.DesiredStateJSON)
	snapshot.PriorityScheduling = &scheduling
	snapshot.Image = spec.Image
	snapshot.CommandMode = spec.CommandMode
	snapshot.Entrypoint = append([]string(nil), spec.Entrypoint...)
	snapshot.Argv = append([]string(nil), spec.Argv...)
	snapshot.Env = redactBenchmarkEnv(spec.Env)
	snapshot.DockerArgs = append([]string(nil), spec.DockerArgs...)
	snapshot.GPUIndexes = append([]int(nil), spec.GPUIndexes...)
	snapshot.TensorParallel = benchmarkTensorParallel(spec.Engine, spec.Argv, numGPUs)
	if node, err := a.d.Store.NodeByID(leader.NodeID); err == nil {
		snapshot.NodeID = node.ID
		snapshot.NodeName = displayBenchmarkNodeName(node)
	}
	for _, instance := range instances {
		// planning.InstanceAsleep, not the reported level: a replica under a
		// sleep order is already refused by the gateway and holds no GPU it
		// would answer from.
		if instance.State != proto.InstRunning || planning.InstanceAsleep(&instance) {
			continue
		}
		node, err := a.d.Store.NodeByID(instance.NodeID)
		if err != nil {
			continue
		}
		gpuByIndex := make(map[int]store.GPU, len(node.GPUs))
		for _, gpu := range node.OwnedGPUs() {
			gpuByIndex[gpu.Index] = gpu
		}
		var indexes []int
		if json.Unmarshal([]byte(instance.GPUIndexes), &indexes) != nil {
			var instanceSpec proto.InstanceDesiredState
			if json.Unmarshal([]byte(instance.DesiredStateJSON), &instanceSpec) == nil {
				indexes = instanceSpec.GPUIndexes
			}
		}
		for _, index := range indexes {
			gpu := gpuByIndex[index]
			snapshot.GPUs = append(snapshot.GPUs, benchmarkSnapshotGPU{Index: index, Name: gpu.Name, ArchID: gpu.ArchID, Vendor: gpu.Vendor, UUID: gpu.UUID, NodeID: node.ID, NodeName: displayBenchmarkNodeName(node)})
		}
	}
	raw, err := json.Marshal(snapshot)
	return string(raw), err
}
