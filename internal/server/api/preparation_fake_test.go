package api

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// testPreparation stands in for the hub and the controller: copy frames go
// through the real prepobs.Copies, so a test sees the same intent binding and
// expiry the server applies.
type testPreparation struct {
	copies *prepobs.Copies
	phases map[store.FileRef]string
	nodes  map[uint][]uint
	retry  func(context.Context, store.FileRef) error
	// generation counts place's changes of nodes.
	generation uint64
}

func newTestPreparation() *testPreparation {
	return &testPreparation{copies: prepobs.NewCopies(), phases: map[store.FileRef]string{}, nodes: map[uint][]uint{}}
}

func (p *testPreparation) record(node uint, status proto.ModelFileStatus) {
	p.copies.Record(node, status, time.Now())
}

func (p *testPreparation) ModelCopyObservation(file *store.ModelFile) (proto.ModelFileStatus, bool) {
	return p.copies.Observe(store.ModelFileKey(file.ID), file.State, file.TransferID, time.Now())
}

func (p *testPreparation) CachePhase(ref store.FileRef) string { return p.phases[ref] }

func (p *testPreparation) PreparationNodes(dep *store.Deployment) []uint { return p.nodes[dep.ID] }

// place is the controller choosing nodes for a deployment's instances to
// come, counted as the controller counts a changed choice.
func (p *testPreparation) place(depID uint, nodes ...uint) {
	p.nodes[depID] = nodes
	p.generation++
}

func (p *testPreparation) PreparationGeneration() uint64 { return p.generation }

func (p *testPreparation) RetryCacheCopy(ctx context.Context, ref store.FileRef) error {
	if p.retry == nil {
		return &planning.CacheRetryBlocked{Detail: "no retry in this test"}
	}
	return p.retry(ctx, ref)
}
