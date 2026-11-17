package proto

// TKubeSettings is the control message (server→agent) that carries the
// settings of a Kubernetes cluster entry which do not live in its DaemonSet
// manifest. The server sends it to a Kubernetes node after its initial state
// replay and again to every online node of the entry when an edit changes
// one, so the agents apply it live. An agent that does not know the message
// ignores it, and one outside Kubernetes mode ignores it too.
const TKubeSettings = "kube.settings"

// KubeSettings is the body of a TKubeSettings message. It is the whole
// current value, never a delta: an agent applies it as given. (The entry's
// engine port range reaches the agent as NetworkConfig.EnginePorts, the way
// every node's does.)
type KubeSettings struct {
	// IdleReturnSeconds is how long a card borrowed from Kubernetes stays the
	// platform's after the last instance on it stopped; negative keeps the
	// cards for good.
	IdleReturnSeconds int `json:"idle_return_seconds"`
	// RuntimeClass is the RuntimeClass that injects NVIDIA devices into the
	// instance pods started from now on; empty lets the agent use the
	// cluster's "nvidia" class when there is one, the node's default runtime
	// otherwise.
	RuntimeClass string `json:"runtime_class"`
}
