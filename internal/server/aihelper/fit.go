package aihelper

// Target is what an answer is about, by reference: the server reads the
// subject itself rather than trusting a description the client wrote. At
// most one field is set.
type Target struct {
	InstanceUUID string
	ModelID      uint
	DeploymentID uint
	NodeUUID     string
}

// Tasks are what the assistant is asked to do: a kind of subject and an
// intent. Each has its own seed and system prompt on the server, so a user
// starts one with a click instead of writing a prompt; TaskAsk is a free
// question, about a subject when one is given.
const (
	TaskInstanceDiagnose      = "instance.diagnose"
	TaskModelFit              = "model.fit"
	TaskDeploymentUnscheduled = "deployment.unscheduled"
	TaskNodeHealth            = "node.health"
	TaskAsk                   = "ask"
)

// AskRequest is one ask: a task about a subject, an optional question, and
// the earlier turns of the thread when it is a follow-up.
type AskRequest struct {
	Task     string
	Target   Target
	Question string
	// Images are the data: URLs sent with the question (ValidateImages).
	Images  []string
	History []Message
	Locale  string
	// API is the platform API as the asking user reads it; the tools read
	// through it. Without it the API tools answer that it is unavailable.
	API APIReader
}

// TaskSubjectSet reports whether the request names the subject its task
// needs; a free question needs the question instead.
func (r AskRequest) TaskSubjectSet() bool {
	switch r.Task {
	case TaskInstanceDiagnose:
		return r.Target.InstanceUUID != ""
	case TaskModelFit:
		return r.Target.ModelID != 0
	case TaskDeploymentUnscheduled:
		return r.Target.DeploymentID != 0
	case TaskNodeHealth:
		return r.Target.NodeUUID != ""
	case TaskAsk:
		return r.Question != "" || len(r.Images) > 0
	}
	return false
}

// KnownTask reports whether task is one of the tasks above.
func KnownTask(task string) bool {
	switch task {
	case TaskInstanceDiagnose, TaskModelFit, TaskDeploymentUnscheduled, TaskNodeHealth, TaskAsk:
		return true
	}
	return false
}

// Fit is the physical fit of one model on each accelerator type of the online
// fleet: what the hardware is, not what is free right now - placement is the
// scheduler's answer, and the two must not disagree. It covers the weights
// and the engine reserve (what admission enforces) at the engine's default
// memory utilization; KV cache comes out of the headroom; multi-node serving
// is not considered.
type Fit struct {
	ModelID     uint     `json:"model_id"`
	WeightBytes int64    `json:"weight_bytes"`
	Dtype       string   `json:"dtype,omitempty"`
	GPUMemUtil  float64  `json:"gpu_memory_utilization"`
	Rows        []FitRow `json:"rows"`
}

// FitRow is one accelerator type.
type FitRow struct {
	Vendor      string `json:"vendor"`
	Name        string `json:"name"`
	MemoryBytes int64  `json:"memory_bytes"`
	Nodes       int    `json:"nodes"`
	// Cards counts the usable cards; Unusable those whose health says they
	// cannot serve.
	Cards           int `json:"cards"`
	Unusable        int `json:"unusable"`
	MaxCardsPerNode int `json:"max_cards_per_node"`
	// DtypeSupported says whether the card computes the model's dtype (a
	// Turing card has no bfloat16); DtypeNote says why not.
	DtypeSupported bool   `json:"dtype_supported"`
	DtypeNote      string `json:"dtype_note,omitempty"`
	// FastLinkCards is the most cards of this type one node links by NVLink
	// or another fast interconnect, over the nodes sampled; 1 means they talk
	// over PCIe only. Zero when no sampled node reported its topology.
	FastLinkCards int `json:"fast_link_cards"`
	// Fits says the weights and reserve fit at TP within one node: TP is the
	// smallest power of two that holds them, zero when none does.
	Fits          bool  `json:"fits"`
	TP            int   `json:"tp"`
	PerGPUBytes   int64 `json:"per_gpu_bytes"`
	UsableBytes   int64 `json:"usable_bytes"`
	HeadroomBytes int64 `json:"headroom_bytes"`
}
