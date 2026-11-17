package deploy

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

func productCode(err error) (string, map[string]any) {
	var product *messages.ProductError
	if errors.As(err, &product) {
		return product.Code(), product.Params()
	}
	return "", nil
}

func wantCode(t *testing.T, err error, code string) map[string]any {
	t.Helper()
	got, params := productCode(err)
	if got != code {
		t.Fatalf("error = %v (code %q), want %s", err, got, code)
	}
	return params
}

func kvTestParams(quick LaunchConfig, vendor string) Params {
	return Params{
		Vendor: vendor, ModelPath: "/model", ServedName: "qwen", APIPort: 30000,
		AssignedGPUs: 1, TensorParallel: 1, NNodes: 1, Quick: quick,
	}
}

func resolveKVTest(p Params, mode TargetValidationMode) (ResolvedLaunch, error) {
	return ResolveLaunch(ResolveLaunchInput{Engine: vLLMEngine{}, Params: p, TargetValidation: mode, ModelWeightBytes: 16 << 30})
}

func selectTestExternal() LaunchConfig {
	return LaunchConfig{KVOffloadBackend: testExternalKVBackend}
}

// While only the native row ships, nothing publishes the selector and every
// stated selection is dropped, as it always was.
func TestKVBackendSelectorUnpublishedWithoutSelectableRows(t *testing.T) {
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		if _, published := engineParams(engine)[paramKVOffloadBackend]; published {
			t.Fatalf("%s publishes %s with only the native row", engine, paramKVOffloadBackend)
		}
		profile, err := ProfileForEngine(engine)
		if err != nil {
			t.Fatal(err)
		}
		if profile.KVBackends != nil {
			t.Fatalf("%s profile lists kv_backends %+v", engine, profile.KVBackends)
		}
		raw, _ := json.Marshal(profile)
		if strings.Contains(string(raw), "kv_backends") || strings.Contains(string(raw), paramKVOffloadBackend) {
			t.Fatalf("%s profile wire mentions the backend seam", engine)
		}
	}
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{paramKVOffloadBackend: testExternalKVBackend}})
	if err != nil || out.Quick.KVOffloadBackend != "" {
		t.Fatalf("unpublished selection kept: %+v, %v", out.Quick, err)
	}
}

func TestKVBackendSelectorAndProfileWire(t *testing.T) {
	withKVBackends(t)
	profile, err := ProfileForEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	if _, inDict := profile.ParamDict[paramKVOffloadBackend]; inDict {
		t.Fatal("the selector has no flag, so param_dict must not list it")
	}
	var selector *ParamDefinition
	for i := range profile.ParamDefinitions {
		if profile.ParamDefinitions[i].Key == paramKVOffloadBackend {
			selector = &profile.ParamDefinitions[i]
		}
	}
	if selector == nil {
		t.Fatal("param_specs lacks the selector")
	}
	raw, _ := json.Marshal(selector)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["flag"] != "" || wire["type"] != "enum" || wire["control"] != "select" || wire["default"] != nil || !slices.Equal(selector.Enum, []string{testExternalKVBackend}) {
		t.Fatalf("selector wire = %s", raw)
	}
	if label := selector.Options[0].Label; label["en"] != "Test External" || label["zh"] != "Test External" {
		t.Fatalf("option label = %v", label)
	}
	backends, _ := json.Marshal(profile.KVBackends)
	want := `[{"id":"testext","label":"Test External","replaces":["kv_offloading_size","kv_disk_offload","shared_kv_dir","kv_offload_read_threads","kv_offload_write_threads","kv_disk_limit_gib","kv_offload_block_size"],"pd_roles":["prefill"]}]`
	if string(backends) != want {
		t.Fatalf("kv_backends = %s, want %s", backends, want)
	}
	// The card list and the launch gate read the same predicate.
	for vendor, listed := range map[string]bool{"nvidia": true, "NVIDIA": true, "ascend": false, "": true} {
		target, err := ProfileForTarget(EngineVLLM, vendor)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(target.KVBackends) == 1; got != listed {
			t.Errorf("ProfileForTarget(%q) lists the backend = %v, want %v", vendor, got, listed)
		}
	}
	sglang, _ := ProfileForEngine(EngineSGLang)
	if sglang.KVBackends != nil {
		t.Fatal("SGLang has no backend rows")
	}
	if _, composite := flagIndex(EngineVLLM)["--kv-transfer-config"]; !composite {
		t.Fatal("--kv-transfer-config must stay a composite flag")
	}
	for _, entry := range flagIndex(EngineVLLM)["--kv-transfer-config"] {
		if entry.spec.Key == paramKVOffloadBackend {
			t.Fatal("the selector joined the --kv-transfer-config composite group")
		}
	}
}

func TestKVBackendSelectionValidation(t *testing.T) {
	withKVBackends(t)
	if err := ValidateLaunchConfigForEngine(EngineVLLM, selectTestExternal()); err != nil {
		t.Fatalf("plain selection refused: %v", err)
	}
	for _, id := range []string{"missing", kvbackend.Native} {
		params := wantCode(t, ValidateLaunchConfigForEngine(EngineVLLM, LaunchConfig{KVOffloadBackend: id}), messages.ErrorKVOffloadBackendUnknown)
		if params["backend"] != id || params["engine"] != EngineVLLM || params["key"] != paramKVOffloadBackend {
			t.Fatalf("params = %v", params)
		}
	}
	wantCode(t, ValidateLaunchConfigForEngine(EngineSGLang, selectTestExternal()), messages.ErrorKVOffloadBackendUnknown)
	// Exclusivity is judged on effective values, one replaced key at a time.
	for key, edit := range map[string]func(*LaunchConfig){
		"kv_offloading_size":       func(q *LaunchConfig) { q.KVOffloadingSize = 4 },
		"kv_disk_offload":          func(q *LaunchConfig) { q.KVDiskOffload = true },
		"shared_kv_dir":            func(q *LaunchConfig) { q.SharedKVDir = "/kv" },
		"kv_disk_limit_gib":        func(q *LaunchConfig) { q.KVDiskLimitGiB = 8 },
		"kv_offload_read_threads":  func(q *LaunchConfig) { q.KVDiskOffload, q.KVOffloadReadThreads = true, 4 },
		"kv_offload_write_threads": func(q *LaunchConfig) { q.KVOffloadWriteThreads = 4; q.KVDiskOffload = true },
		"kv_offload_block_size":    func(q *LaunchConfig) { q.KVOffloadBlockSize = 128 },
	} {
		q := selectTestExternal()
		edit(&q)
		params := wantCode(t, validateKVBackend(EngineVLLM, q), messages.ErrorKVOffloadBackendConflictsNativeTier)
		if key != "kv_offload_read_threads" && key != "kv_offload_write_threads" && params["key"] != key {
			t.Errorf("%s: params = %v", key, params)
		}
	}
	// Values the platform states by default change nothing.
	q := selectTestExternal()
	q.KVOffloadReadThreads, q.KVOffloadWriteThreads = 16, 16
	if err := validateKVBackend(EngineVLLM, q); err != nil {
		t.Fatalf("thread defaults of a disk tier that is off refused: %v", err)
	}
	// A new request gets the dictionary defaults (kv_disk_offload false) and
	// still validates.
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{paramKVOffloadBackend: testExternalKVBackend}})
	if err != nil || out.Quick.KVOffloadBackend != testExternalKVBackend {
		t.Fatalf("materialize: %+v, %v", out.Quick, err)
	}
	if err := ValidateLaunchConfigForEngine(EngineVLLM, out.Quick); err != nil {
		t.Fatalf("materialized selection refused: %v", err)
	}
	// An unknown id is the caller's to correct, not a decode failure.
	out, err = Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{paramKVOffloadBackend: "missing"}})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, ValidateLaunchConfigForEngine(EngineVLLM, out.Quick), messages.ErrorKVOffloadBackendUnknown)
}

// Switching a stored native row to the backend without tombstoning its tiers
// is refused; with the tombstones the form sends it is accepted.
func TestKVBackendEditOfStoredNativeRow(t *testing.T) {
	withKVBackends(t)
	stored := LaunchConfig{KVOffloadingSize: 16, KVDiskOffload: true, SharedKVDir: "/kv-cache"}
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Base: &stored, Raw: map[string]any{paramKVOffloadBackend: testExternalKVBackend}})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, ValidateLaunchConfigForEngine(EngineVLLM, out.Quick), messages.ErrorKVOffloadBackendConflictsNativeTier)
	out, err = Materialize(MaterializeInput{Engine: EngineVLLM, Base: &stored, Raw: map[string]any{paramKVOffloadBackend: testExternalKVBackend}, Unset: testExternalKVBackendKeys})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLaunchConfigForEngine(EngineVLLM, out.Quick); err != nil {
		t.Fatalf("switch with tombstones refused: %v (%+v)", err, out.Quick)
	}
	// And back: tombstoning the selection restores the engine's own offload.
	selected := out.Quick
	back, err := Materialize(MaterializeInput{Engine: EngineVLLM, Base: &selected, Raw: map[string]any{"kv_offloading_size": 8}, Unset: []string{paramKVOffloadBackend}})
	if err != nil || back.Quick.KVOffloadBackend != "" || back.Quick.KVOffloadingSize != 8 {
		t.Fatalf("switch back: %+v, %v", back.Quick, err)
	}
}

// A stored row naming a backend the server does not know still decodes and is
// refused, with the code, on every path that runs it.
func TestKVBackendStoredUnknownIDRefusedOnLaunch(t *testing.T) {
	stored := []byte(`{"kv_offload_backend":"retired"}`)
	var q LaunchConfig
	if err := json.Unmarshal(stored, &q); err != nil || q.KVOffloadBackend != "retired" {
		t.Fatalf("decode: %+v, %v", q, err)
	}
	wantCode(t, ValidateLaunchConfigForEngine(EngineVLLM, q), messages.ErrorKVOffloadBackendUnknown)
	for _, mode := range []TargetValidationMode{ValidateTargetCapabilities, ValidateAssignedTarget, CollectTargetCapabilities} {
		_, err := resolveKVTest(kvTestParams(q, "nvidia"), mode)
		wantCode(t, err, messages.ErrorKVOffloadBackendUnknown)
	}
}

func TestKVBackendArgConflicts(t *testing.T) {
	withKVBackends(t)
	for name, layer := range map[string][]string{
		"hyphen":            {"--kv-transfer-config", "{}"},
		"underscore":        {"--kv_transfer_config", "{}"},
		"equals":            {"--kv-transfer-config={}"},
		"quoted":            {"'--kv-transfer-config={}'"},
		"cpu tier flag":     {"--kv_offloading_backend", "native"},
		"underscore equals": {"--kv_transfer_config={}"},
	} {
		for _, where := range []string{"extra", "template"} {
			p := kvTestParams(selectTestExternal(), "nvidia")
			if where == "extra" {
				p.ExtraArgs = layer
			} else {
				p.TemplateArgs = layer
			}
			_, err := resolveKVTest(p, ValidateAssignedTarget)
			params := wantCode(t, err, messages.ErrorKVOffloadBackendConflictsArg)
			if params["backend"] != testExternalKVBackend || params["flag"] == "" {
				t.Errorf("%s/%s: params = %v", name, where, params)
			}
		}
	}
	// Under PD the prefill keeps the backend inside the composed
	// MultiConnector; a layer spelling the flag the parser's other way would
	// replace it as surely as the hyphen spelling PD_CONNECTOR_ARG_CONFLICT
	// matches, so the backend's own normalized check runs there too.
	for _, layer := range [][]string{{"--kv_transfer_config", `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`}, {"--kv_offloading_size", "4"}} {
		p := kvTestParams(PDRoleLaunchConfig(EngineVLLM, selectTestExternal(), pdmode.RolePrefill), "nvidia")
		p.PD = &PDLaunch{Mode: "vllm-nixl", Role: pdmode.RolePrefill, SideChannelHost: "10.0.0.5", SideChannelPort: 30001, RoleTP: map[string]int{pdmode.RolePrefill: 1, pdmode.RoleDecode: 1}}
		p.ExtraArgs = layer
		_, err := resolveKVTest(p, ValidateAssignedTarget)
		wantCode(t, err, messages.ErrorKVOffloadBackendConflictsArg)
	}
	if got := matchConflictFlags([]string{"--kv-transfer-configs", "kv-transfer-config", "--model"}, []string{"--kv-transfer-config"}); len(got) != 0 {
		t.Fatalf("near misses matched: %v", got)
	}
	// The engine's own offload keeps today's behaviour: no refusal, W4 stays.
	p := kvTestParams(LaunchConfig{}, "nvidia")
	p.ExtraArgs = []string{"--kv-transfer-config", "{}"}
	if _, err := resolveKVTest(p, ValidateAssignedTarget); err != nil {
		t.Fatalf("native launch with a hand-written connector refused: %v", err)
	}
}

func TestKVBackendTargetAndTopologyGates(t *testing.T) {
	withKVBackends(t)
	for _, mode := range []TargetValidationMode{ValidateTargetCapabilities, ValidateAssignedTarget} {
		p := kvTestParams(selectTestExternal(), "ascend")
		_, err := resolveKVTest(p, mode)
		if params := wantCode(t, err, messages.ErrorKVOffloadBackendUnsupportedOnTarget); params["vendor"] != "Ascend" {
			t.Errorf("mode %d: params = %v", mode, params)
		}
		for name, edit := range map[string]func(*Params){
			"nnodes": func(p *Params) {
				p.NNodes, p.TensorParallel, p.MasterAddr, p.MasterPort = 2, 2, "192.0.2.10", DefaultMasterPort
			},
			"pp": func(p *Params) { p.PipelineParallel, p.AssignedGPUs = 2, 2 },
			"dp": func(p *Params) { p.Quick.DP, p.AssignedGPUs = 2, 2 },
		} {
			p := kvTestParams(selectTestExternal(), "nvidia")
			edit(&p)
			_, err := resolveKVTest(p, mode)
			wantCode(t, err, messages.ErrorKVOffloadBackendTopologyUnsupported)
			_ = name
		}
		if _, err := resolveKVTest(kvTestParams(selectTestExternal(), "nvidia"), mode); err != nil {
			t.Fatalf("mode %d: supported target refused: %v", mode, err)
		}
	}
	// An unknown vendor (evaluate with no node, automatic placement) is not
	// yet a target: the profile lists the card, so the zero mode allows it,
	// and the assigned target is held to the vendor list after placement.
	if _, err := resolveKVTest(kvTestParams(selectTestExternal(), ""), ValidateTargetCapabilities); err != nil {
		t.Fatalf("unknown vendor refused before placement: %v", err)
	}
	if !slices.ContainsFunc(kvBackendProfiles(EngineVLLM, ""), func(p KVBackendProfile) bool { return p.ID == testExternalKVBackend }) {
		t.Fatal("the profile does not list the row for an unknown vendor")
	}
	if _, err := resolveKVTest(kvTestParams(selectTestExternal(), ""), ValidateAssignedTarget); err != nil {
		t.Fatalf("assigned target with no vendor fact refused: %v", err)
	}
	// Collection only gathers what is requested; the assigned target is
	// validated after placement.
	launch, err := resolveKVTest(kvTestParams(selectTestExternal(), "ascend"), CollectTargetCapabilities)
	if err != nil || !launch.RequestedFeatures.ExternalKVCache {
		t.Fatalf("collect: %+v, %v", launch.RequestedFeatures, err)
	}
	// The direct entry the edit path uses (ValidateTargetLaunch) refuses too.
	p := kvTestParams(selectTestExternal(), "ascend")
	launch, err = resolveKVTest(kvTestParams(selectTestExternal(), "nvidia"), CollectTargetCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, ValidateTargetLaunch(EngineVLLM, launch, p), messages.ErrorKVOffloadBackendUnsupportedOnTarget)
}

func TestKVBackendLaunchShape(t *testing.T) {
	withKVBackends(t)
	launch, err := resolveKVTest(kvTestParams(selectTestExternal(), "nvidia"), ValidateAssignedTarget)
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(t, launch.Argv, "--kv-transfer-config"); got != testExternalKVTransfer {
		t.Fatalf("--kv-transfer-config = %s", got)
	}
	if slices.Contains(launch.Argv, "--kv-offloading-size") || slices.Contains(launch.Argv, "--kv-offloading-backend") {
		t.Fatalf("native tier flags leaked: %v", launch.Argv)
	}
	features := launch.RequestedFeatures
	if !features.CommandKnown || features.PD || features.KVDiskTier || !features.ExternalKVCache || !slices.Contains(features.Names(), "external_kv_cache") {
		t.Fatalf("requested features = %+v", features)
	}
	native, err := resolveKVTest(kvTestParams(LaunchConfig{}, "nvidia"), ValidateAssignedTarget)
	if err != nil {
		t.Fatal(err)
	}
	if launch.ShmBytes != native.ShmBytes {
		t.Fatalf("shm = %d, want the engine default %d", launch.ShmBytes, native.ShmBytes)
	}
	for _, mount := range launch.Mounts {
		if mount.Purpose == "kv-disk" || strings.Contains(mount.Target, "kv") {
			t.Fatalf("unexpected KV mount %+v", mount)
		}
	}
}

func TestKVBackendPDRoleScopeAndComposition(t *testing.T) {
	withKVBackends(t)
	q := selectTestExternal()
	if got := PDRoleLaunchConfig(EngineVLLM, q, pdmode.RoleDecode); got.KVOffloadBackend != "" {
		t.Fatalf("decode kept the backend: %+v", got)
	}
	prefill := PDRoleLaunchConfig(EngineVLLM, q, pdmode.RolePrefill)
	if prefill.KVOffloadBackend != testExternalKVBackend {
		t.Fatalf("prefill dropped the backend: %+v", prefill)
	}
	for role, quick := range map[string]LaunchConfig{pdmode.RolePrefill: prefill, pdmode.RoleDecode: PDRoleLaunchConfig(EngineVLLM, q, pdmode.RoleDecode)} {
		p := kvTestParams(quick, "nvidia")
		p.PD = &PDLaunch{Mode: "vllm-nixl", Role: role, SideChannelHost: "10.0.0.5", SideChannelPort: 30001, RoleTP: map[string]int{pdmode.RolePrefill: 1, pdmode.RoleDecode: 1}}
		launch, err := resolveKVTest(p, ValidateAssignedTarget)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		var config struct {
			Connector string `json:"kv_connector"`
			Role      string `json:"kv_role"`
			Extra     struct {
				Connectors []map[string]any `json:"connectors"`
			} `json:"kv_connector_extra_config"`
		}
		if err := json.Unmarshal([]byte(argValue(t, launch.Argv, "--kv-transfer-config")), &config); err != nil {
			t.Fatal(err)
		}
		switch role {
		case pdmode.RolePrefill:
			if config.Connector != "MultiConnector" || len(config.Extra.Connectors) != 2 || config.Extra.Connectors[0]["kv_connector"] != "NixlConnector" || config.Extra.Connectors[1]["kv_connector_extra_config"].(map[string]any)["spec_name"] != "TestExternalSpec" || config.Role != "kv_producer" {
				t.Fatalf("prefill config = %+v", config)
			}
			// The backend shaped its own part and the wrapper from the role:
			// the composer only assembles.
			outer := map[string]any{}
			_ = json.Unmarshal([]byte(argValue(t, launch.Argv, "--kv-transfer-config")), &outer)
			if outer["kv_connector_module_path"] != testExternalWrapperModule || config.Extra.Connectors[1]["kv_role"] != "kv_producer" {
				t.Fatalf("prefill wrapper = %v, inner = %v", outer, config.Extra.Connectors[1])
			}
			if !launch.RequestedFeatures.PD || !launch.RequestedFeatures.ExternalKVCache {
				t.Fatalf("prefill features = %+v", launch.RequestedFeatures)
			}
		case pdmode.RoleDecode:
			if config.Connector != "NixlConnector" || launch.RequestedFeatures.ExternalKVCache {
				t.Fatalf("decode config = %+v, features %+v", config, launch.RequestedFeatures)
			}
		}
	}
}

// A template saved from a deployment selects the backend again; the round
// trip goes through the recognizer, not an opaque residual.
func TestKVBackendTemplateRoundTrip(t *testing.T) {
	withKVBackends(t)
	exported, err := ExportLaunchArgs(EngineVLLM, selectTestExternal(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(exported.Args, testExternalKVTransfer) || slices.Contains(exported.Dropped, paramKVOffloadBackend) {
		t.Fatalf("exported = %+v", exported)
	}
	params, residual, err := ReverseArgs(EngineVLLM, exported.Args)
	if err != nil || params[paramKVOffloadBackend] != testExternalKVBackend || slices.Contains(residual, "--kv-transfer-config") {
		t.Fatalf("reverse = %v, %v, %v", params, residual, err)
	}
	for _, args := range [][]string{{"--kv-transfer-config=" + testExternalKVTransfer}, {"--kv-transfer-config", "'" + testExternalKVTransfer + "'"}} {
		params, _, _ := ReverseArgs(EngineVLLM, args)
		if params[paramKVOffloadBackend] != testExternalKVBackend {
			t.Fatalf("%v read as %v", args, params)
		}
	}
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: exported.Args})
	if err != nil || out.Quick.KVOffloadBackend != testExternalKVBackend || len(out.TemplateResidual) != 0 {
		t.Fatalf("materialize template = %+v, %v", out, err)
	}
	// The form's native card tombstones the selection, which removes the
	// template's object with it.
	out, err = Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: exported.Args, Unset: []string{paramKVOffloadBackend}})
	if err != nil || out.Quick.KVOffloadBackend != "" || len(out.TemplateResidual) != 0 {
		t.Fatalf("tombstoned template = %+v, %v", out, err)
	}
	// The backend card tombstones the replaced keys, which unsets the
	// composite flag the template's object rides on: it may be dropped.
	if !(vLLMEngine{}).CanUnsetTemplateFlag("--kv-transfer-config", testExternalKVTransfer) {
		t.Fatal("the backend's own object is not unsettable")
	}
	out, err = Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: exported.Args, Raw: map[string]any{paramKVOffloadBackend: testExternalKVBackend}, Unset: testExternalKVBackendKeys})
	if err != nil || out.Quick.KVOffloadBackend != testExternalKVBackend {
		t.Fatalf("backend card over its own template = %+v, %v", out, err)
	}
	// A PD-wrapped object stays opaque: no selection, and unsetting the
	// native keys over it is still an explicit conflict.
	wrapped := `{"kv_connector":"MultiConnector","kv_connector_extra_config":{"connectors":[{"kv_connector":"NixlConnector","kv_role":"kv_both"},` + testExternalKVTransfer + `]},"kv_role":"kv_both"}`
	params, residual, _ = ReverseArgs(EngineVLLM, []string{"--kv-transfer-config", wrapped})
	if _, selected := params[paramKVOffloadBackend]; selected || !slices.Contains(residual, wrapped) {
		t.Fatalf("wrapped object read as %v / %v", params, residual)
	}
	_, err = Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: []string{"--kv-transfer-config", wrapped}, Unset: []string{"kv_disk_offload", "shared_kv_dir", "kv_offload_read_threads", "kv_offload_write_threads"}})
	wantCode(t, err, ErrorLaunchConfigUnsetConflict)
}

// A row written by this server, read by a server that predates the field,
// loses the selection: the downgrade the ADR documents.
func TestKVBackendDowngradeDropsTheSelection(t *testing.T) {
	type masterLaunchConfig struct {
		KVOffloadingSize float64 `json:"kv_offloading_size,omitempty"`
	}
	var old masterLaunchConfig
	if err := json.Unmarshal([]byte(`{"kv_offload_backend":"testext","kv_offloading_size":0}`), &old); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(old)
	if string(again) != `{}` {
		t.Fatalf("re-marshalled by an old server: %s", again)
	}
}
