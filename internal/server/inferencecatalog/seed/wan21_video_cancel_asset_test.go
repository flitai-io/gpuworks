package seed

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestWan21VideoCancellationUsesTemplateLocalR2Patch(t *testing.T) {
	_, spec := catalogSpec(t, "wan-ai/wan2.1-t2v-1.3b-diffusers", "bf16-sglang-nvidia-general")
	if spec.Image != "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2" {
		t.Fatalf("the cancellation fix must reuse the published r2 image: %s", spec.Image)
	}
	if len(spec.Assets) != 1 {
		t.Fatalf("want one template-local cancellation asset, got %v", spec.Assets)
	}
	asset := spec.Assets[0]
	if asset.UnpackTo != "/opt/gpuworks/sglang-video-cancel" ||
		spec.PreStartScript != "python3 /opt/gpuworks/sglang-video-cancel/apply_video_cancel.py" ||
		strings.Join(spec.Entrypoint, " ") != "sglang serve" || len(spec.PreStartArgv) != 0 {
		t.Fatalf("patch must run before the original SGLang entrypoint: %+v", spec)
	}
	data, err := embeddedCatalog.ReadFile("catalog/models/wan-ai/wan2.1-t2v-1.3b-diffusers/" + asset.File)
	if err != nil {
		t.Fatal(err)
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(data)); actual != asset.SHA256 {
		t.Fatalf("asset checksum %s does not match template %s", actual, asset.SHA256)
	}
	if int64(len(data)) != asset.SizeBytes {
		t.Fatalf("asset size %d does not match template %d", len(data), asset.SizeBytes)
	}
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, other := range model.Specs {
			if model.ModelID == "wan-ai/wan2.1-t2v-1.3b-diffusers" && other.Name == spec.Name {
				continue
			}
			if strings.Contains(other.PreStartScript, "apply_video_cancel.py") {
				t.Errorf("video cancellation patch leaked into %s/%s", model.ModelID, other.Name)
			}
			for _, item := range other.Assets {
				if item.UnpackTo == asset.UnpackTo {
					t.Errorf("video cancellation asset leaked into %s/%s", model.ModelID, other.Name)
				}
			}
		}
	}
}
