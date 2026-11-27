package imagecatalog_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The candidates carry the chip family their reference names, so the
// resolver can tell the 910B build of a CANN release from the A3 one.
func TestCandidatesCarryTheRunnerChipFamily(t *testing.T) {
	st, err := store.Open("file:imagecatalog_chip_family_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	candidates, err := imagecatalog.Candidates(st)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"gpuworks/gpuworks-runner:cann8.5-a3-sglang0.5.12.post1-r2": "910c",
	}
	seen := map[string]bool{}
	families := map[string]int{}
	for _, candidate := range candidates {
		families[candidate.ChipFamily]++
		if family, ok := want[candidate.ImageRef]; ok {
			seen[candidate.ImageRef] = true
			if candidate.ChipFamily != family {
				t.Errorf("%s: chip family = %q, want %q", candidate.ImageRef, candidate.ChipFamily, family)
			}
		}
		if candidate.Framework != "cann" && candidate.ChipFamily != "" {
			t.Errorf("%s (%s): only CANN images name a chip family, got %q", candidate.ImageRef, candidate.Framework, candidate.ChipFamily)
		}
	}
	for ref := range want {
		if !seen[ref] {
			t.Errorf("seed has no %s", ref)
		}
	}
	if families["910b"] == 0 || families["910c"] == 0 || families[""] == 0 {
		t.Fatalf("seed candidates by family = %v, want 910b, 910c and generic images", families)
	}
}
