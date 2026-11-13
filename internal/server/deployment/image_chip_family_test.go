package deployment

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	chipFamilyImage910B = "gpuworks/gpuworks-runner:cann9.2-910b-vllm0.30.0rc2-r1"
	chipFamilyImageA3   = "gpuworks/gpuworks-runner:cann9.2-a3-vllm0.30.0rc2-r1"
)

func ascendNodeOf(archID string, cards int) *store.Node {
	node := &store.Node{UUID: "ascend-" + archID, Arch: "arm64", Framework: "cann", FrameworkVersion: "9.2"}
	for i := 0; i < cards; i++ {
		node.GPUs = append(node.GPUs, store.GPU{Name: "Ascend " + archID, Vendor: "ascend", ArchID: archID, Index: i})
	}
	return node
}

// A node with two 910B4 cards once got the A3 (910C) build of the runner,
// because both builds of a CANN release looked alike to the catalog and the
// newer-inserted row won the tie.
func TestAutomaticImageFollowsTheNodesChipFamily(t *testing.T) {
	st, err := store.Open("file:deployment_image_chip_family_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The 910B build is inserted first, so the A3 build is the newer row.
	for _, ref := range []string{chipFamilyImage910B, chipFamilyImageA3} {
		if _, err := st.UpsertImage(&store.Image{
			Engine: "vllm", EngineVersion: "0.30.0rc2", Framework: "cann", FrameworkVersion: "9.2", Arch: "arm64",
			ImageRef: ref, Source: "official", Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := New(Deps{Store: st})

	if got := s.rankImage("vllm", templateSelection{}, "", ascendNodeOf("910B4", 2)).Image; got != chipFamilyImage910B {
		t.Fatalf("910B4 node picked %q, want %q", got, chipFamilyImage910B)
	}
	if got := s.rankImage("vllm", templateSelection{}, "", ascendNodeOf("910C", 2)).Image; got != chipFamilyImageA3 {
		t.Fatalf("910C node picked %q, want %q", got, chipFamilyImageA3)
	}
	// The preview's engine version and the options list use the same target.
	if got := s.requestEngineVersion("vllm", &dto.DeployRequest{}, templateSelection{}, "", ascendNodeOf("910B4", 2)); got != "0.30.0rc2" {
		t.Fatalf("engine version = %q", got)
	}

	// A node whose chip identity the build does not know excludes nothing.
	unknown := ascendNodeOf("Ascend910", 1)
	if got := s.rankImage("vllm", templateSelection{}, "", unknown); got.Image == "" || got.Blocking {
		t.Fatalf("unknown family node: %+v", got)
	}

	// The family only orders the pick: the image list of a 910B4 node still
	// offers the A3 build, and choosing it by hand is admitted.
	options := s.deploymentImageOptions(deploymentImageTarget("vllm", "", ascendNodeOf("910B4", 2)), templateSelection{}, "", nil)
	if !hasImageOptionContaining(options, "cann9.2-910b") || !hasImageOptionContaining(options, "cann9.2-a3") {
		t.Fatalf("910B4 node options = %+v", options)
	}
	if err := s.admitImagePlatforms("vllm", templateSelection{}, chipFamilyImageA3, []*store.Node{ascendNodeOf("910B4", 2)}); err != nil {
		t.Fatalf("manual A3 image on a 910B4 node was refused: %v", err)
	}
}
