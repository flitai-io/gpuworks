package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var launchWarningCodes = []string{
	messages.WarningAscendV1CompatMultiNode, messages.WarningAscendV1CompatDataParallel,
	messages.WarningAscendCompatManualShell, messages.WarningEngramSharedMemoryRuntime,
	messages.WarningEngramConfigUncertain, messages.WarningEngramManualUnchecked,
}

// launchWarningRig is one two-GPU node of the given vendor and runtime plus a
// registered model.
func launchWarningRig(t *testing.T, runtime, vendor string) (*apiHandlers, *store.Store, uint) {
	t.Helper()
	h, modelID, st := pdAPIFixture(t)
	status := nodeStatusTwoGPUs("pd-a", 0)
	status.Sys.RuntimeDriver, status.Sys.GPUVendor = runtime, vendor
	// Four cards: tensor parallel 2 times data parallel 2 must fit.
	status.GPUs = append(status.GPUs, status.GPUs...)
	for index := range status.GPUs {
		status.GPUs[index].Index, status.GPUs[index].Vendor = index, vendor
	}
	if err := st.UpdateNodeStatus("pd-a", status); err != nil {
		t.Fatal(err)
	}
	return h, st, modelID
}

func launchWarningBody(t *testing.T, modelID uint, image string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{
		"name": "launch-warning", "model_id": modelID, "engine": "vllm", "image": image, "node_uuid": "pd-a",
		"quick_params": map[string]any{"max_model_len": 4096},
	}
	for key, value := range extra {
		body[key] = value
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func manualCommand(command string) map[string]any {
	return map[string]any{"container_overrides": map[string]any{"command_mode": "manual", "cmd_display": command}}
}

func requireLaunchWarnings(t *testing.T, h *apiHandlers, body string, want ...string) {
	t.Helper()
	codes := evaluateWarningCodes(t, h, body)
	for _, code := range launchWarningCodes {
		if slices.Contains(codes, code) != slices.Contains(want, code) {
			t.Fatalf("evaluate warnings = %v, want launch warnings %v", codes, want)
		}
	}
}

// A hand-written command that never mentions Engram is not inspected: any
// shell syntax passes on every vendor, with nothing said and nothing refused.
func TestManualShellCommandsWithoutEngramPassSilentlyOnEveryVendor(t *testing.T) {
	commands := map[string]map[string]any{
		"variables":     manualCommand("NAME=demo\nvllm serve /model --served-model-name \"$NAME\""),
		"and list":      manualCommand("cd /tmp && vllm serve /model"),
		"pipe":          manualCommand("vllm serve /model 2>&1 | tee /tmp/engine.log"),
		"redirect":      manualCommand("vllm serve /model > /tmp/engine.log 2>&1"),
		"command subst": manualCommand("export LEN=$(cat /tmp/len)\nvllm serve /model --max-model-len $LEN"),
		"JSON sh -c":    manualCommand(`["sh","-c","cd /tmp && vllm serve /model | tee /tmp/engine.log"]`),
		"JSON bash -lc": manualCommand(`["bash","-lc","export A=$(date +%s); vllm serve /model"]`),
	}
	for _, vendor := range []string{"nvidia", "ppu"} {
		for name, command := range commands {
			t.Run(vendor+"/"+name, func(t *testing.T) {
				h, st, modelID := launchWarningRig(t, "docker", vendor)
				body := launchWarningBody(t, modelID, "private.example/vllm:custom", command)
				requireLaunchWarnings(t, h, body)
				requireCreateSucceeds(t, h, st, body)
			})
		}
	}
}

// The unverifiable combinations show up as evaluate warnings and never stop
// the create.
func TestUnverifiableLaunchesWarnInEvaluateAndCreateSucceeds(t *testing.T) {
	const compat = "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
	const engramJSON = `{"cpu_offload":true,"dp_shared_memory":true}`
	for name, test := range map[string]struct {
		runtime, image string
		extra          map[string]any
		want           []string
	}{
		"Engram the platform cannot read": {
			runtime: "docker", image: "private.example/vllm:custom",
			extra: manualCommand("ARGS=\"--engram-config $CONFIG\"\nvllm serve /model $ARGS"),
			want:  []string{messages.WarningEngramManualUnchecked},
		},
		"Engram shared memory on udocker": {
			runtime: "udocker", image: "private.example/vllm:custom",
			extra: manualCommand("vllm serve /model --engram-config '" + engramJSON + "'"),
			want:  []string{messages.WarningEngramSharedMemoryRuntime},
		},
		"Engram shared memory on Docker is derived, not warned": {
			runtime: "docker", image: "private.example/vllm:custom",
			extra: manualCommand("vllm serve /model --engram-config '" + engramJSON + "'"),
		},
		"compat image with a manual shell": {
			runtime: "udocker", image: compat,
			extra: manualCommand("vllm serve /model --port 30000"),
			want:  []string{messages.WarningAscendCompatManualShell},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, st, modelID := launchWarningRig(t, test.runtime, "nvidia")
			body := launchWarningBody(t, modelID, test.image, test.extra)
			requireLaunchWarnings(t, h, body, test.want...)
			requireCreateSucceeds(t, h, st, body)
		})
	}
}

// A manual command that never states the API port is shown in evaluate with
// what to add, and the create still succeeds: the warning is advice, not a
// refusal of a command the platform cannot prove wrong.
func TestManualCommandWithoutPortWarnsInEvaluateAndCreateSucceeds(t *testing.T) {
	for name, test := range map[string]struct {
		command string
		warn    bool
	}{
		"no port":       {command: "vllm serve /model --served-model-name demo", warn: true},
		"stated port":   {command: "vllm serve /model --port 30000"},
		"port variable": {command: "vllm serve /model --port ${GPUWORKS_API_PORT}"},
	} {
		t.Run(name, func(t *testing.T) {
			h, st, modelID := launchWarningRig(t, "docker", "nvidia")
			body := launchWarningBody(t, modelID, "private.example/vllm:custom", manualCommand(test.command))
			if got := slices.Contains(evaluateWarningCodes(t, h, body), messages.WarningManualCommandPortUnstated); got != test.warn {
				t.Fatalf("port warning = %t, want %t", got, test.warn)
			}
			requireCreateSucceeds(t, h, st, body)
		})
	}
}

// A port the manual command states that another deployment's instance holds
// on the node is named in evaluate; the create still succeeds, since the
// launch runs on the port the platform assigns either way.
func TestManualCommandPortTakenWarnsInEvaluateAndCreateSucceeds(t *testing.T) {
	for name, test := range map[string]struct {
		port  string
		taken bool
	}{
		"taken port": {port: "30000", taken: true},
		"free port":  {port: "30001"},
	} {
		t.Run(name, func(t *testing.T) {
			h, st, modelID := launchWarningRig(t, "docker", "nvidia")
			node, err := st.NodeByUUID("pd-a")
			if err != nil {
				t.Fatal(err)
			}
			holder, err := st.CreateDeployment(&store.Deployment{Name: "holder", ModelID: modelID, NodeID: node.ID, Replicas: 1, PlacementStrategy: store.PlacementSpread})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: "holder-0", DeploymentID: holder.ID, NodeID: node.ID, State: proto.InstRunning,
				Image: "example/engine:1", GPUIndexes: "[0]", Ports: "[30000]"}); err != nil {
				t.Fatal(err)
			}
			body := launchWarningBody(t, modelID, "private.example/vllm:custom", manualCommand("vllm serve /model --port "+test.port))
			if got := slices.Contains(evaluateWarningCodes(t, h, body), messages.WarningManualCommandPortTaken); got != test.taken {
				t.Fatalf("port taken warning = %t, want %t", got, test.taken)
			}
			requireCreateSucceeds(t, h, st, body)
		})
	}
}

// compatTemplateRig is a template whose own image is the compatibility image,
// on a udocker node. Requests built from it never state an image.
type compatTemplateRig struct {
	h      *apiHandlers
	st     *store.Store
	nodeID string
	body   func(extra map[string]any) string
}

func newCompatTemplateRig(t *testing.T, runtime string, mutate func(*store.InferenceSpecRow)) compatTemplateRig {
	t.Helper()
	st, err := store.Open("file:"+strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := deploymentFlatFixture()
	fixture.Specs[0].Image = "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
	fixture.Specs[0].PreStartArgv = ""
	fixture.Specs[0].Env = `{}`
	mutate(&fixture.Specs[0])
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "tpl-node")
	status := nodeStatusTwoGPUs("tpl-node", 0)
	status.GPUs = append(status.GPUs, status.GPUs...)
	for index := range status.GPUs {
		status.GPUs[index].Index = index
	}
	status.Sys.RuntimeDriver = runtime
	if err := st.UpdateNodeStatus("tpl-node", status); err != nil {
		t.Fatal(err)
	}
	return compatTemplateRig{
		h: &apiHandlers{d: Deps{Store: st}}, st: st, nodeID: "tpl-node",
		body: func(extra map[string]any) string {
			body := map[string]any{"template_id": template.ID, "template_spec_name": fixture.Specs[0].Name}
			for key, value := range extra {
				body[key] = value
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		},
	}
}

// V1 with data parallel above one is the DeepSeek tp-by-dp recipe: the
// template's own arguments and placement, no image in the request.
func TestEvaluateWarnsAboutV1CompatDataParallelFromTheTemplate(t *testing.T) {
	const resolver = "def resolve(gpu):\n    if gpu.count != 4:\n        return reject(\"needs four cards\")\n    return {\"GPU_COUNT\": gpu.count, \"TP_SIZE\": 2}\n"
	dataParallel := func(spec *store.InferenceSpecRow) {
		spec.Args = `["--tensor-parallel-size","2","--data-parallel-size","2"]`
		placement, err := json.Marshal(map[string]string{"language": "starlark", "resolver": resolver})
		if err != nil {
			t.Fatal(err)
		}
		spec.Placement = string(placement)
	}
	for name, test := range map[string]struct {
		runtime string
		env     string
		want    []string
	}{
		"V1 on runveil warns":    {runtime: "udocker", env: `{}`, want: []string{messages.WarningAscendV1CompatDataParallel}},
		"V2 on runveil is quiet": {runtime: "udocker", env: `{"VLLM_USE_V2_MODEL_RUNNER":"1"}`},
		"Docker is quiet":        {runtime: "docker", env: `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCompatTemplateRig(t, test.runtime, func(spec *store.InferenceSpecRow) {
				dataParallel(spec)
				spec.Env = test.env
			})
			body := rig.body(map[string]any{"node_uuid": rig.nodeID, "placement_mode": "manual"})
			requireLaunchWarnings(t, rig.h, body, test.want...)
			requireCreateSucceeds(t, rig.h, rig.st, body)
		})
	}
}
