package dto

import "time"

// List is a whole collection: every element, with Total = len(Items). Every
// unpaged collection answers with it rather than a bare array
// (docs/reference/api-conventions.md).
type List[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

// NewList wraps items; an empty collection encodes as "items": [].
func NewList[T any](items []T) List[T] {
	if items == nil {
		items = []T{}
	}
	return List[T]{Items: items, Total: len(items)}
}

// CursorPage is one page of a cursor-paged collection, where the next page
// is asked for by a cursor taken from the last item (the event timelines'
// before=<id>) and a total is not counted.
type CursorPage[T any] struct {
	Items []T `json:"items"`
}

// NewCursorPage wraps items; an empty page encodes as "items": [].
func NewCursorPage[T any](items []T) CursorPage[T] {
	if items == nil {
		items = []T{}
	}
	return CursorPage[T]{Items: items}
}

// ListPage is one page of a paged list: the rows the query's filters match,
// in the list's order, cut to the requested page. Total counts every row the
// filters match, not only this page. Every paged list answers with it (GET
// /deployments and GET /nodes with page parameters, GET /inference-templates).
type ListPage[T any] struct {
	Items    []T `json:"items"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	Total    int `json:"total"`
}

// DeploymentPage is one page of GET /api/v1/deployments?page=…, newest first.
type DeploymentPage = ListPage[DeploymentListItem]

// DeploymentRuntimeNode is one node the deployment's instances are on now.
type DeploymentRuntimeNode struct {
	NodeUUID string `json:"node_uuid"`
	NodeName string `json:"node_name,omitempty"`
	// Instances is how many instance rows are on the node, of any state, so a
	// client can fold a spread deployment into "N nodes" or expand it.
	Instances int `json:"instances"`
}

// DeploymentListItem is a deployment as a list row shows it. The launch
// configuration, the catalog selection and the instances stay with
// GET /api/v1/deployments/{id}; every field here but preparation has the
// meaning and value it has on Deployment.
type DeploymentListItem struct {
	// Preparation is the first of the deployment's model_progress still being
	// prepared while it is active; the detail has the whole list.
	Preparation      *ModelNodePresence `json:"preparation,omitempty"`
	ID               uint               `json:"id"`
	Name             string             `json:"name"`
	TemplateID       uint               `json:"template_id,omitempty"`
	TemplateDeviated bool               `json:"template_deviated"`
	Engine           string             `json:"engine"`
	Image            string             `json:"image,omitempty"`
	ModelID          uint               `json:"model_id"`
	ModelName        string             `json:"model_name"`
	ServedName       string             `json:"served_name"`
	NodeUUID         string             `json:"node_uuid"`
	NodeName         string             `json:"node_name,omitempty"`
	NodeNames        []string           `json:"node_names,omitempty"`
	// RuntimeNodes and RuntimeNodeNames are where the deployment's instances
	// run now, as on Deployment. node_uuid/node_name/node_names beside them
	// stay the placement target, so a row can show where a deployment was
	// asked to run and where it actually runs without either field meaning
	// both.
	RuntimeNodes     []DeploymentRuntimeNode `json:"runtime_nodes,omitempty"`
	RuntimeNodeNames []string                `json:"runtime_node_names,omitempty"`
	DesiredState     string                  `json:"desired_state"`
	RuntimeState     string                  `json:"runtime_state"`
	// State, AsleepReplicas and SleepLevel have the meaning and the value they
	// have on Deployment: the state to show, how many of the serving replicas
	// sleep, and the sleep level the engines report.
	State          string `json:"state"`
	AsleepReplicas int    `json:"asleep_replicas,omitempty"`
	SleepLevel     int    `json:"sleep_level,omitempty"`
	Sleepable      bool   `json:"sleepable"`
	Replicas       int    `json:"replicas"`
	// Replicas and ReadyReplicas total both roles for PD deployments, and the
	// detail response totals them the same way, so the row and the drawer
	// print one pair of counts rather than two.
	ReadyReplicas *int                  `json:"ready_replicas,omitempty"`
	Health        DeploymentHealth      `json:"health"`
	Operations    []DeploymentOperation `json:"operations"`
	CreatedAt     time.Time             `json:"created_at"`
}

// NodePage is one page of GET /api/v1/nodes?page=…, newest first. Total
// counts the nodes the filters match and All every node. Labels lists every
// user label key=value pair across all nodes, whatever the filters, so a
// label filter can offer them.
type NodePage struct {
	ListPage[Node]
	AgentFleet *AgentFleetSummary `json:"agent_fleet,omitempty"`
	All        int                `json:"all"`
	Labels     []NodeLabelValues  `json:"labels"`
}

// NodeLabelValues is one user label key and the values nodes give it, both
// sorted.
type NodeLabelValues struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// NodeFacets describes the nodes GET /api/v1/nodes/facets matched: how many,
// and the label values and accelerators they carry, each with the number of
// matched nodes carrying it. Forms that pick where a deployment may run show
// these instead of reading every node.
type NodeFacets struct {
	Total int `json:"total"`
	// Labels are the written labels, keys and values sorted.
	Labels []NodeLabelCounts `json:"labels"`
	// SystemLabels are the generated labels except the node identity (every
	// node has its own), with the server's display text.
	SystemLabels []NodeSystemLabelCounts `json:"system_labels"`
	// GPUs are the distinct accelerator models, as the node's cards report
	// them.
	GPUs []NodeGPUCount `json:"gpus"`
}

// NodeLabelCounts is one written label key and the nodes carrying each value.
type NodeLabelCounts struct {
	Key    string       `json:"key"`
	Values []ValueCount `json:"values"`
}

// ValueCount is one label value and how many matched nodes carry it.
type ValueCount struct {
	Value string `json:"value"`
	Nodes int    `json:"nodes"`
}

// NodeSystemLabelCounts is one system label key and the nodes carrying each
// value.
type NodeSystemLabelCounts struct {
	Key    string                  `json:"key"`
	Values []SystemLabelValueCount `json:"values"`
}

// SystemLabelValueCount is a system label value with its display text and
// how many matched nodes carry it.
type SystemLabelValueCount struct {
	Value       string `json:"value"`
	Display     string `json:"display"`
	DisplayCode string `json:"display_code,omitempty"`
	Nodes       int    `json:"nodes"`
}

// NodeGPUCount is one accelerator model - vendor, name and compute
// capability as a card reports them - and how many matched nodes have one.
// Model is the card's gpuworks.ai/gpu-model label value, so the nodes with
// it can be listed with label=gpuworks.ai/gpu-model=<model>.
type NodeGPUCount struct {
	ArchID string `json:"arch_id,omitempty"`
	Vendor string `json:"vendor"`
	Name   string `json:"name"`
	CC     string `json:"cc"`
	Model  string `json:"model"`
	Nodes  int    `json:"nodes"`
	// Cards counts the chip's cards on the matched nodes; MemoryBytes is the
	// largest one's memory (one model can ship in several memory sizes).
	Cards       int   `json:"cards"`
	MemoryBytes int64 `json:"memory_bytes"`
}

// DashboardSummary is what the dashboard shows of the fleet, counted on the
// server so the page does not fetch every node and deployment.
type DashboardSummary struct {
	CurrentMetrics DashboardCurrentMetrics `json:"current_metrics"`
	Nodes          DashboardNodes          `json:"nodes"`
	Deployments    DashboardDeployments    `json:"deployments"`
	// Cards counts the fleet's cards by the scheduling overview's states,
	// the same buckets GET /scheduling/overview sums; null when the overview
	// cannot be built.
	Cards *DashboardCards `json:"cards"`
	Tasks DashboardTasks  `json:"tasks"`
	// ByVendor groups nodes by accelerator vendor, most GPUs first.
	ByVendor []CapacityGroup `json:"by_vendor"`
	// LabelKeys are the user label keys in use, most widely used first.
	LabelKeys []string `json:"label_keys"`
	// LabelKey is the key ByLabel groups by: the requested one while some
	// node carries it, else the most widely used; empty without labels.
	LabelKey string          `json:"label_key"`
	ByLabel  []CapacityGroup `json:"by_label"`
}

// DashboardCards is the scheduling overview's summary (its fields inline)
// and Allocated, the cards a deployment holds: Awake+Asleep, a card a
// sleeper keeps counting as allocated because its memory is reserved for
// the wake. Cards held outside the platform, unhealthy or offline are not
// allocated, so Allocated/Cards is the share of the fleet given to
// deployments.
type DashboardCards struct {
	OverviewSummary
	Allocated int `json:"allocated"`
}

// DashboardNodes counts nodes by state and sums their hardware.
type DashboardNodes struct {
	Total        int     `json:"total"`
	Online       int     `json:"online"`
	Offline      int     `json:"offline"`
	Connecting   int     `json:"connecting"`
	GPUs         int     `json:"gpus"`
	UnusableGPUs int     `json:"unusable_gpus"`
	VRAMGiB      float64 `json:"vram_gib"`
	MemoryGiB    float64 `json:"memory_gib"`
}

// DashboardDeployments counts deployments by their effective state. Each
// deployment is in exactly one of Running, Warming, Sleeping, Pending and
// Other, so they add up to Total. A sleeping deployment serves nothing and
// is not counted as running. Sleepable counts the deployments that could
// sleep at all, whatever their state.
type DashboardDeployments struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	// Warming are deployments pulling an image or starting.
	Warming int `json:"warming"`
	// Sleeping are deployments asleep, or going to sleep, as a whole.
	Sleeping int `json:"sleeping"`
	// Pending are deployments waiting for a placement.
	Pending int `json:"pending"`
	// Other are the deployments in none of the buckets above: stopped,
	// failed, offline and the like.
	Other     int `json:"other"`
	Sleepable int `json:"sleepable"`
}

// DashboardTasks counts the work in flight besides the deployments.
type DashboardTasks struct {
	// Batches counts only the batches the caller may see (every user's for an
	// administrator, the caller's own otherwise); null when the Batch API is
	// not served or the count failed.
	Batches *DashboardBatches `json:"batches"`
	// Benchmarks are the evaluation runs pending or running, of every user:
	// evaluation runs are not owned per user.
	Benchmarks int `json:"benchmarks"`
}

// DashboardBatches counts the batches the caller may see: Active are in
// progress or finalizing, Waiting are still validating their input.
type DashboardBatches struct {
	Active  int `json:"active"`
	Waiting int `json:"waiting"`
}

// CapacityGroup is the capacity of the nodes sharing one value. For a label
// group Key is "key=value", and Unlabeled marks the nodes without the key.
type CapacityGroup struct {
	Key          string  `json:"key"`
	Unlabeled    bool    `json:"unlabeled,omitempty"`
	Nodes        int     `json:"nodes"`
	GPUs         int     `json:"gpus"`
	VRAMGiB      float64 `json:"vram_gib"`
	UnusableGPUs int     `json:"unusable_gpus"`
}

// ModelPage is one page of GET /api/v1/models?page=…: the matching models as
// list rows and the summary of every model.
type ModelPage struct {
	ListPage[ModelListItem]
	Summary ModelListSummary `json:"summary"`
}

// ModelListItem is a model as a list row shows it: the model without its
// README and per-location presence (GET /api/v1/models/{id} carries both),
// with the presence summed instead.
type ModelListItem struct {
	Model
	Presence ModelPresenceSummary `json:"presence"`
}

// ModelPresenceSummary sums where a model's files are: ready copies on nodes
// (node caches counted apart) and in shared storages, transfers in flight
// with their mean progress (0-100), and locations that failed, wait for a
// choice or went missing.
type ModelPresenceSummary struct {
	ReadyNodes      int `json:"ready_nodes"`
	CachedNodes     int `json:"cached_nodes"`
	ReadyStorages   int `json:"ready_storages"`
	InFlight        int `json:"in_flight"`
	InFlightPercent int `json:"in_flight_percent"`
	Failed          int `json:"failed"`
}

// ModelListSummary describes every model, whatever the page or filters.
type ModelListSummary struct {
	Registered int     `json:"registered"`
	ReadyFiles int     `json:"ready_files"`
	Sources    int     `json:"sources"`
	WeightGiB  float64 `json:"weight_gib"`
}

// DashboardCurrentMetrics contains fresh measurements from existing online
// hardware. Null means no sample; a measured zero remains zero.
type DashboardCurrentMetrics struct {
	UtilizationPct *float64 `json:"utilization_pct"`
	TemperatureC   *float64 `json:"temperature_c"`
	PowerWatts     *float64 `json:"power_watts"`
	// MemoryUsedBytes is the host memory the online nodes use.
	MemoryUsedBytes *float64 `json:"memory_used_bytes"`
	// VRAMUsedBytes and VRAMTotalBytes are the GPU memory the online nodes'
	// own cards use and have, summed over the same cards: those with a fresh
	// sample of both, so their ratio is the fleet's VRAM utilization. Both
	// are null when no card has both.
	VRAMUsedBytes  *float64 `json:"vram_used_bytes"`
	VRAMTotalBytes *float64 `json:"vram_total_bytes"`
}

// TemplateWeightModels answers GET /inference-templates/weight-models: the
// registered models that serve one spec's declared weight sources, matched
// by the server's repository identity rule (the deploy create path's).
type TemplateWeightModels struct {
	// Models is every registered model serving at least one source, newest
	// first, each once.
	Models  []ModelListItem       `json:"models"`
	Sources []TemplateWeightMatch `json:"sources"`
}

// TemplateWeightMatch is one declared source, in the spec's order, and the
// registered model a choice of that source reuses: the newest one from the
// same provider that serves it. Absent when the source would be registered.
type TemplateWeightMatch struct {
	Provider     string `json:"provider"`
	RepoID       string `json:"repo_id"`
	Revision     string `json:"revision,omitempty"`
	Subfolder    string `json:"subfolder,omitempty"`
	ReuseModelID uint   `json:"reuse_model_id,omitempty"`
}
