package imagecatalog_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSyntheticXREManifestCandidateSelection(t *testing.T) {
	// This is a synthetic import contract, not a published runner image.
	data := []byte(`{"schema_version":3,"catalog_version":"fixture","series":[{"name":"XRE fixture","engine":"sglang","purpose":"inference","framework":"xre","versions":[{"image_ref":"registry.example/synthetic:tested","arch":"amd64","framework_version":"5.0.21.46"}]}]}`)
	st, err := store.Open("file:synthetic_xre_image_fixture?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := imagecatalog.ImportManifest(st, data); err != nil {
		t.Fatal(err)
	}
	row, err := st.ImageByRef("registry.example/synthetic:tested")
	if err != nil {
		t.Fatal(err)
	}
	candidate := deploy.ImageCandidate{Engine: row.Engine, Framework: row.Framework, FrameworkVersion: row.FrameworkVersion, Arch: row.Arch, ImageRef: row.ImageRef, Enabled: row.Enabled}
	if !deploy.ImageCandidateSelectable(deploy.ImageTarget{Engine: "sglang", Vendor: "kunlunxin", Framework: "xre", PlatformArch: "amd64", DriverVersion: "5.0.21.8"}, candidate) {
		t.Fatal("XRE image was not selectable for Kunlun")
	}
	if deploy.ImageCandidateSelectable(deploy.ImageTarget{Engine: "sglang", Vendor: "nvidia", Framework: "cuda", PlatformArch: "amd64"}, candidate) {
		t.Fatal("XRE image was selectable for CUDA")
	}
}
