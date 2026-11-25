package manifest

import "testing"

func TestLegacyCatalogRequiresAnExactReplacementReference(t *testing.T) {
	const tag = "cuda13.0-vllm0.30.0-r0"
	const custom = "registry.example:5000/gpuworks/gpuworks-runner:" + tag
	data := []byte(`{"schema_version":4,"catalog_version":"test","series":[{"name":"CUDA","versions":[{"image_ref":"` + custom + `","arch":"multi","manual_only":true}]}]}`)
	legacy, err := LegacyCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := Parse(legacy)
	if err != nil {
		t.Fatal(err)
	}
	versions := projected.Series[0].Versions
	if len(versions) != 1 || versions[0].ImageRef != custom || !versions[0].RollingTag {
		t.Fatalf("unrelated repository acquired an unverified alias: %+v", versions)
	}
}
