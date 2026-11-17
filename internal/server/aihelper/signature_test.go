package aihelper

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// Match is the shared table (pkg/enginelog tests every signature): the codes,
// evidence and indexes it reports come through unchanged.
func TestMatchUsesTheSharedTable(t *testing.T) {
	lines := []string{"start", "CUDA out of memory #1", "x", "CUDA out of memory #2", "CUDA out of memory #3", "CUDA out of memory #4"}
	got := Match(lines)
	if len(got) != 1 || got[0].Code != enginelog.CodeCUDAOOM || len(got[0].Evidence) != enginelog.MaxEvidence {
		t.Fatalf("Match = %+v", got)
	}
	if ev := got[0].Evidence; ev[0].Index != 3 || ev[2].Index != 5 || ev[2].Line != "CUDA out of memory #4" {
		t.Fatalf("evidence = %+v, want the newest three with their indexes", ev)
	}
	if Match(nil) != nil {
		t.Fatal("Match(nil) should report nothing")
	}
	if !reflect.DeepEqual(SignatureCodes(), enginelog.Codes()) {
		t.Fatal("finding codes are not the shared table's")
	}
}

// An instance with a structured reason is diagnosed by the causes its agent
// reported, even when its lines would match something else; one without (an
// older agent) is matched against its lines.
func TestFindingsAreCodeFirst(t *testing.T) {
	lines := []string{"Error response from daemon: pull access denied for x"}
	reported := &proto.RuntimeFailure{Code: proto.ErrorContainerExited, Causes: []proto.FailureCause{
		{Code: "kv_cache_insufficient", Evidence: []string{"ValueError: ... larger than the maximum number of tokens that can be stored in KV cache"}},
		{Code: enginelog.CodeNoOutput},
	}}
	got := Findings(reported, lines)
	if len(got) != 2 || got[0].Code != enginelog.CodeKVCacheInsufficient || got[1].Code != enginelog.CodeNoOutput {
		t.Fatalf("reported findings = %+v", got)
	}
	if got[0].Evidence[0].Index != -1 || !strings.Contains(got[0].Evidence[0].Line, "KV cache") || len(got[1].Evidence) != 0 {
		t.Fatalf("reported evidence = %+v", got)
	}
	// A reason without causes (a health timeout) finds nothing in the lines.
	if got := Findings(&proto.RuntimeFailure{Code: proto.ErrorEngineHealthTimeout}, lines); got != nil {
		t.Fatalf("reason without causes = %+v", got)
	}
	// An older agent's report has no reason: its lines are matched.
	got = Findings(nil, lines)
	if len(got) != 1 || got[0].Code != enginelog.CodeImagePullDenied || got[0].Evidence[0].Index != 0 {
		t.Fatalf("fallback findings = %+v", got)
	}
}

func TestSeedHypothesesAreMarkedUnverified(t *testing.T) {
	if SeedHypotheses(nil) != "" {
		t.Fatal("no findings should render nothing")
	}
	long := "CUDA out of memory " + strings.Repeat("y", 1000)
	h := SeedHypotheses(Match([]string{long}))
	if !strings.Contains(h, "unverified") || !strings.Contains(h, "[CUDA_OOM]") || !strings.Contains(h, "evidence: CUDA out of memory") {
		t.Fatalf("hypotheses = %q", h)
	}
	if strings.Contains(h, strings.Repeat("y", maxLineChars)) {
		t.Fatal("evidence line was not clipped")
	}
	// A code outside the table has no hypothesis to offer.
	if h := SeedHypotheses([]Finding{{Code: "SOMETHING_NEW"}}); strings.Contains(h, "SOMETHING_NEW") {
		t.Fatalf("unknown code rendered: %q", h)
	}
}

func TestClipKeepsUTF8Boundaries(t *testing.T) {
	got := clip("显存不足显存不足", 7)
	if got != "显存…" {
		t.Fatalf("clip = %q", got)
	}
}
