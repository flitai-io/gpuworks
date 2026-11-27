package inferencecatalog

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestBuildSeedManifestCountsModelFilesAndSpecs(t *testing.T) {
	source := packageFixture(t)
	descriptor := []byte("schema_version: 3\n")
	source["manifest.yaml"].Data = descriptor

	// A second contributor adds two profiles in another model file. Neither
	// contributor has to edit the descriptor.
	model := flatFixture().Models[0]
	model.ModelID = "org/second"
	spec := model.Specs[0]
	spec.Name = "second-profile"
	spec.DisplayNameEN = "Second profile"
	spec.DisplayNameZHCN = "第二个配置"
	model.Specs = append(model.Specs, spec)
	data, err := MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	source["models/org/second/model.yaml"] = &fstest.MapFile{Data: data}
	date := time.Date(2026, 11, 24, 1, 2, 3, 0, time.FixedZone("UTC+8", 8*60*60))
	manifest, err := BuildSeedManifest(source, date)
	if err != nil {
		t.Fatal(err)
	}
	again, err := BuildSeedManifest(source, date.UTC())
	if err != nil || !bytes.Equal(again, manifest) {
		t.Fatalf("same instant produced different manifests: %v", err)
	}
	if !bytes.Equal(source["manifest.yaml"].Data, descriptor) {
		t.Fatal("generation modified the source descriptor")
	}
	source["manifest.yaml"].Data = manifest
	pkg, err := LoadPackage(source)
	if err != nil {
		t.Fatal(err)
	}
	want := PackageManifest{SchemaVersion: 3, PublishedAt: "2026-11-23T17:02:03Z", Models: 2, Specs: 3}
	if pkg.Manifest != want {
		t.Fatalf("manifest = %+v, want %+v", pkg.Manifest, want)
	}
	if version := CatalogVersion(pkg.Manifest.PublishedAt, "1234567890"); version != "2026-11-23.12345678" {
		t.Fatalf("version = %s", version)
	}
}

func TestSeedDescriptorIsNotADistributionManifest(t *testing.T) {
	source := packageFixture(t)
	source["manifest.yaml"].Data = []byte("schema_version: 3\n")
	if _, err := LoadPackage(source); err == nil {
		t.Fatal("strict reader accepted a source descriptor as a published manifest")
	}
	if _, _, err := LoadPackageTolerant(source); err == nil {
		t.Fatal("official reader accepted a source descriptor as a published manifest")
	}
	for _, tc := range []struct {
		text string
		want string
	}{
		{"schema_version: 999\n", "unsupported seed catalog descriptor schema_version 999"},
		{"schema_version: 3\nspecs: 1\n", "parse seed catalog descriptor"},
		{"schema_version: 3\n---\n{}\n", "seed catalog descriptor must contain one document"},
	} {
		source["manifest.yaml"].Data = []byte(tc.text)
		if _, err := BuildSeedManifest(source, time.Unix(0, 0)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("descriptor %q: got %v, want %q", tc.text, err, tc.want)
		}
	}
	source["manifest.yaml"].Data = []byte("schema_version: 3\n")
	delete(source, "models/org/model/assets/serve.py")
	if _, err := BuildSeedManifest(source, time.Unix(0, 0)); err == nil {
		t.Fatal("a missing asset was accepted for publication")
	}
}
