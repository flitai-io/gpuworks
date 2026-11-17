package deploy

import (
	"encoding/json"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func priorityArgv(r ResolvedLaunch) []string {
	return append(append([]string(nil), r.Entrypoint...), r.Argv...)
}

func TestPrioritySchedulingRefusal(t *testing.T) {
	cases := []struct {
		name           string
		engine, vendor string
		pd             bool
		template, user []string
		code           string
		fix            []string
		flag, value    string
	}{
		{name: "vllm nvidia", engine: EngineVLLM, vendor: "nvidia"},
		{name: "sglang nvidia", engine: EngineSGLang, vendor: "nvidia"},
		{name: "draft without a target", engine: EngineSGLang, vendor: ""},
		{name: "vllm amd", engine: EngineVLLM, vendor: "amd"},
		{name: "vllm hygon", engine: EngineVLLM, vendor: "hygon"},
		{name: "sglang hygon unverified", engine: EngineSGLang, vendor: "hygon", code: messages.ErrorPrioritySchedulingTargetUnverified},
		{name: "vllm ascend unverified", engine: EngineVLLM, vendor: "ascend", code: messages.ErrorPrioritySchedulingTargetUnverified},
		{name: "other engine", engine: "llamacpp", vendor: "nvidia", code: messages.ErrorPrioritySchedulingEngineUnsupported},
		{name: "pd", engine: EngineVLLM, vendor: "nvidia", pd: true, code: messages.ErrorPrioritySchedulingPDUnsupported},
		{name: "vllm user fcfs", engine: EngineVLLM, vendor: "nvidia", user: []string{"--scheduling-policy", "fcfs"},
			code: messages.ErrorPrioritySchedulingPolicyConflict, flag: "--scheduling-policy", value: "fcfs"},
		{name: "vllm user already priority", engine: EngineVLLM, vendor: "nvidia", user: []string{"--scheduling-policy=priority"}},
		{name: "sglang template lpm offers a fix", engine: EngineSGLang, vendor: "amd", template: []string{"--tp-size", "8", "--schedule-policy", "lpm"},
			code: messages.ErrorPrioritySchedulingPolicyConflict, fix: []string{"--schedule-policy", "fcfs"}, flag: "--schedule-policy", value: "lpm"},
		{name: "sglang inline template lpm", engine: EngineSGLang, vendor: "nvidia", template: []string{"--schedule-policy=lpm"},
			code: messages.ErrorPrioritySchedulingPolicyConflict, fix: []string{"--schedule-policy", "fcfs"}, flag: "--schedule-policy", value: "lpm"},
		{name: "sglang user lpm is the user's to change", engine: EngineSGLang, vendor: "nvidia", user: []string{"--schedule-policy", "lpm"},
			code: messages.ErrorPrioritySchedulingPolicyConflict, flag: "--schedule-policy", value: "lpm"},
		{name: "sglang user fcfs overrides template lpm", engine: EngineSGLang, vendor: "nvidia", template: []string{"--schedule-policy", "lpm"}, user: []string{"--schedule-policy", "fcfs"}},
		{name: "sglang lof is compatible", engine: EngineSGLang, vendor: "nvidia", template: []string{"--schedule-policy", "lof"}},
		{name: "sglang low-first", engine: EngineSGLang, vendor: "nvidia", user: []string{"--schedule-low-priority-values-first"},
			code: messages.ErrorPrioritySchedulingPolicyConflict, flag: "--schedule-low-priority-values-first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PrioritySchedulingRefusal(tc.engine, tc.vendor, tc.pd, tc.template, tc.user)
			if tc.code == "" {
				if got != nil {
					t.Fatalf("refusal = %+v, want none", got)
				}
				return
			}
			if got == nil || got.Code != tc.code {
				t.Fatalf("refusal = %+v, want %s", got, tc.code)
			}
			if !slices.Equal(got.FixArgs, tc.fix) {
				t.Fatalf("fix = %v, want %v", got.FixArgs, tc.fix)
			}
			if tc.flag != "" && (got.Params["flag"] != tc.flag || (tc.value != "" && got.Params["value"] != tc.value)) {
				t.Fatalf("params = %v", got.Params)
			}
		})
	}
}

func TestPrioritySchedulingInjection(t *testing.T) {
	sglang := sglangEngine{}
	vllm := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", PriorityScheduling: true})
	if argv := priorityArgv(vllm); !argvHas(argv, "--scheduling-policy", "priority") || countFlag(argv, "--scheduling-policy") != 1 {
		t.Fatalf("vllm argv = %v", argv)
	}
	sg := sglang.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, Vendor: "nvidia", PriorityScheduling: true})
	if argv := priorityArgv(sg); countFlag(argv, "--enable-priority-scheduling") != 1 || !argvHas(argv, "--default-priority-value", "0") ||
		countFlag(argv, "--schedule-low-priority-values-first") != 0 {
		t.Fatalf("sglang argv = %v", argv)
	}

	// Off, nothing is injected.
	for _, r := range []ResolvedLaunch{
		BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia"}),
		sglang.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, Vendor: "nvidia"}),
	} {
		argv := priorityArgv(r)
		if countFlag(argv, "--scheduling-policy") != 0 || countFlag(argv, "--enable-priority-scheduling") != 0 || countFlag(argv, "--default-priority-value") != 0 {
			t.Fatalf("argv without priority scheduling = %v", argv)
		}
	}

	// A launch that no longer allows it runs without it rather than with a
	// command the engine refuses at startup.
	conflict := sglang.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, Vendor: "amd", PriorityScheduling: true,
		TemplateArgs: []string{"--schedule-policy", "lpm"}})
	if argv := priorityArgv(conflict); countFlag(argv, "--enable-priority-scheduling") != 0 || !argvHas(argv, "--schedule-policy", "lpm") {
		t.Fatalf("conflicting argv = %v", argv)
	}
	unverified := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "ascend", PriorityScheduling: true})
	if argv := priorityArgv(unverified); countFlag(argv, "--scheduling-policy") != 0 {
		t.Fatalf("unverified target argv = %v", argv)
	}
	pd := sglang.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, Vendor: "nvidia", PriorityScheduling: true,
		PD: &PDLaunch{Mode: "sglang-nixl", Role: "decode"}})
	if argv := priorityArgv(pd); countFlag(argv, "--enable-priority-scheduling") != 0 {
		t.Fatalf("pd argv = %v", argv)
	}

	// The user's override of the template's policy clears the conflict, and
	// the user's own values are never duplicated.
	fixed := sglang.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, Vendor: "amd", PriorityScheduling: true,
		TemplateArgs: []string{"--schedule-policy", "lpm"}, ExtraArgs: []string{"--schedule-policy", "fcfs", "--default-priority-value", "5"}})
	argv := priorityArgv(fixed)
	if countFlag(argv, "--enable-priority-scheduling") != 1 || countFlag(argv, "--default-priority-value") != 1 || !argvHas(argv, "--default-priority-value", "5") ||
		countFlag(argv, "--schedule-policy") != 1 || !argvHas(argv, "--schedule-policy", "fcfs") {
		t.Fatalf("fixed argv = %v", argv)
	}
	stated := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", PriorityScheduling: true, ExtraArgs: []string{"--scheduling-policy", "priority"}})
	if argv := priorityArgv(stated); countFlag(argv, "--scheduling-policy") != 1 {
		t.Fatalf("stated argv = %v", argv)
	}
}

func TestPriorityFieldValue(t *testing.T) {
	if v, ok := PriorityFieldValue(EngineVLLM, 20); !ok || v != -20 {
		t.Fatalf("vllm = %d %v", v, ok)
	}
	if v, ok := PriorityFieldValue(EngineSGLang, 20); !ok || v != 20 {
		t.Fatalf("sglang = %d %v", v, ok)
	}
	if _, ok := PriorityFieldValue("llamacpp", 20); ok {
		t.Fatal("llamacpp has no priority")
	}
}

func TestSpecRunsPriority(t *testing.T) {
	for spec, want := range map[string]bool{
		`{"argv":["--scheduling-policy","priority"]}`:   true,
		`{"argv":["--scheduling-policy=priority"]}`:     true,
		`{"argv":["--enable-priority-scheduling"]}`:     true,
		`{"argv":["--scheduling-policy","fcfs"]}`:       false,
		`{"argv":["--schedule-policy","lpm"]}`:          false,
		`{"argv":["--prompt","--scheduling-policy x"]}`: false,
		``: false,
	} {
		if got := SpecRunsPriority(spec); got != want {
			t.Fatalf("SpecRunsPriority(%s) = %v", spec, got)
		}
	}
	// The flags the adapters inject are the ones it recognizes.
	vllm := BuildVLLM(Params{ModelPath: "/m", APIPort: 30000, Vendor: "nvidia", PriorityScheduling: true})
	sg := sglangEngine{}.Build(Params{ModelPath: "/m", APIPort: 30000, TensorParallel: 1, Vendor: "nvidia", PriorityScheduling: true})
	for _, r := range []ResolvedLaunch{vllm, sg} {
		raw, _ := json.Marshal(map[string]any{"argv": priorityArgv(r)})
		if !SpecRunsPriority(string(raw)) {
			t.Fatalf("injected argv not recognized: %s", raw)
		}
	}
}
