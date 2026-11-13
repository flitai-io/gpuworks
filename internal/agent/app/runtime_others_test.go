package app

import (
	"context"
	goruntime "runtime"
	"testing"
)

// The active runtime is never its own image source, and a runtime that does
// not answer on this node is not offered. With no docker or nerdctl binary on
// PATH only runveil, which needs no daemon, answers.
func TestOtherRuntimesOfferOnlyRuntimesThatAnswerBesidesTheActiveOne(t *testing.T) {
	if goruntime.GOOS != "linux" || (goruntime.GOARCH != "amd64" && goruntime.GOARCH != "arm64") {
		t.Skip("runveil runs on linux/amd64 and linux/arm64 only")
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GPUWORKS_UDOCKER_LAUNCHER", "") // runveil, not the udocker CLI that needs python
	n := &NodeRuntime{stateDir: t.TempDir()}
	names := func(active string) []string {
		var out []string
		for _, source := range n.otherRuntimes(context.Background(), active) {
			out = append(out, source.Name())
		}
		return out
	}
	if got := names("docker"); len(got) != 1 || got[0] != "udocker" {
		t.Fatalf("sources besides docker = %v, want [udocker]", got)
	}
	if got := names("udocker"); len(got) != 0 {
		t.Fatalf("sources besides udocker = %v, want none", got)
	}
}
