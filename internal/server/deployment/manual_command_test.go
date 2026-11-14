package deployment

import (
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func draftRanks(t *testing.T) []deploy.ResolveLaunchInput {
	t.Helper()
	engine, err := deploy.SelectEngine(deploy.EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	ranks := make([]deploy.ResolveLaunchInput, 2)
	for rank := range ranks {
		ranks[rank] = deploy.ResolveLaunchInput{Engine: engine, Params: deploy.Params{
			ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 2,
			NNodes: 2, NodeRank: rank, Headless: rank > 0, MasterAddr: "192.0.2.10", MasterPort: 29500,
		}}
	}
	return ranks
}

func hasWarning(resp dto.EvaluateResponse, code string) bool {
	for _, warning := range resp.Warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func TestSetManualCommandDraft(t *testing.T) {
	t.Run("built from the engine-mode launch whatever the request states", func(t *testing.T) {
		ranks := draftRanks(t)
		var engine dto.EvaluateResponse
		setManualCommandDraft(&engine, ranks)
		for index := range ranks {
			ranks[index].ContainerOverrides = deploy.ContainerOverrides{CommandMode: string(deploy.CommandManual), CmdDisplay: "echo ${GPUWORKS_NODE_RANK}", CommandIncludesPreparation: true}
		}
		var manual dto.EvaluateResponse
		setManualCommandDraft(&manual, ranks)
		if engine.ManualCommandDraft == "" || manual.ManualCommandDraft != engine.ManualCommandDraft || len(manual.ManualCommandVariables) == 0 || hasWarning(manual, messages.WarningManualDraftUnavailable) {
			t.Fatalf("draft = %q (engine %q) variables %v warnings %+v", manual.ManualCommandDraft, engine.ManualCommandDraft, manual.ManualCommandVariables, manual.Warnings)
		}
	})
	t.Run("ranks of one class that differ have no draft", func(t *testing.T) {
		ranks := draftRanks(t)
		ranks[1].Params.Headless = false
		ranks[1].Params.ExtraArgs = []string{"--enforce-eager"}
		var resp dto.EvaluateResponse
		setManualCommandDraft(&resp, ranks)
		if resp.ManualCommandDraft != "" || resp.ManualCommandVariables != nil || !hasWarning(resp, messages.WarningManualDraftUnavailable) {
			t.Fatalf("draft = %q variables %v warnings %+v", resp.ManualCommandDraft, resp.ManualCommandVariables, resp.Warnings)
		}
	})
	t.Run("a rank the plan refused before resolving has no draft", func(t *testing.T) {
		ranks := draftRanks(t)
		ranks[1] = deploy.ResolveLaunchInput{}
		var resp dto.EvaluateResponse
		setManualCommandDraft(&resp, ranks)
		if resp.ManualCommandDraft != "" || !hasWarning(resp, messages.WarningManualDraftUnavailable) {
			t.Fatalf("draft = %q warnings %+v", resp.ManualCommandDraft, resp.Warnings)
		}
	})
	t.Run("one launch has none and no warning", func(t *testing.T) {
		var resp dto.EvaluateResponse
		setManualCommandDraft(&resp, draftRanks(t)[:1])
		if resp.ManualCommandDraft != "" || len(resp.Warnings) != 0 {
			t.Fatalf("single launch = %q %+v", resp.ManualCommandDraft, resp.Warnings)
		}
	})
}

func TestCheckManualCommandRanks(t *testing.T) {
	manual := func(script string) map[string]any {
		return map[string]any{"command_mode": "manual", "cmd_display": script}
	}
	pdMultiNode := map[string]any{"pd": map[string]any{"mode": "vllm-nixl", "roles": map[string]any{
		"prefill": map[string]any{"tp": 1, "units": []any{map[string]any{"node_uuid": "a"}}},
		"decode":  map[string]any{"tp": 2, "units": []any{map[string]any{"node_uuid": "b", "followers": []any{map[string]any{"node_uuid": "c"}}}}},
	}}}
	for _, tc := range []struct {
		name    string
		req     dto.DeployRequest
		missing string
	}{
		{"single node", dto.DeployRequest{NNodes: 1, ContainerOverrides: manual("vllm serve")}, ""},
		{"engine mode", dto.DeployRequest{NNodes: 2, ContainerOverrides: map[string]any{"cmd_display": `["vllm","serve"]`}}, ""},
		{"empty manual command", dto.DeployRequest{NNodes: 2, ContainerOverrides: manual("  ")}, ""},
		{"multi-node blind", dto.DeployRequest{NNodes: 2, ContainerOverrides: manual("vllm serve --node-rank ${NODE_RANK}")}, "[GPUWORKS_NODE_RANK]"},
		{"multi-node by rank", dto.DeployRequest{NNodes: 2, ContainerOverrides: manual("vllm serve --node-rank ${GPUWORKS_NODE_RANK}")}, ""},
		{"multi-node by headless", dto.DeployRequest{NNodes: 2, ContainerOverrides: manual(`case "${GPUWORKS_HEADLESS}" in 1) exec a;; esac`)}, ""},
		{"pd with a multi-node unit", dto.DeployRequest{Placement: pdMultiNode, ContainerOverrides: manual("vllm serve ${GPUWORKS_PD_ROLE}")}, "[GPUWORKS_NODE_RANK]"},
		{"pd blind", dto.DeployRequest{Placement: pdMultiNode, ContainerOverrides: manual("vllm serve")}, "[GPUWORKS_PD_ROLE GPUWORKS_NODE_RANK]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkManualCommandRanks(&tc.req)
			if tc.missing == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var product *messages.ProductError
			if !errors.As(err, &product) || product.Code() != messages.ErrorManualCommandRankBlind || fmt.Sprint(product.Params()["missing"]) != tc.missing {
				t.Fatalf("err = %v, want %s missing %s", err, messages.ErrorManualCommandRankBlind, tc.missing)
			}
		})
	}
}
