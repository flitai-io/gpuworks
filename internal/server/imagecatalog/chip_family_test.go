package imagecatalog_test

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
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

func TestPublishedSpecialistImagesRemainManualAfterEnable(t *testing.T) {
	st, err := store.Open("file:image_specialist_sync?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	tags := []string{
		"rocm10.0-sglang0.5.23-mi30x-r3-linux-amd64",
		"rocm10.0-sglang0.5.23-mi35x-r3-linux-amd64",
		"cuda12.8-v100-vllm1cat1.5.3-r1-linux-amd64",
		"cuda12.8-v100-vllm1cat1.5.1-r1",
		"cuda12.8-systemone1.0-r1",
	}
	for _, tag := range tags {
		ref := "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:" + tag
		row, err := st.ImageByRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetImageEnabled(row.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := imagecatalog.Candidates(st)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range candidates {
		if !c.Enabled || !strings.HasPrefix(c.ImageRef, "swr.") {
			continue
		}
		target := deploy.ImageTarget{Engine: c.Engine, Framework: c.Framework, PlatformOS: "linux", PlatformArch: "amd64"}
		if !deploy.ImageCandidateSelectable(target, c) {
			t.Fatalf("%s unavailable for manual selection", c.ImageRef)
		}
		if auto := deploy.ResolveImageCatalog(target, candidates); strings.HasPrefix(auto.Image, "swr.") {
			t.Fatalf("auto-picked specialist image: %+v", auto)
		}
		target.Override = c.ImageRef
		if pick := deploy.ResolveImageCatalog(target, candidates); pick.Image != c.ImageRef || pick.Blocking {
			t.Fatalf("explicit choice rejected: %+v", pick)
		}
		checked++
	}
	if checked != len(tags) {
		t.Fatalf("checked %d specialist images, want %d", checked, len(tags))
	}
}
