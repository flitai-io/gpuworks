package report

import (
	"fmt"
	"strings"
)

type targetSnapshot struct {
	Entry          string        `json:"entry"`
	Role           string        `json:"role"`
	DeploymentID   uint          `json:"deployment_id"`
	InstanceUUID   string        `json:"instance_uuid"`
	TensorParallel int           `json:"tensor_parallel"`
	NodeID         uint          `json:"node_id"`
	NodeName       string        `json:"node_name"`
	GPUIndexes     []int         `json:"gpu_indexes"`
	GPUs           []snapshotGPU `json:"gpus"`
	TargetKind     string        `json:"target_kind"`
	DeploymentName string        `json:"deployment_name"`
	Model          string        `json:"model"`
	ModelSourceID  string        `json:"model_source_id"`
	ServedName     string        `json:"served_name"`
	BaseURL        string        `json:"base_url"`
	LoadOrigin     string        `json:"load_origin"`
	Engine         string        `json:"engine"`
	// PriorityScheduling is nil when the snapshot does not say.
	PriorityScheduling *bool             `json:"priority_scheduling"`
	Image              string            `json:"image"`
	CommandMode        string            `json:"command_mode"`
	Entrypoint         []string          `json:"entrypoint"`
	Argv               []string          `json:"argv"`
	Env                map[string]string `json:"env"`
	DockerArgs         []string          `json:"docker_args"`
	NumGPUs            int               `json:"num_gpus"`
}

type snapshotGPU struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	ArchID   string `json:"arch_id"`
	Vendor   string `json:"vendor"`
	UUID     string `json:"uuid"`
	NodeID   uint   `json:"node_id"`
	NodeName string `json:"node_name"`
}

func targetProperties(target targetSnapshot) [][2]string {
	candidates := [][2]string{
		{"Target type", target.TargetKind}, {"Entry", target.Entry}, {"Role", target.Role}, {"Deployment ID", optionalInt(int(target.DeploymentID))}, {"Instance UUID", target.InstanceUUID}, {"Model", target.Model}, {"Model source ID", target.ModelSourceID}, {"Served name", target.ServedName}, {"Node", target.NodeName}, {"Node ID", optionalInt(int(target.NodeID))}, {"Tensor parallel", optionalInt(target.TensorParallel)}, {"Deployment", target.DeploymentName}, {"Endpoint", target.BaseURL},
		{"Load origin", target.LoadOrigin}, {"Engine", target.Engine}, {"Priority scheduling", optionalBool(target.PriorityScheduling)}, {"Image", target.Image},
		{"GPU count", optionalInt(target.NumGPUs)}, {"Command mode", target.CommandMode},
		{"Entrypoint", strings.Join(target.Entrypoint, " ")}, {"argv", strings.Join(target.Argv, " ")}, {"Docker args", strings.Join(target.DockerArgs, " ")},
	}
	properties := make([][2]string, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate[1]) != "" {
			properties = append(properties, candidate)
		}
	}
	if len(target.GPUIndexes) > 0 {
		properties = append(properties, [2]string{"GPU indexes", jsonValue(target.GPUIndexes)})
	}
	for i, gpu := range target.GPUs {
		properties = append(properties, [2]string{fmt.Sprintf("GPU %d", i+1), fmt.Sprintf("index=%d; name=%s; arch=%s; vendor=%s; UUID=%s; node=%s (%d)", gpu.Index, unknown(gpu.Name), unknown(gpu.ArchID), unknown(gpu.Vendor), unknown(gpu.UUID), unknown(gpu.NodeName), gpu.NodeID)})
	}
	if target.TargetKind != "" || len(properties) > 0 {
		for _, field := range []string{"Model revision", "Tokenizer revision", "Engine version", "Image digest", "Expert parallel", "Data parallel", "Weight precision", "Activation precision", "KV precision", "Service context limit", "Executed quantization kernel"} {
			properties = append(properties, [2]string{field, "unknown (not captured)"})
		}
		if len(target.GPUs) == 0 {
			properties = append(properties, [2]string{"GPU hardware", "unknown (not captured)"})
		}
		properties = append(properties, [2]string{"Snapshot source", "saved run target snapshot; launch configuration is not runtime kernel evidence"})
	}
	return properties
}

// optionalBool is enabled or disabled, and nothing when the fact is unknown.
func optionalBool(value *bool) string {
	switch {
	case value == nil:
		return ""
	case *value:
		return "enabled"
	}
	return "disabled"
}
