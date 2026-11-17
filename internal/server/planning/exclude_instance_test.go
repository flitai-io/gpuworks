package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A replacement previewed before its instance goes (a rolling restart's
// check) is not charged that instance's claim; the deployment's other
// instances still are.
func TestExcludeInstanceUUIDLeavesOnlyThatInstanceOut(t *testing.T) {
	st, in, _ := gpuCandidatesFixture(t, proto.InstRunning, "", yieldpolicy.Never)
	in.GPUIndexes = []int{0}
	if candidateAvailable(t, in.Evaluate(st).GPUCandidates, 0) {
		t.Fatal("occupied card offered")
	}
	in.ExcludeInstanceUUID = "holder-instance"
	if !candidateAvailable(t, in.Evaluate(st).GPUCandidates, 0) {
		t.Fatal("the excluded instance's card is still charged")
	}
	in.ExcludeInstanceUUID = "another-instance"
	if candidateAvailable(t, in.Evaluate(st).GPUCandidates, 0) {
		t.Fatal("excluding another instance freed this one's card")
	}
}
