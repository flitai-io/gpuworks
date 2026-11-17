package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func historicalEngineArgsDeployment(t *testing.T) (*apiHandlers, uint) {
	t.Helper()
	h, modelID := engineArgsFixture(t)
	node, err := h.d.Store.NodeWithGPUs("engine-args-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := h.d.Store.CreateDeployment(&store.Deployment{
		Name: "historical-edit", ServedName: "historical-edit", ModelID: modelID, NodeID: node.ID,
		Engine: "sglang", Image: "lmsysorg/sglang:v0.5.21", Replicas: 1,
		QuickParams: `{}`, ExtraArgs: "--kv-cache-dtype future_dtype", Placement: `{"tp":1,"gpus":[0]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, dep.ID
}

func TestEditRejectsChangedInvalidEngineArgs(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			h, id := historicalEngineArgsDeployment(t)
			draft, _ := getDraft(t, h, id)
			edit := draft.Deployment
			edit.ExtraArgs = "--kv-cache-dtype fp8"
			status, body := putEdit(t, h, id, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, dryRun)
			if status != http.StatusBadRequest || !strings.Contains(body, "--kv-cache-dtype") {
				t.Fatalf("changed invalid extra_args: status=%d, want 400 naming the flag: %s", status, body)
			}
			after, _ := getDraft(t, h, id)
			if after.Revision != draft.Revision || after.Deployment.ExtraArgs != draft.Deployment.ExtraArgs {
				t.Fatalf("refused edit changed the stored draft: %+v", after)
			}
		})
	}
}

func TestEditKeepsHistoricalAndValidEngineArgs(t *testing.T) {
	for _, tc := range []struct {
		name, extra, wantExtra, key, value string
		manual                             bool
	}{
		{name: "unchanged historical tokens", extra: "--kv-cache-dtype future_dtype", wantExtra: "--kv-cache-dtype future_dtype"},
		{name: "equivalent historical quoting and whitespace", extra: "  --kv-cache-dtype\t'future_dtype'  ", wantExtra: "--kv-cache-dtype future_dtype"},
		{name: "changed valid target value", extra: "--kv-cache-dtype fp8_e4m3", key: "kv_cache_dtype", value: "fp8_e4m3"},
		{name: "open parser plugin", extra: "--reasoning-parser custom_reasoning_v2", key: "reasoning_parser", value: "custom_reasoning_v2"},
		{name: "unverified and unknown flags pass through", extra: "--cpu-offload-gb 4 --future-custom-mode", wantExtra: "--cpu-offload-gb 4 --future-custom-mode"},
		{name: "manual command owns its dialect", extra: "--kv-cache-dtype fp8", wantExtra: "--kv-cache-dtype fp8", manual: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, id := historicalEngineArgsDeployment(t)
			draft, _ := getDraft(t, h, id)
			edit := draft.Deployment
			edit.StartTimeoutSec = 900 // Editable metadata; identity names remain locked.
			edit.ExtraArgs = tc.extra
			if tc.manual {
				edit.ContainerOverrides = map[string]any{"command_mode": "manual", "cmd_display": `["python3","custom_server.py"]`}
			}
			request := dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}
			status, body := putEdit(t, h, id, request, true)
			if status != http.StatusOK {
				t.Fatalf("allowed dry run=%d: %s", status, body)
			}
			beforeSave, _ := getDraft(t, h, id)
			if beforeSave.Revision != draft.Revision || beforeSave.Deployment.ExtraArgs != draft.Deployment.ExtraArgs {
				t.Fatalf("dry run wrote the deployment: %+v", beforeSave)
			}
			status, body = putEdit(t, h, id, request, false)
			if status != http.StatusOK {
				t.Fatalf("allowed save=%d: %s", status, body)
			}
			after, _ := getDraft(t, h, id)
			if after.Deployment.ExtraArgs != tc.wantExtra || after.Deployment.StartTimeoutSec != 900 {
				t.Fatalf("saved edit lost its arguments or metadata: %+v", after.Deployment)
			}
			if tc.key != "" && after.Deployment.QuickParams[tc.key] != tc.value {
				t.Fatalf("target parameter was not materialized: %+v", after.Deployment.QuickParams)
			}
			if tc.manual && after.Deployment.ContainerOverrides["cmd_display"] != `["python3","custom_server.py"]` {
				t.Fatalf("manual command was changed: %+v", after.Deployment.ContainerOverrides)
			}
		})
	}
}
