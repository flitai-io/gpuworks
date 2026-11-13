package aihelper

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The seed is the context the server gathers before the model sees anything:
// the first hop every diagnosis takes (this instance's logs, events and launch
// command) costs a round trip when the model has to ask for it and buys no
// judgement. The seed carries a manifest of what it left out and which tool
// fetches it, so the model knows it is looking at a sample, not the whole.

// maxSeedLogBytes bounds the log lines in one seed. The newest lines and the
// error-level ones are what a failure is read from; the rest is a search away.
const maxSeedLogBytes = 8 << 10

// LogLine is one log line in a seed.
type LogLine struct {
	TimeMS int64  `json:"time_ms"`
	Level  string `json:"level,omitempty"`
	Line   string `json:"line"`
}

// EventLine is one instance timeline event in a seed.
type EventLine struct {
	TimeMS  int64  `json:"time_ms"`
	Kind    string `json:"kind"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ErrorContext is what the server gathered about a failed instance.
type ErrorContext struct {
	InstanceUUID   string
	DeploymentID   uint
	DeploymentName string
	NodeUUID       string
	State          string
	Phase          string
	Message        string
	Image          string
	Command        []string
	GPUIndexes     []int
	Node           any
	Schedule       any
	Events         []EventLine
	// Logs are oldest first: the newest TailLines lines merged with up to
	// ErrorLines error-level lines from further back.
	Logs       []LogLine
	TailLines  int
	ErrorLines int
	Findings   []Finding
	// Question is the user's own question, if they asked one.
	Question string
}

// SeedLogs trims lines (oldest first) to the seed budget, keeping the newest,
// and clips each line. dropped counts the lines that did not fit.
func SeedLogs(lines []LogLine) (kept []LogLine, dropped int) {
	total := 0
	start := len(lines)
	for start > 0 {
		l := lines[start-1]
		l.Line = clip(l.Line, maxLineChars)
		if total+len(l.Line) > maxSeedLogBytes {
			break
		}
		total += len(l.Line)
		start--
	}
	kept = make([]LogLine, 0, len(lines)-start)
	for _, l := range lines[start:] {
		l.Line = clip(l.Line, maxLineChars)
		kept = append(kept, l)
	}
	return kept, start
}

// Prompt renders the seed as the user message of an error diagnosis.
func (c ErrorContext) Prompt() string {
	var b strings.Builder
	if q := strings.TrimSpace(c.Question); q != "" {
		b.WriteString("Question: " + q + "\n\n")
	} else {
		b.WriteString("Diagnose why this instance failed and how to fix it.\n\n")
	}
	b.WriteString("## Instance\n")
	fmt.Fprintf(&b, "instance_uuid: %s\nstate: %s\n", c.InstanceUUID, c.State)
	if c.Phase != "" {
		fmt.Fprintf(&b, "phase: %s\n", c.Phase)
	}
	if c.Message != "" {
		fmt.Fprintf(&b, "failure message: %s\n", c.Message)
	}
	if c.DeploymentID != 0 {
		fmt.Fprintf(&b, "deployment: %s (deployment_id %d)\n", c.DeploymentName, c.DeploymentID)
	}
	if c.NodeUUID != "" {
		fmt.Fprintf(&b, "node_uuid: %s\n", c.NodeUUID)
	}
	if len(c.GPUIndexes) > 0 {
		fmt.Fprintf(&b, "gpu indexes: %v\n", c.GPUIndexes)
	}
	if c.Image != "" {
		fmt.Fprintf(&b, "image: %s\n", c.Image)
	}
	if len(c.Command) > 0 {
		fmt.Fprintf(&b, "launch command: %s\n", strings.Join(c.Command, " "))
	}
	if c.Node != nil {
		b.WriteString("\n## Node hardware\n" + compactJSON(c.Node) + "\n")
	}
	if c.Schedule != nil {
		b.WriteString("\n## Schedule explanation and VRAM breakdown\n" + compactJSON(c.Schedule) + "\n")
	}
	if len(c.Events) > 0 {
		b.WriteString("\n## Recent events (newest first)\n")
		for _, e := range c.Events {
			fmt.Fprintf(&b, "%s %s %s %s\n", stamp(e.TimeMS), e.Kind, e.Code, e.Message)
		}
	}
	logs, dropped := SeedLogs(c.Logs)
	b.WriteString("\n## Container log lines (oldest first)\n")
	if len(logs) == 0 {
		b.WriteString("(no log lines recorded)\n")
	}
	for _, l := range logs {
		fmt.Fprintf(&b, "%s %s %s\n", stamp(l.TimeMS), l.Level, l.Line)
	}
	if h := SeedHypotheses(c.Findings); h != "" {
		b.WriteString("\n## " + h)
	}
	b.WriteString("\n## Manifest\n")
	fmt.Fprintf(&b, "- Logs: only the newest %d lines and up to %d error-level lines are included", c.TailLines, c.ErrorLines)
	if dropped > 0 {
		fmt.Fprintf(&b, "; %d of those were dropped to fit", dropped)
	}
	b.WriteString(". GET /api/v1/logs/search (q, level, from/to, instances) has the rest.\n")
	fmt.Fprintf(&b, "- Events: the newest %d; GET /api/v1/instances/{uuid}/events has more.\n", len(c.Events))
	b.WriteString("- Not included: other replicas (compare_with_healthy_replica), metrics (search_metrics), GPU topology (get_gpu_topology), the deployment (GET /api/v1/deployments/{id}).\n")
	return b.String()
}

// ModelContext is what the server gathered for a model-suitability question.
type ModelContext struct {
	ModelID  uint
	Meta     any
	Fit      Fit
	Question string
}

// Prompt renders the seed as the user message of a model question.
func (c ModelContext) Prompt() string {
	var b strings.Builder
	if q := strings.TrimSpace(c.Question); q != "" {
		b.WriteString("Question: " + q + "\n\n")
	} else {
		b.WriteString("Assess whether this model suits the fleet below: per accelerator type, whether it runs, how many cards and what parallelism, memory headroom, quantization and engine compatibility, risks and fitting use cases. Do not give a generic model introduction.\n\n")
	}
	fmt.Fprintf(&b, "## Model (model_id %d)\n%s\n", c.ModelID, compactJSON(c.Meta))
	b.WriteString("\n## Estimator fit per accelerator type in the online fleet\n")
	if len(c.Fit.Rows) == 0 {
		// An empty table read as data invites a guess at why it is empty.
		b.WriteString("No online node reports an accelerator, so there is nothing to size this model against. Say so plainly, then give only general guidance on what hardware the model needs.\n")
	} else {
		b.WriteString(compactJSON(c.Fit) + "\n")
	}
	b.WriteString("\n## Manifest\n- The fit is about the hardware, not what is free now: cards counts usable cards, unusable the broken ones; dtype_supported false means the card cannot compute the model's dtype (dtype_note says what to use instead); fast_link_cards is the largest group of that card one node links by NVLink or another fast interconnect (1 = PCIe only, 0 = unknown), which bounds an efficient tensor-parallel size.\n- It covers weights plus the engine's reserve at the engine's default memory utilization; KV cache comes out of the headroom. It does not consider multi-node serving or quantized variants of this model.\n- One node's detail is in GET /api/v1/nodes/{uuid} (a name works too) and get_gpu_topology.\n")
	return b.String()
}

func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

func stamp(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("15:04:05")
}

// InstanceLine is one instance as a deployment or node seed lists it.
type InstanceLine struct {
	UUID    string `json:"uuid"`
	State   string `json:"state"`
	Node    string `json:"node,omitempty"`
	Message string `json:"message,omitempty"`
}

// DeploymentContext is what the server gathered about a deployment whose
// replicas are not being placed.
type DeploymentContext struct {
	DeploymentID   uint
	Name           string
	State          string
	Model          string
	Engine         string
	Replicas       int
	GPUNeed        int
	VRAMLimitBytes int64
	NNodes         int
	Strategy       string
	Requirements   any
	Instances      []InstanceLine
	// Refusal is the scheduler's latest refusal: its code, message and the
	// recorded reason (counts by candidate status, closest nodes).
	Refusal        any
	RefusalAtMS    int64
	FirstRefusalMS int64
	Question       string
}

// Prompt renders the seed as the user message of a scheduling question.
func (c DeploymentContext) Prompt() string {
	var b strings.Builder
	if q := strings.TrimSpace(c.Question); q != "" {
		b.WriteString("Question: " + q + "\n\n")
	} else {
		b.WriteString("Explain why this deployment's replicas are not being placed and what would get them placed.\n\n")
	}
	fmt.Fprintf(&b, "## Deployment %s (deployment_id %d)\nstate: %s\nmodel: %s\nengine: %s\nreplicas wanted: %d\n", c.Name, c.DeploymentID, c.State, c.Model, c.Engine, c.Replicas)
	if c.VRAMLimitBytes > 0 {
		fmt.Fprintf(&b, "per replica: a GPU slice of %d bytes of VRAM (dGPU partition)\n", c.VRAMLimitBytes)
	} else {
		fmt.Fprintf(&b, "per replica: %d whole GPU(s) on one node\n", c.GPUNeed)
	}
	if c.NNodes > 1 {
		fmt.Fprintf(&b, "multi-node: %d nodes per replica\n", c.NNodes)
	}
	fmt.Fprintf(&b, "placement strategy: %s\nnode requirements: %s\n", c.Strategy, compactJSON(c.Requirements))
	b.WriteString("\n## Instances\n")
	if len(c.Instances) == 0 {
		b.WriteString("(none: no replica has been placed)\n")
	}
	for _, in := range c.Instances {
		fmt.Fprintf(&b, "%s %s node=%s %s\n", in.UUID, in.State, in.Node, in.Message)
	}
	b.WriteString("\n## Scheduler's latest refusal\n")
	if c.Refusal == nil {
		b.WriteString("(none recorded)\n")
	} else {
		fmt.Fprintf(&b, "at %s (first refusal at %s)\n%s\n", stamp(c.RefusalAtMS), stamp(c.FirstRefusalMS), compactJSON(c.Refusal))
	}
	b.WriteString("\n## Manifest\n- The refusal is the scheduler's own record; its counts cover every candidate node. A named node's cards are in GET /api/v1/nodes/{uuid}, the full spec in GET /api/v1/deployments/{id}, the candidate pool in GET /api/v1/deployments/{id}/candidates.\n")
	return b.String()
}

// GPUHealthLine is one accelerator as a node seed lists it.
type GPUHealthLine struct {
	Index int `json:"index"`
	// PCIeBDF names a card the vendor CLI did not number (Index -1).
	PCIeBDF string `json:"pcie_bdf,omitempty"`
	Name    string `json:"name"`
	Level   string `json:"level"`
	Code    string `json:"code,omitempty"`
	// Schedulable is whether work may go here, which the level does not
	// answer: a card at level ok can be lent to a virtual machine or held
	// by somebody else's process. The seed asks the model whether each
	// accelerator can serve, so it has to be told.
	Schedulable bool `json:"schedulable"`
	// Ownership is why not, when the reason is not a finding.
	Ownership string `json:"ownership,omitempty"`
	// ForeignUse names who outside the platform holds the card
	// ("python3 pid 4242"), another reason it takes no work that is not a
	// fault.
	ForeignUse []string `json:"foreign_use,omitempty"`
	Evidence   string   `json:"evidence,omitempty"`
}

// NodeContext is what the server gathered for a node health check.
type NodeContext struct {
	NodeUUID   string
	Name       string
	State      string
	Scheduling string
	// RuntimeGPUBlocked is why the node's container runtime cannot give a
	// container its accelerators (the platform cordons it while set);
	// GPUProbeError why the vendor tool did not answer, so the card facts
	// it alone supplies are as it last left them.
	RuntimeGPUBlocked string
	GPUProbeError     string
	// RuntimeDriver is the runtime the node runs; RuntimeFallbackFrom the one
	// it passed over for runveil and RuntimeFallbackReason why (a code; empty
	// when nothing was passed over).
	RuntimeDriver         string
	RuntimeFallbackFrom   string
	RuntimeFallbackReason string
	LastSeenMS            int64
	Hardware              any
	GPUs                  []GPUHealthLine
	// FastCliques are the groups of cards linked by NVLink or another fast
	// interconnect, by index.
	FastCliques [][]int
	Instances   []InstanceLine
	Logs        []LogLine
	LogLimit    int
	Question    string
}

// Prompt renders the seed as the user message of a node health check.
func (c NodeContext) Prompt() string {
	var b strings.Builder
	if q := strings.TrimSpace(c.Question); q != "" {
		b.WriteString("Question: " + q + "\n\n")
	} else {
		b.WriteString("Check this node's health: can it and each of its accelerators serve, and what needs fixing?\n\n")
	}
	fmt.Fprintf(&b, "## Node %s (node_uuid %s)\nstate: %s\nscheduling: %s\nlast heartbeat: %s\n", c.Name, c.NodeUUID, c.State, c.Scheduling, stamp(c.LastSeenMS))
	if c.RuntimeDriver != "" {
		b.WriteString("container runtime: " + c.RuntimeDriver + "\n")
	}
	if c.RuntimeGPUBlocked != "" {
		b.WriteString("container runtime cannot use the accelerators: " + c.RuntimeGPUBlocked + "\n")
	}
	if c.RuntimeFallbackFrom != "" {
		b.WriteString("runtime passed over for runveil (udocker): " + c.RuntimeFallbackFrom + " (" + c.RuntimeFallbackReason + ")\n")
	}
	if c.GPUProbeError != "" {
		b.WriteString("vendor tool did not answer (card memory/UUID/health are from its last answer): " + clip(c.GPUProbeError, maxLineChars) + "\n")
	}
	if c.Hardware != nil {
		b.WriteString("hardware: " + compactJSON(c.Hardware) + "\n")
	}
	b.WriteString("\n## Accelerators\n")
	if len(c.GPUs) == 0 {
		b.WriteString("(none reported)\n")
	}
	for _, g := range c.GPUs {
		serves := "yes"
		if !g.Schedulable {
			serves = "no"
			switch {
			case g.Ownership != "":
				serves = "no (" + g.Ownership + ")"
			case len(g.ForeignUse) > 0:
				serves = "no (in use outside the platform: " + strings.Join(g.ForeignUse, ", ") + ")"
			}
		}
		// A card the vendor CLI does not number is named by its PCI
		// address; "#-1" would read as a device that exists.
		label := fmt.Sprintf("#%d", g.Index)
		if g.Index < 0 {
			label = "pci " + g.PCIeBDF
		}
		fmt.Fprintf(&b, "%s %s level=%s code=%s can_serve=%s %s\n", label, g.Name, g.Level, g.Code, serves, clip(g.Evidence, maxLineChars))
	}
	if len(c.FastCliques) > 0 {
		fmt.Fprintf(&b, "fast interconnect groups: %v\n", c.FastCliques)
	}
	b.WriteString("\n## Instances on this node\n")
	if len(c.Instances) == 0 {
		b.WriteString("(none)\n")
	}
	for _, in := range c.Instances {
		fmt.Fprintf(&b, "%s %s %s\n", in.UUID, in.State, in.Message)
	}
	logs, dropped := SeedLogs(c.Logs)
	b.WriteString("\n## Recent warning and error lines from the node and its agent (oldest first)\n")
	if len(logs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, l := range logs {
		fmt.Fprintf(&b, "%s %s %s\n", stamp(l.TimeMS), l.Level, l.Line)
	}
	b.WriteString("\n## Manifest\n")
	fmt.Fprintf(&b, "- Logs: at most the newest %d warning-or-worse lines from the node and agent", c.LogLimit)
	if dropped > 0 {
		fmt.Fprintf(&b, "; %d dropped to fit", dropped)
	}
	b.WriteString(". Metrics are not included. Read a series only when the lines above do not settle temperature, power or throttle: call_api GET /api/v1/metrics/query with metric gpuworks_gpu_temperature_celsius, gpuworks_gpu_power_watts or gpuworks_gpu_throttle_reasons and jq `.items[].points[-1][1]` (a point is [unix ms, value]). get_gpu_topology has the link matrix. Do not re-read this node's hardware. Connection diagnostics are not in this seed: for connecting/offline nodes or a connection question, call get_node_connection_diagnostics with this node_uuid before attributing a connection failure.\n")
	return b.String()
}
