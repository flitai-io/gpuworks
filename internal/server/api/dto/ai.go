package dto

// AIAskRequest is POST /api/v1/ai/ask: a task about a subject named by
// reference - instance_uuid for instance.diagnose, model_id for model.fit,
// deployment_id for deployment.unscheduled, node_uuid for node.health - and
// the server gathers its context itself. Task "ask" is a free question,
// about at most one subject when one is named. A follow-up carries the
// thread's earlier turns in history. images are data: URLs sent with the
// question, and a user turn of history carries its own; the limits are those
// GET /api/v1/ai/available answers.
type AIAskRequest struct {
	Task         string      `json:"task"`
	InstanceUUID string      `json:"instance_uuid,omitempty"`
	ModelID      uint        `json:"model_id,omitempty"`
	DeploymentID uint        `json:"deployment_id,omitempty"`
	NodeUUID     string      `json:"node_uuid,omitempty"`
	Question     string      `json:"question,omitempty"`
	Images       []string    `json:"images,omitempty"`
	History      []AIMessage `json:"history,omitempty"`
	Locale       string      `json:"locale,omitempty"`
}

// AIMessage is one earlier turn of a thread.
type AIMessage struct {
	Role    string   `json:"role"` // user | assistant
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"` // user turns only
}

// AIAvailability is GET /api/v1/ai/available: whether the assistant can
// answer, and whether the service in use takes images - with the limits an
// ask's images must keep - so the console offers pasting only then.
type AIAvailability struct {
	Available  bool          `json:"available"`
	ImageInput bool          `json:"image_input"`
	Images     *AIImageLimit `json:"images,omitempty"`
}

// AIImageLimit bounds the images of one ask: per message, per request (the
// question and the thread together), the bytes of one image, and the longest
// side the console shrinks an image to before sending it.
type AIImageLimit struct {
	PerMessage int `json:"per_message"`
	PerRequest int `json:"per_request"`
	MaxBytes   int `json:"max_bytes"`
	MaxSide    int `json:"max_side"`
}

// AIAnswer is the answer and how the model reached it: every tool it called,
// the LLM round trips and the tokens they cost.
type AIAnswer struct {
	Answer string   `json:"answer"`
	Steps  []AIStep `json:"steps"`
	Calls  int      `json:"calls"`
	Usage  AIUsage  `json:"usage"`
}

type AIStep struct {
	Tool  string `json:"tool"`
	Args  string `json:"args"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

type AIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// AIFinding is one known failure an instance's logs match, as GET
// /api/v1/ai/instances/{uuid}/findings lists them. code is stable; the
// client renders its title and suggested fix.
type AIFinding struct {
	Code     string       `json:"code"`
	Evidence []AIEvidence `json:"evidence"`
}

type AIEvidence struct {
	Line string `json:"line"`
}

// AIModelFit is GET /api/v1/ai/models/{id}/fit: the physical fit of a model
// on each accelerator type of the online fleet (not what is free now).
type AIModelFit struct {
	ModelID     uint       `json:"model_id"`
	WeightBytes int64      `json:"weight_bytes"`
	Dtype       string     `json:"dtype,omitempty"`
	GPUMemUtil  float64    `json:"gpu_memory_utilization"`
	Rows        []AIFitRow `json:"rows"`
}

type AIFitRow struct {
	Vendor          string `json:"vendor"`
	Name            string `json:"name"`
	MemoryBytes     int64  `json:"memory_bytes"`
	Nodes           int    `json:"nodes"`
	Cards           int    `json:"cards"`
	Unusable        int    `json:"unusable"`
	MaxCardsPerNode int    `json:"max_cards_per_node"`
	DtypeSupported  bool   `json:"dtype_supported"`
	DtypeNote       string `json:"dtype_note,omitempty"`
	FastLinkCards   int    `json:"fast_link_cards"`
	Fits            bool   `json:"fits"`
	TP              int    `json:"tp"`
	PerGPUBytes     int64  `json:"per_gpu_bytes"`
	UsableBytes     int64  `json:"usable_bytes"`
	HeadroomBytes   int64  `json:"headroom_bytes"`
}

// The streamed form of /ai/ask (Accept: text/event-stream) sends these as SSE
// events: "step" for each tool call as it starts and ends, "text" and
// "reasoning" deltas, then one "done" or "error" event. ": ping" comments keep
// idle proxies from closing the stream.

// AIStreamStep is a "step" event. id ties a tool call's running and finished
// events together.
type AIStreamStep struct {
	ID     int    `json:"id"`
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Status string `json:"status"` // running | done | error
	MS     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
}

// AIStreamDelta is a "text" or "reasoning" event.
type AIStreamDelta struct {
	Delta string `json:"delta"`
}

// AIStreamError is the "error" event. code is a stable product error code
// when the failure has one - AI_IMAGE_REJECTED for a service that refused the
// images - and empty otherwise.
type AIStreamError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// AIStreamDone is the "done" event.
type AIStreamDone struct {
	Calls       int     `json:"calls"`
	Usage       AIUsage `json:"usage"`
	FirstTextMS int64   `json:"first_text_ms"`
	TotalMS     int64   `json:"total_ms"`
}
