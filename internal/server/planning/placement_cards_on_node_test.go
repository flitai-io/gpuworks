package planning

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An index names a card on one machine: a placement naming a card the node
// does not report is refused with the node, the index and the node's card
// count, never narrowed to the cards that happen to share a number (GWKS-522).
func TestCardsOnNodeRefusesACardTheNodeDoesNotReport(t *testing.T) {
	node := &store.Node{UUID: "uuid-two", Name: "two-card", GPUs: []store.GPU{{Index: 0}, {Index: 3}}}
	for _, tc := range []struct {
		name    string
		indexes []int
		missing int // -1: accepted
	}{
		{"both reported", []int{0, 3}, -1},
		{"nothing pinned", nil, -1},
		{"one is another node's", []int{3, 7}, 7},
		{"the first missing is named", []int{9, 7}, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CardsOnNode(Placement{GPUIndexes: tc.indexes, TP: 1}, node)
			if tc.missing < 0 {
				if err != nil {
					t.Fatalf("CardsOnNode = %v, want accepted", err)
				}
				return
			}
			var pe *messages.ProductError
			if !errors.As(err, &pe) || pe.Code() != messages.ErrorPlacementGPUNotOnNode {
				t.Fatalf("CardsOnNode = %v, want %s", err, messages.ErrorPlacementGPUNotOnNode)
			}
			params := pe.Params()
			if params["node"] != "two-card" || params["gpu_index"] != tc.missing || params["node_gpus"] != 2 {
				t.Fatalf("params = %v", params)
			}
		})
	}
	if err := CardsOnNode(Placement{GPUIndexes: []int{7}}, nil); err != nil {
		t.Fatalf("no node to judge against = %v", err)
	}
}

// A preview has no deployment yet: its errors must not read "deployment 0".
func TestALaunchErrorWithoutADeploymentNamesNoDeployment(t *testing.T) {
	cause := errors.New("vllm topology: needs 2 GPUs, node has 1")
	preview := (&LaunchResolveError{Err: cause}).Error()
	if strings.Contains(preview, "deployment") || !strings.HasPrefix(preview, "resolve launch: vllm topology: ") {
		t.Fatalf("preview error = %q", preview)
	}
	stored := (&LaunchResolveError{DeploymentID: 42, Err: cause}).Error()
	if !strings.HasPrefix(stored, "deployment 42 resolve launch: vllm topology: ") {
		t.Fatalf("stored error = %q", stored)
	}
}
