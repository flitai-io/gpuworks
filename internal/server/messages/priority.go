package messages

// Product error codes of per-request priority scheduling: why a deployment
// cannot run its engine with priority scheduling (deploy.PrioritySchedulingRefusal).
const (
	// ErrorPrioritySchedulingEngineUnsupported: the engine ({engine}) has no
	// per-request priority scheduling the platform drives.
	ErrorPrioritySchedulingEngineUnsupported = "PRIORITY_SCHEDULING_ENGINE_UNSUPPORTED"
	// ErrorPrioritySchedulingTargetUnverified: the engine's build for the
	// accelerator ({vendor}) has not been verified with priority scheduling.
	ErrorPrioritySchedulingTargetUnverified = "PRIORITY_SCHEDULING_TARGET_UNVERIFIED"
	// ErrorPrioritySchedulingPDUnsupported: PD disaggregated deployments do
	// not run priority scheduling yet.
	ErrorPrioritySchedulingPDUnsupported = "PRIORITY_SCHEDULING_PD_UNSUPPORTED"
	// ErrorPrioritySchedulingPolicyConflict: the launch arguments set a
	// scheduling option ({flag}, with {value} when it takes one) that
	// priority scheduling cannot run with.
	ErrorPrioritySchedulingPolicyConflict = "PRIORITY_SCHEDULING_POLICY_CONFLICT"
)

// PrioritySchedulingRefusalFallback is the English wire text for a refusal;
// the UI renders the code.
func PrioritySchedulingRefusalFallback(code string) string {
	switch code {
	case ErrorPrioritySchedulingEngineUnsupported:
		return "the engine does not support per-request priority scheduling"
	case ErrorPrioritySchedulingTargetUnverified:
		return "priority scheduling has not been verified on this accelerator"
	case ErrorPrioritySchedulingPDUnsupported:
		return "PD disaggregated deployments do not support priority scheduling yet"
	case ErrorPrioritySchedulingPolicyConflict:
		return "the launch arguments set a scheduling option priority scheduling cannot run with"
	}
	return "priority scheduling is unavailable"
}

// Product error codes of request priority classes and key bindings
// (internal/server/api/priority_classes.go).
const (
	// ErrorPriorityClassInvalid: a class name or value out of bounds
	// ({field}), or a binding naming no class.
	ErrorPriorityClassInvalid = "PRIORITY_CLASS_INVALID"
	// ErrorPriorityClassExists: a class with the name already exists.
	ErrorPriorityClassExists = "PRIORITY_CLASS_EXISTS"
	// ErrorPriorityClassBuiltin: a built-in class keeps its name and value.
	ErrorPriorityClassBuiltin = "PRIORITY_CLASS_BUILTIN"
	// ErrorPriorityClassInUse: the class is the default, bound to a live key,
	// or what a batch still sending requests or an evaluation still running
	// runs at.
	ErrorPriorityClassInUse = "PRIORITY_CLASS_IN_USE"
	// ErrorPriorityClassNotFound: no class has the name.
	ErrorPriorityClassNotFound = "PRIORITY_CLASS_NOT_FOUND"
	// ErrorPriorityPolicyInvalid: a key policy that is none of the known ones ({policy}).
	ErrorPriorityPolicyInvalid = "PRIORITY_POLICY_INVALID"
)
