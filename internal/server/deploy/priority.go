package deploy

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Per-request priority scheduling: the engine orders its waiting queue (and,
// under KV pressure or on SGLang a much higher arrival, chooses whom to
// preempt) by a request's "priority" field. The gateway is what writes that
// field; clients never reach it (docs/explanation/request-priority.md).
//
// vLLM: lower value runs first, --scheduling-policy priority turns it on.
// SGLang: higher value runs first by default; --default-priority-value 0
// keeps requests without the field (health checks, the engine's own warmup)
// at the neutral level instead of last, and is required for the low-first
// direction, which SGLang 0.5.21 crashes at startup without. The platform
// never uses low-first.
const (
	vllmSchedulingPolicyFlag       = "--scheduling-policy"
	vllmSchedulingPolicyPriority   = "priority"
	sglangPrioritySchedulingFlag   = "--enable-priority-scheduling"
	sglangDefaultPriorityFlag      = "--default-priority-value"
	sglangSchedulePolicyFlag       = "--schedule-policy"
	sglangLowPriorityFirstFlag     = "--schedule-low-priority-values-first"
	sglangPriorityCompatiblePolicy = "fcfs"
)

// sglangPriorityPolicies are the schedule policies SGLang accepts together
// with priority scheduling; any other fails an assertion at startup.
var sglangPriorityPolicies = map[string]bool{"fcfs": true, "lof": true}

// prioritySchedulingVerified says whether engine's build for vendor was
// checked to run priority scheduling with the semantics above: NVIDIA on a
// real card (vLLM 0.31, SGLang 0.5.21); AMD (same upstream schedulers) and
// Hygon's vLLM build by their source only. An empty vendor is a draft
// whose target is not chosen yet; the launch is checked again on its node.
func prioritySchedulingVerified(engine, vendor string) bool {
	switch vendor {
	case "", accelerator.VendorNVIDIA, accelerator.VendorAMD:
		return true
	case accelerator.VendorHygon:
		return engine == EngineVLLM
	}
	return false
}

// PriorityRefusal is why a launch cannot run with priority scheduling. Code
// is a messages.ErrorPriorityScheduling* code; FixArgs, when set, are user
// arguments that clear the refusal (they override the template's flag).
type PriorityRefusal struct {
	Code    string
	Params  map[string]any
	FixArgs []string
}

// PrioritySchedulingRefusal answers whether a launch of engine on vendor
// may turn on priority scheduling, judged on the arguments the engine will
// actually receive: the template's, overridden by the user's. Nil means it
// may.
func PrioritySchedulingRefusal(engine, vendor string, pd bool, templateArgs, userArgs []string) *PriorityRefusal {
	if engine != EngineVLLM && engine != EngineSGLang {
		return &PriorityRefusal{Code: messages.ErrorPrioritySchedulingEngineUnsupported, Params: map[string]any{"engine": engine}}
	}
	vendor = accelerator.NormalizeVendor(vendor)
	if !prioritySchedulingVerified(engine, vendor) {
		return &PriorityRefusal{Code: messages.ErrorPrioritySchedulingTargetUnverified, Params: map[string]any{"vendor": accelerator.VendorBrand(vendor)}}
	}
	if pd {
		return &PriorityRefusal{Code: messages.ErrorPrioritySchedulingPDUnsupported}
	}
	_, template := residualLayers(templateArgs, userArgs)
	args := append(append([]string(nil), template...), userArgs...)
	if engine == EngineVLLM {
		if value, ok := lastFlagValue(args, vllmSchedulingPolicyFlag); ok && value != vllmSchedulingPolicyPriority {
			return &PriorityRefusal{Code: messages.ErrorPrioritySchedulingPolicyConflict, Params: map[string]any{"flag": vllmSchedulingPolicyFlag, "value": value}}
		}
		return nil
	}
	if value, ok := lastFlagValue(args, sglangSchedulePolicyFlag); ok && !sglangPriorityPolicies[value] {
		refusal := &PriorityRefusal{Code: messages.ErrorPrioritySchedulingPolicyConflict, Params: map[string]any{"flag": sglangSchedulePolicyFlag, "value": value}}
		// The user's own flag is theirs to change; only a template's value
		// can be overridden by adding one.
		if _, userSet := lastFlagValue(userArgs, sglangSchedulePolicyFlag); !userSet {
			refusal.FixArgs = []string{sglangSchedulePolicyFlag, sglangPriorityCompatiblePolicy}
		}
		return refusal
	}
	if flagSet(args)[sglangLowPriorityFirstFlag] {
		return &PriorityRefusal{Code: messages.ErrorPrioritySchedulingPolicyConflict, Params: map[string]any{"flag": sglangLowPriorityFirstFlag}}
	}
	return nil
}

// injectPriorityScheduling turns the engine's priority scheduling on when the
// deployment asked for it and the launch allows it. A launch that no longer
// allows it (its arguments changed) runs without, never with a command the
// engine refuses.
func injectPriorityScheduling(b *builder, p Params) {
	if !p.PriorityScheduling || PrioritySchedulingRefusal(b.engine, p.Vendor, p.PD != nil, p.TemplateArgs, p.ExtraArgs) != nil {
		return
	}
	switch b.engine {
	case EngineVLLM:
		b.injectKV(vllmSchedulingPolicyFlag, vllmSchedulingPolicyPriority)
	case EngineSGLang:
		b.injectFlag(sglangPrioritySchedulingFlag)
		b.injectKV(sglangDefaultPriorityFlag, "0")
	}
}

// SpecRunsPriority reports whether an instance's stored launch (its desired
// state JSON, as json.Marshal writes it) starts the engine with priority
// scheduling on, whoever wrote the flag.
func SpecRunsPriority(specJSON string) bool {
	for _, marker := range []string{
		`"` + sglangPrioritySchedulingFlag + `"`,
		`"` + vllmSchedulingPolicyFlag + `","` + vllmSchedulingPolicyPriority + `"`,
		`"` + vllmSchedulingPolicyFlag + `=` + vllmSchedulingPolicyPriority + `"`,
	} {
		if strings.Contains(specJSON, marker) {
			return true
		}
	}
	return false
}

// PriorityFieldValue is the request "priority" value that gives a class of
// value classValue (larger is more important) on engine, as the platform
// launches it: vLLM handles lower values first, SGLang higher ones. ok is
// false for an engine without priority scheduling.
func PriorityFieldValue(engine string, classValue int) (int, bool) {
	switch engine {
	case EngineVLLM:
		return -classValue, true
	case EngineSGLang:
		return classValue, true
	}
	return 0, false
}

// lastFlagValue is the value the last occurrence of flag gives in args, in
// either "--flag value" or "--flag=value" form; argparse keeps the last.
func lastFlagValue(args []string, flag string) (string, bool) {
	value, found := "", false
	for index := 0; index < len(args); index++ {
		name, raw, inline := strings.Cut(args[index], "=")
		if name != flag {
			continue
		}
		if inline {
			value, found = raw, true
			continue
		}
		if index+1 < len(args) {
			index++
			value, found = args[index], true
		}
	}
	return strings.TrimSpace(value), found
}
