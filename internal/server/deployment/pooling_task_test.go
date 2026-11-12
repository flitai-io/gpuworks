package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestPoolingMaterializationPreservesCommandOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		overrides   map[string]any
		entrypoint  []string
	}{
		{name: "manual command", overrides: map[string]any{"command_mode": "manual", "cmd_display": "python3 custom.py"}},
		{name: "full command replacement", overrides: map[string]any{"cmd_display": "python3 custom.py"}},
		{name: "custom entrypoint", entrypoint: []string{"python3", "custom.py"}},
		{name: "explicit runner", extra: "--runner generate"},
		{name: "explicit legacy task", extra: "--task auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &dto.DeployRequest{ExtraArgs: tc.extra, ContainerOverrides: tc.overrides}
			selection := templateSelection{APIContract: json.RawMessage(`{"operations":[{"type":"openai.rerank"}]}`), Entrypoint: tc.entrypoint}
			result, err := materializeDeployRequest("vllm", req, selection, nil, "0.23.0")
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(append(result.TemplateResidual, result.UserResidual...), " ")
			if strings.Contains(joined, "pooling") || (tc.extra == "" && (strings.Contains(joined, "--runner") || strings.Contains(joined, "--task"))) {
				t.Fatalf("command gained implicit task: %v", joined)
			}
			if tc.extra != "" && joined != tc.extra {
				t.Fatalf("explicit selection changed: %q want %q", joined, tc.extra)
			}
		})
	}
}
