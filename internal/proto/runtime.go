package proto

// Runtime preferences are persisted by the server and replayed to agents.
// RuntimeDriver in SysInfo remains the observed runtime; these values describe
// the user's selection policy.
const (
	RuntimePreferenceAuto    = "auto"
	RuntimePreferenceDocker  = "docker"
	RuntimePreferenceNerdctl = "nerdctl"
	RuntimePreferenceUdocker = "udocker"
)

func ValidRuntimePreference(value string) bool {
	switch value {
	case RuntimePreferenceAuto, RuntimePreferenceDocker, RuntimePreferenceNerdctl, RuntimePreferenceUdocker:
		return true
	default:
		return false
	}
}

// The runtime drivers a node can report (Registered.runtime_driver). They
// are the names the agent's drivers return and the names an accelerator
// vendor lists the runtimes its cards work on by.
const (
	RuntimeDriverDocker  = "docker"
	RuntimeDriverNerdctl = "nerdctl"
	RuntimeDriverUdocker = "udocker"
	RuntimeDriverKube    = "kube"
	// RuntimeDriverHost is the runtime a Mac node reports: engines run as
	// host processes from environment packages, in the node's own network.
	RuntimeDriverHost = "host"
)
