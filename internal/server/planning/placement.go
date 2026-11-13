package planning

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/parallelism"
)

// Placement is the placement hint a deployment carries: what a confirmed plan
// pinned (GPU indexes and tensor parallel) and the shape of a PD or
// multi-node deployment's replicas. TP and PP are degrees over the whole
// deployment (package parallelism). Where a deployment may run is not here:
// that is its node requirements (store.DeploymentRequirements).
type Placement struct {
	GPUIndexes []int `json:"gpus,omitempty"`
	TP         int   `json:"tp,omitempty"`
	PP         int   `json:"pp,omitempty"`
	// PD is set on a PD disaggregated deployment: its roles, each with its
	// own TP and one unit per instance (ADR 0028). TP and GPUIndexes above
	// do not apply to it.
	PD *PDPlacement `json:"pd,omitempty"`
	// Units lists an ordinary multi-node deployment's replicas: each one's
	// leader node and follower nodes, in rank order. Without it the
	// deployment is one replica over its stored node list.
	Units []PDUnit `json:"units,omitempty"`
}

// ParallelismSpec is package parallelism's spec, named here for the same
// reason as the errors below.
type ParallelismSpec = parallelism.Spec

// The parallelism errors, surfaced here so callers that read placements
// through planning need not import package parallelism for them. They are the
// same values, not copies.
var (
	ErrParallelismTPRequired  = parallelism.ErrTPRequired
	ErrParallelismIndivisible = parallelism.ErrIndivisible
)

// Parallelism is the placement's parallelism for a deployment on nnodes
// nodes: the only conversion from the stored hint to package parallelism.
func (p Placement) Parallelism(nnodes int) parallelism.Spec {
	return parallelism.Spec{TP: p.TP, PP: p.PP, NNodes: nnodes}.Normalized()
}

// ParallelismOn is the placement's parallelism on nodes with these GPU
// counts, with an unstated multi-node TP and PP defaulted from them
// (parallelism.Spec.Defaulted).
func (p Placement) ParallelismOn(nodeGPUs []int) parallelism.Spec {
	return parallelism.Spec{TP: p.TP, PP: p.PP, NNodes: len(nodeGPUs)}.Defaulted(nodeGPUs)
}

// ParallelismOnDP defaults an unstated topology after accounting for each
// data-parallel copy. DP is derived from launch configuration, not stored here.
func (p Placement) ParallelismOnDP(nodeGPUs []int, dp int) (parallelism.Spec, error) {
	return parallelism.Spec{TP: p.TP, PP: p.PP, DP: dp, NNodes: len(nodeGPUs)}.DefaultedChecked(nodeGPUs)
}

// ParsePlacement reads a placement hint from either shape one appears in: the
// decoded object an api request carries, or the JSON text a stored row holds.
// An empty or absent hint is the zero placement, which is a deployment that
// named nothing.
//
// A document that is not a JSON object is an error. A single field the reader
// cannot use reads as unset instead, because these are best-effort hints and
// refusing to place a deployment is worse than placing it with the platform
// default. The one key refused outright is node_labels: a selector that is
// still in a document after it moved to the node requirements (migration 120
// leaves one it could not move) must never turn into an unconstrained
// placement.
func ParsePlacement(value any) (Placement, error) {
	switch typed := value.(type) {
	case nil:
		return Placement{}, nil
	case Placement:
		return typed, nil
	case map[string]any:
		return placementFromDocument(typed)
	case string:
		text := strings.TrimSpace(typed)
		// An empty column and an explicit JSON null both mean "nothing was
		// placed"; the api writes the latter for a deployment with no hint.
		if text == "" || text == "null" {
			return Placement{}, nil
		}
		document := map[string]any{}
		if err := json.Unmarshal([]byte(text), &document); err != nil || document == nil {
			if err == nil {
				err = fmt.Errorf("expected object")
			}
			return Placement{}, fmt.Errorf("decode placement: %w", err)
		}
		return placementFromDocument(document)
	}
	return Placement{}, fmt.Errorf("decode placement: unsupported value %T", value)
}

func placementFromDocument(document map[string]any) (Placement, error) {
	if _, present := document["node_labels"]; present {
		return Placement{}, fmt.Errorf("placement.node_labels is not read: state where a deployment may run as node_requirements")
	}
	placement := Placement{
		TP: placementInteger(document["tp"]),
		PP: placementInteger(document["pp"]),
	}
	// Both spellings appear in the wild: the deploy form sends "gpus" and the
	// confirmed plan an older client echoes back is "gpu_indexes".
	indexes, present := document["gpus"]
	if !present || indexes == nil {
		indexes = document["gpu_indexes"]
	}
	placement.GPUIndexes = placementIndexes(indexes)
	pd, err := parsePD(document)
	if err != nil {
		return Placement{}, err
	}
	placement.PD = pd
	units, err := parseReplicaUnits(document)
	if err != nil {
		return Placement{}, err
	}
	placement.Units = units
	return placement, nil
}

// placementInteger reads a whole number from any of the spellings a decoded
// JSON document produces; anything else is unset.
func placementInteger(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		if typed != float64(int(typed)) {
			return 0
		}
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	}
	return 0
}

// placementIndexes reads a GPU index list, dropping anything that is not a
// non-negative whole number: a hint the reader cannot use leaves the scheduler
// free rather than half-pinned.
func placementIndexes(value any) []int {
	switch typed := value.(type) {
	case []int:
		out := make([]int, 0, len(typed))
		for _, index := range typed {
			if index >= 0 {
				out = append(out, index)
			}
		}
		return out
	case []any:
		out := make([]int, 0, len(typed))
		for _, item := range typed {
			if index, ok := placementIndex(item); ok {
				out = append(out, index)
			}
		}
		return out
	}
	return nil
}

// placementIndex reads one GPU index. A negative or fractional value is not an
// index, which is why the second result exists: index 0 is legitimate.
func placementIndex(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		if typed >= 0 {
			return typed, true
		}
	case int64:
		if typed >= 0 {
			return int(typed), true
		}
	case float64:
		if typed >= 0 && typed == float64(int(typed)) {
			return int(typed), true
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil && parsed >= 0 {
			return int(parsed), true
		}
	}
	return 0, false
}
