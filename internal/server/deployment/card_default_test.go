package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An unhealthy card cannot run anything, so it must not drag the share down
// for the cards that can.
func TestAnUnhealthyCardDoesNotDecideTheShare(t *testing.T) {
	node := &store.Node{GPUs: []store.GPU{
		{Index: 0, Name: "A100-SXM4-80GB", MemoryTotal: 80 << 30},
		{Index: 1, Name: "GeForce RTX 2080 Ti", MemoryTotal: 11 << 30, HealthStatus: accelerator.HealthError},
	}}
	cards := DeployTargetCards(node, nil)
	if len(cards) != 1 || cards[0].MemoryTotalBytes != 80<<30 {
		t.Fatalf("cards = %+v, want only the healthy 80 GiB card", cards)
	}
	// A pinned unhealthy card is still named by the deployer, and admission is
	// what refuses it; silently dropping it here would leave no cards at all
	// and read as "hardware unknown".
	pinned := DeployTargetCards(node, map[string]any{"gpus": []any{1}})
	if len(pinned) != 1 || pinned[0].MemoryTotalBytes != 11<<30 {
		t.Fatalf("pinned = %+v, want the card the request named", pinned)
	}
}

// With no node there is no hardware to read, and inventing one would be worse
// than leaving the dictionary's default: the automatic mode picks the node
// after submission.
func TestNoNodeLeavesTheDictionaryDefault(t *testing.T) {
	if cards := DeployTargetCards(nil, nil); cards != nil {
		t.Fatalf("cards = %+v, want none", cards)
	}
}
