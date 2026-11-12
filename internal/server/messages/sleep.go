package messages

const (
	// ErrorSleepNotEnabled: a sleep order to a deployment that was not
	// created sleepable.
	ErrorSleepNotEnabled = "SLEEP_NOT_ENABLED"
	// ErrorSleepHostMemoryInsufficient: the node's host memory cannot hold the
	// weights an L1 sleep moves there.
	ErrorSleepHostMemoryInsufficient = "SLEEP_HOST_MEMORY_INSUFFICIENT"
)

// SleepRefusalFallback is the English wire text for a refused sleep order or
// sleep declaration; the UI renders the code.
func SleepRefusalFallback(code string) string {
	switch code {
	case ErrorSleepNotEnabled:
		return "sleep mode was not enabled for this deployment"
	case ErrorSleepEngineUnsupported:
		return "the deployment's engine does not support sleep mode"
	case ErrorInstanceNotServing:
		return "the instance fills no replica: it failed, is draining or is being removed"
	case ErrorSleepRoleCannotEmpty:
		return "the role cannot be left without an awake instance; sleep the whole deployment instead"
	case ErrorSleepHostMemoryInsufficient:
		return "the node's host memory cannot hold the weights an L1 sleep moves there; sleep at L2 instead"
	case ErrorPDNotPD:
		return "deployment is not PD disaggregated"
	case ErrorPDRoleUnknown:
		return "the role is not a role of this deployment"
	case ErrorAcceleratorFeatureUnsupported:
		return "the selected accelerator does not support sleep mode"
	}
	return "sleep order refused"
}
