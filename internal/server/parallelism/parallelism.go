// Package parallelism is the one definition of how a deployment splits a
// model across GPUs. Tensor and pipeline parallel degrees are stated for the
// whole deployment; every node runs an equal share of the TP×PP×DP ranks, so a
// tensor-parallel group may span nodes. The scheduler, the launch assembly
// and request validation all derive the per-node GPU count from here.
//
// It is a leaf package: no imports from the rest of the server.
package parallelism

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrTPRequired rejects a multi-node deployment that states no tensor
	// parallel degree: the scheduler only chooses one on a single node.
	ErrTPRequired = errors.New("a multi-node deployment needs a tensor parallel degree")
	// ErrIndivisible rejects TP×PP×DP ranks that cannot be shared equally by
	// the nodes.
	ErrIndivisible = errors.New("tensor parallel × pipeline parallel × data parallel is not a multiple of the node count")
)

// Spec is a deployment's parallelism. TP 0 means unstated, which only a
// single-node deployment allows: the scheduler then picks the GPU count and
// TP follows it. PP, DP and NNodes below one read as one.
type Spec struct {
	TP     int
	PP     int
	DP     int
	NNodes int
}

// Normalized returns the spec with PP, DP and NNodes defaulted to one.
func (s Spec) Normalized() Spec {
	if s.DP < 1 {
		s.DP = 1
	}
	if s.PP < 1 {
		s.PP = 1
	}
	if s.NNodes < 1 {
		s.NNodes = 1
	}
	if s.TP < 0 {
		s.TP = 0
	}
	return s
}

// Stated reports whether the deployment fixed its tensor parallel degree.
func (s Spec) Stated() bool { return s.TP > 0 }

// World is the number of model shards in each data-parallel replica: TP×PP.
func (s Spec) World() int {
	s = s.Normalized()
	return s.TP * s.PP
}

// GPUsPerNode is how many GPUs each node runs. It is zero, with no error, for
// an unstated single-node spec the scheduler sizes.
func (s Spec) GPUsPerNode() (int, error) {
	s = s.Normalized()
	if !s.Stated() {
		if s.NNodes > 1 {
			return 0, ErrTPRequired
		}
		return 0, nil
	}
	world := s.TP * s.PP * s.DP
	if world%s.NNodes != 0 {
		return 0, fmt.Errorf("%w: %d×%d×%d ranks over %d nodes", ErrIndivisible, s.TP, s.PP, s.DP, s.NNodes)
	}
	return world / s.NNodes, nil
}

// Defaulted completes a multi-node spec whose TP is unstated from the GPU
// counts of its nodes. Callers that validate the result should use
// DefaultedChecked so an impossible PP×DP shape is not mistaken for missing TP.
func (s Spec) Defaulted(nodeGPUs []int) Spec {
	defaulted, _ := s.DefaultedChecked(nodeGPUs)
	return defaulted
}

// DefaultedChecked completes a multi-node spec whose TP is unstated from the
// GPU counts of its nodes: one pipeline stage per node unless PP is stated,
// and TP over the GPUs the fewest-GPU node has, so every node runs the same
// share and a node with more GPUs leaves the rest unused. A stated TP, a
// single node or a node without GPUs leaves the spec as stated.
func (s Spec) DefaultedChecked(nodeGPUs []int) (Spec, error) {
	if s.TP > 0 || len(nodeGPUs) < 2 {
		return s.Normalized(), nil
	}
	fewest := nodeGPUs[0]
	for _, count := range nodeGPUs[1:] {
		fewest = min(fewest, count)
	}
	if fewest < 1 {
		return s.Normalized(), nil
	}
	nodes := len(nodeGPUs)
	pp := s.PP
	if pp < 1 {
		pp = nodes
	}
	world := fewest * nodes
	if world%pp != 0 {
		// A stated PP that cannot split the GPUs falls back to a stage
		// per node.
		pp = nodes
	}
	dp := s.Normalized().DP
	if s.PP < 1 && world%(pp*dp) != 0 && world%dp == 0 {
		// With DP replicas consuming the available cards, the default
		// stage-per-node split may no longer fit; keep a single stage.
		pp = 1
	}
	if world%(pp*dp) != 0 || world/(pp*dp) < 1 {
		return s.Normalized(), fmt.Errorf("%w: %d GPUs over %d nodes cannot form pipeline parallel %d × data parallel %d", ErrIndivisible, world, nodes, pp, dp)
	}
	return Spec{TP: world / (pp * dp), PP: pp, DP: dp, NNodes: nodes}, nil
}

// MinimumGPUs is the smallest GPU group an automatic single-node placement
// may choose: every pipeline stage of every data-parallel replica needs one.
func (s Spec) MinimumGPUs() int {
	s = s.Normalized()
	if s.PP > math.MaxInt/s.DP {
		return math.MaxInt
	}
	return s.PP * s.DP
}

// Assigned completes an unstated spec from the GPU count the scheduler chose
// for a single node: TP takes every GPU the pipeline stages leave, falling
// back to PP 1 when the stages do not divide the count. A stated spec is
// returned as is.
func (s Spec) Assigned(gpus int) Spec {
	s = s.Normalized()
	if s.Stated() {
		return s
	}
	if gpus < 1 {
		gpus = 1
	}
	if gpus%(s.PP*s.DP) != 0 {
		s.PP = 1
	}
	s.TP = max(1, gpus/(s.PP*s.DP))
	return s
}
