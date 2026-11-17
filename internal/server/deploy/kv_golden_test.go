package deploy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// kvGoldenDir holds one record per vLLM KV launch the KV offload backend seam
// must leave untouched: everything the platform derives from a launch
// configuration that states (or carries) KV offload, from materialization to
// the resolved launch and back to catalog args. Regenerate with
// GPUWORKS_UPDATE_KV_GOLDEN=1 and review every diff: the only change the seam
// itself may make is none.
//
//	GPUWORKS_UPDATE_KV_GOLDEN=1 go test ./internal/server/deploy -run TestVLLMKVLaunchGoldens
const kvGoldenDir = "testdata/kv"

const kvGoldenUpdateEnv = "GPUWORKS_UPDATE_KV_GOLDEN"

// kvGoldenCase is one launch as the deploy form and the PD planner state it.
type kvGoldenCase struct {
	name         string
	vendor       string
	raw          map[string]any
	unset        []string
	templateArgs []string
	extraArgs    []string
	pdMode       string
	pdRole       string
	nNodes       int
	pp           int
	gpus         int
	overrides    ContainerOverrides
}

// kvGoldenRecord is what a case pins. Each stage records its error instead of
// failing the test, so a refusal is as much part of the corpus as a launch.
type kvGoldenRecord struct {
	Materialize kvGoldenMaterialize `json:"materialize"`
	Launch      *kvGoldenLaunch     `json:"launch,omitempty"`
	Export      *kvGoldenExport     `json:"export,omitempty"`
	// Unsettable is the engine's CanUnsetTemplateFlag verdict for every
	// --kv-transfer-config value the case's layers and final argv carry.
	Unsettable []kvGoldenVerdict `json:"unsettable,omitempty"`
}

type kvGoldenMaterialize struct {
	Error            *kvGoldenError     `json:"error,omitempty"`
	Quick            json.RawMessage    `json:"quick,omitempty"`
	TemplateResidual []string           `json:"template_residual,omitempty"`
	UserResidual     []string           `json:"user_residual,omitempty"`
	Moved            []MovedFlag        `json:"moved,omitempty"`
	Validation       *kvGoldenError     `json:"validation,omitempty"`
	Performance      []messages.Warning `json:"performance_warnings,omitempty"`
	RoleQuick        json.RawMessage    `json:"role_quick,omitempty"`
}

type kvGoldenLaunch struct {
	Error             *kvGoldenError     `json:"error,omitempty"`
	Command           []string           `json:"command,omitempty"`
	Env               map[string]string  `json:"env,omitempty"`
	Mounts            []Mount            `json:"mounts,omitempty"`
	ShmBytes          int64              `json:"shm_bytes"`
	RequestedFeatures json.RawMessage    `json:"requested_features,omitempty"`
	Warnings          []messages.Warning `json:"warnings,omitempty"`
	ReverseParams     map[string]any     `json:"reverse_params,omitempty"`
	ReverseResidual   []string           `json:"reverse_residual,omitempty"`
	ReverseError      *kvGoldenError     `json:"reverse_error,omitempty"`
}

type kvGoldenExport struct {
	Error   *kvGoldenError `json:"error,omitempty"`
	Args    []string       `json:"args,omitempty"`
	Dropped []string       `json:"dropped,omitempty"`
}

type kvGoldenVerdict struct {
	Source string `json:"source"`
	Value  string `json:"value"`
	Unset  bool   `json:"unsettable"`
}

type kvGoldenError struct {
	Code   string         `json:"code,omitempty"`
	Params map[string]any `json:"params,omitempty"`
	Text   string         `json:"text"`
}

func kvGoldenErr(err error) *kvGoldenError {
	if err == nil {
		return nil
	}
	out := &kvGoldenError{Text: err.Error()}
	var product *messages.ProductError
	if errors.As(err, &product) {
		out.Code, out.Params = product.Code(), product.Params()
	}
	return out
}

func TestVLLMKVLaunchGoldens(t *testing.T) {
	for _, test := range kvGoldenCases() {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.MarshalIndent(recordKVGoldenCase(t, test), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			compareGolden(t, filepath.Join(kvGoldenDir, test.name+".golden"), append(encoded, '\n'), kvGoldenUpdateEnv)
		})
	}
}

// compareGolden writes got under the update variable and otherwise requires
// it to equal the stored file byte for byte.
func compareGolden(t *testing.T, path string, got []byte, updateEnv string) {
	t.Helper()
	if os.Getenv(updateEnv) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with %s=1): %v", updateEnv, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s changed (regenerate with %s=1 and review the diff):\n--- want\n%s\n--- got\n%s", path, updateEnv, want, got)
	}
}

func recordKVGoldenCase(t *testing.T, test kvGoldenCase) kvGoldenRecord {
	t.Helper()
	var record kvGoldenRecord
	for _, value := range kvTransferValues(test.templateArgs) {
		record.Unsettable = append(record.Unsettable, kvGoldenVerdict{"template_args", value, vLLMEngine{}.CanUnsetTemplateFlag("--kv-transfer-config", value)})
	}
	for _, value := range kvTransferValues(test.extraArgs) {
		record.Unsettable = append(record.Unsettable, kvGoldenVerdict{"extra_args", value, vLLMEngine{}.CanUnsetTemplateFlag("--kv-transfer-config", value)})
	}
	out, err := Materialize(MaterializeInput{
		Engine: EngineVLLM, Raw: test.raw, Unset: test.unset,
		TemplateArgs: test.templateArgs, ExtraArgs: test.extraArgs,
	})
	if err != nil {
		record.Materialize.Error = kvGoldenErr(err)
		return record
	}
	quick := out.Quick
	record.Materialize.Quick = mustJSON(t, quick)
	record.Materialize.TemplateResidual = out.TemplateResidual
	record.Materialize.UserResidual = out.UserResidual
	record.Materialize.Moved = out.Moved
	record.Materialize.Validation = kvGoldenErr(ValidateLaunchConfigForEngine(EngineVLLM, quick))
	gpus := max(test.gpus, 1)
	record.Materialize.Performance = PerformanceWarnings(quick, strings.Join(test.extraArgs, " "), "", gpus)

	params := Params{
		Vendor: test.vendor, ModelPath: "/model", ServedName: "qwen", APIPort: 30000,
		AssignedGPUs: gpus, TensorParallel: 1, NNodes: 1, Quick: quick,
		TemplateArgs: out.TemplateResidual, ExtraArgs: out.UserResidual,
	}
	if test.pdMode != "" {
		params.Quick = PDRoleLaunchConfig(EngineVLLM, quick, test.pdRole)
		record.Materialize.RoleQuick = mustJSON(t, params.Quick)
		params.PD = &PDLaunch{
			Mode: test.pdMode, Role: test.pdRole, SideChannelHost: "10.0.0.5", SideChannelPort: 30001,
			RoleTP: map[string]int{pdmode.RolePrefill: 1, pdmode.RoleDecode: 1},
		}
	}
	if test.nNodes > 1 {
		params.NNodes, params.MasterAddr, params.MasterPort = test.nNodes, "192.0.2.10", DefaultMasterPort
		params.TensorParallel = test.nNodes
	}
	if test.pp > 1 {
		params.PipelineParallel, params.AssignedGPUs = test.pp, test.pp
	}
	launch, warnings, err := ResolveLaunchWithWarnings(ResolveLaunchInput{
		Engine: vLLMEngine{}, Params: params, ContainerOverrides: test.overrides,
		TargetValidation: ValidateAssignedTarget, ModelWeightBytes: 16 << 30,
	})
	record.Launch = &kvGoldenLaunch{Error: kvGoldenErr(err)}
	if err == nil {
		command := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
		record.Launch.Command = command
		record.Launch.Env = launch.Env
		record.Launch.Mounts = launch.Mounts
		record.Launch.ShmBytes = launch.ShmBytes
		record.Launch.RequestedFeatures = mustJSON(t, launch.RequestedFeatures)
		record.Launch.Warnings = warnings
		reverse, residual, reverseErr := ReverseArgs(EngineVLLM, launch.Argv)
		record.Launch.ReverseParams, record.Launch.ReverseResidual = reverse, residual
		record.Launch.ReverseError = kvGoldenErr(reverseErr)
		for _, value := range kvTransferValues(command) {
			record.Unsettable = append(record.Unsettable, kvGoldenVerdict{"argv", value, vLLMEngine{}.CanUnsetTemplateFlag("--kv-transfer-config", value)})
		}
	} else {
		// A refused launch still reports what was requested of the target.
		launch, collectErr := ResolveLaunch(ResolveLaunchInput{
			Engine: vLLMEngine{}, Params: params, ContainerOverrides: test.overrides,
			TargetValidation: CollectTargetCapabilities, ModelWeightBytes: 16 << 30,
		})
		if collectErr == nil {
			record.Launch.RequestedFeatures = mustJSON(t, launch.RequestedFeatures)
		}
	}
	exported, err := ExportLaunchArgs(EngineVLLM, quick, out.TemplateResidual, out.UserResidual)
	record.Export = &kvGoldenExport{Error: kvGoldenErr(err), Args: exported.Args, Dropped: exported.Dropped}
	return record
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// kvTransferValues returns every --kv-transfer-config value args states, in
// any spelling vLLM's argument parser accepts.
func kvTransferValues(args []string) []string {
	var out []string
	for index, arg := range args {
		for _, flag := range []string{"--kv-transfer-config", "--kv_transfer_config"} {
			if arg == flag && index+1 < len(args) {
				out = append(out, args[index+1])
			} else if value, ok := strings.CutPrefix(arg, flag+"="); ok {
				out = append(out, value)
			}
		}
	}
	return out
}

// kvGoldenTiers are the KV tiers the deploy form states.
var kvGoldenTiers = []struct {
	name string
	raw  map[string]any
}{
	{"none", nil},
	{"cpu", map[string]any{"kv_offloading_size": 8}},
	{"cpu_disk", map[string]any{"kv_offloading_size": 16, "kv_disk_offload": true, "shared_kv_dir": "/kv-cache"}},
	{"cpu_disk_tuned", map[string]any{
		"kv_offloading_size": 16, "kv_disk_offload": true, "shared_kv_dir": "/kv-cache", "kv_disk_limit_gib": 64,
		"kv_offload_read_threads": 8, "kv_offload_write_threads": 4, "kv_offload_block_size": 32,
	}},
}

func cloneRaw(raw map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range raw {
		out[key] = value
	}
	return out
}

const (
	kvGoldenNIXLBoth    = `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`
	kvGoldenLMCacheBoth = `{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_both"}`
)

func kvGoldenCases() []kvGoldenCase {
	var cases []kvGoldenCase
	for _, tier := range kvGoldenTiers {
		for _, vendor := range []string{"nvidia", "kunlunxin"} {
			cases = append(cases, kvGoldenCase{name: "single_" + vendor + "_" + tier.name, vendor: vendor, raw: tier.raw})
		}
		for _, name := range pdmode.ForEngine(EngineVLLM) {
			mode, _ := pdmode.Lookup(name)
			for _, role := range mode.Roles {
				cases = append(cases, kvGoldenCase{
					name: "pd_" + name + "_" + role + "_" + tier.name, vendor: mode.Vendors[0], raw: tier.raw,
					pdMode: name, pdRole: role,
				})
			}
		}
	}
	disk := kvGoldenTiers[2].raw
	cpu := kvGoldenTiers[1].raw
	dp := cloneRaw(disk)
	dp["data_parallel_size"] = 2
	cases = append(cases,
		kvGoldenCase{name: "multinode_nnodes2_cpu_disk", vendor: "nvidia", raw: disk, nNodes: 2},
		kvGoldenCase{name: "parallel_pp2_cpu_disk", vendor: "nvidia", raw: disk, pp: 2},
		kvGoldenCase{name: "parallel_dp2_cpu_disk", vendor: "nvidia", raw: dp, gpus: 2},
	)
	// A connector stated in a residual layer, in every spelling vLLM's parser
	// accepts, next to the platform's CPU tier.
	for _, spelling := range []struct {
		name string
		args func(string) []string
	}{
		{"hyphen", func(value string) []string { return []string{"--kv-transfer-config", value} }},
		{"underscore", func(value string) []string { return []string{"--kv_transfer_config", value} }},
		{"equals", func(value string) []string { return []string{"--kv-transfer-config=" + value} }},
	} {
		cases = append(cases,
			kvGoldenCase{name: "extra_args_" + spelling.name + "_lmcache_cpu", vendor: "nvidia", raw: cpu, extraArgs: spelling.args(kvGoldenLMCacheBoth)},
			kvGoldenCase{name: "template_" + spelling.name + "_lmcache_cpu", vendor: "nvidia", raw: cpu, templateArgs: spelling.args(kvGoldenLMCacheBoth)},
			kvGoldenCase{name: "pd_prefill_extra_args_" + spelling.name, vendor: "nvidia", raw: cpu, extraArgs: spelling.args(kvGoldenLMCacheBoth), pdMode: "vllm-nixl", pdRole: pdmode.RolePrefill},
		)
	}
	// A hand-written NIXL connector in kv_both is a PD connector whatever its
	// role: it stays a PD request, in the user's args and in a custom command.
	custom := ContainerOverrides{
		EntrypointDisplay: `["python3","-m","vllm.entrypoints.openai.api_server"]`,
		CmdDisplay:        `["--model","/model","--port","30000","--kv-transfer-config",` + jsonString(kvGoldenNIXLBoth) + `]`,
	}
	for _, vendor := range []string{"nvidia", "kunlunxin"} {
		cases = append(cases,
			kvGoldenCase{name: "extra_args_nixl_both_" + vendor, vendor: vendor, extraArgs: []string{"--kv-transfer-config", kvGoldenNIXLBoth}},
			kvGoldenCase{name: "custom_command_nixl_both_" + vendor, vendor: vendor, overrides: custom},
		)
	}
	// quick_params_unset over the disk tier against a template that states it.
	residual := []string{"--kv-transfer-config", diskKVTransferConfig(LaunchConfig{
		KVOffloadingSize: 16, KVDiskOffload: true, SharedKVDir: "/kv-cache", KVOffloadReadThreads: 8, KVOffloadWriteThreads: 4,
	}), "--max-model-len", "8192"}
	cases = append(cases,
		kvGoldenCase{name: "template_disk_residual", vendor: "nvidia", templateArgs: residual},
		kvGoldenCase{name: "template_disk_residual_unset_all", vendor: "nvidia", templateArgs: residual, unset: []string{
			"kv_offloading_size", "kv_disk_offload", "shared_kv_dir", "kv_offload_read_threads", "kv_offload_write_threads", "kv_disk_limit_gib",
		}},
		kvGoldenCase{name: "template_disk_residual_unset_disk_keys", vendor: "nvidia", templateArgs: residual, unset: []string{
			"kv_disk_offload", "shared_kv_dir", "kv_offload_read_threads", "kv_offload_write_threads",
		}},
		kvGoldenCase{name: "template_disk_residual_unset_dir", vendor: "nvidia", templateArgs: residual, unset: []string{"shared_kv_dir"}},
	)
	// A request naming a KV offload backend: master has no such key.
	withBackend := cloneRaw(disk)
	withBackend["kv_offload_backend"] = "lmcache"
	cases = append(cases, kvGoldenCase{name: "request_kv_offload_backend", vendor: "nvidia", raw: withBackend})
	return cases
}

func jsonString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// TestKVCPUBytesEncodeAsInteger pins the wire form of cpu_bytes_to_use: vLLM
// reads it as an integer, and a float spelling (8.589934592e+09) is refused.
func TestKVCPUBytesEncodeAsInteger(t *testing.T) {
	disk := diskKVTransferConfig(LaunchConfig{KVOffloadingSize: 8, KVDiskOffload: true, SharedKVDir: "/kv-cache"})
	if !strings.Contains(disk, `"cpu_bytes_to_use":8589934592`) {
		t.Fatalf("disk tier config = %s", disk)
	}
	launch := BuildVLLM(pdParams(pdmode.RolePrefill, LaunchConfig{KVOffloadingSize: 8}, ComputeNetwork{}))
	if config := argValue(t, launch.Argv, "--kv-transfer-config"); !strings.Contains(config, `"cpu_bytes_to_use":8589934592`) {
		t.Fatalf("PD prefill config = %s", config)
	}
}
