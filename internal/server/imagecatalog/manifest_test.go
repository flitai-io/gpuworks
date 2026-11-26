package imagecatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestManifestValidateAndVerify(t *testing.T) {
	m, err := manifest.Parse([]byte(`{"schema_version":2,"catalog_version":"v3","series":[{"name":"vLLM","engine":"vllm","framework":"cuda","versions":[{"image_ref":"gpustack/runner:cuda12-vllm1"}]}]}`))
	if err != nil || m.CatalogVersion != "v3" {
		t.Fatalf("manifest=%+v err=%v", m, err)
	}
	if _, err := manifest.Parse([]byte(`{"schema_version":2,"catalog_version":"v3","series":[{"name":"x","versions":[{"image_ref":"x/y@sha256:abc"}]}]}`)); err == nil {
		t.Fatal("digest ref should be rejected")
	}
}

func TestManifestHashVerification(t *testing.T) {
	m := manifest.Manifest{
		SchemaVersion:  2,
		CatalogVersion: "v3",
		Series:         []manifest.ManifestSeries{{Name: "x", Versions: []manifest.ManifestVersion{{ImageRef: "x/y:v3"}}}},
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	m.SHA256 = hex.EncodeToString(sum[:])
	raw, _ := json.Marshal(m)
	if parsed, err := manifest.Parse(raw); err != nil || parsed.SHA256 == "" {
		t.Fatalf("hashed manifest=%+v err=%v", parsed, err)
	}
	m.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	bad, _ := json.Marshal(m)
	if _, err := manifest.Parse(bad); err == nil {
		t.Fatal("tampered manifest hash was accepted")
	}
}

func checkImportedDownloadSize(t *testing.T, st *store.Store, schema int, arch string, rolling bool, measured, want int64) {
	t.Helper()
	const ref = "example.org/runner:cann-r1"
	m := manifest.Manifest{SchemaVersion: schema, CatalogVersion: "fixture", Series: []manifest.ManifestSeries{{
		Name: "fixture", Engine: "vllm", Framework: "cann", Versions: []manifest.ManifestVersion{{
			ImageRef: ref, Arch: arch, RollingTag: rolling, DownloadBytes: measured,
		}},
	}}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportManifest(st, body); err != nil {
		t.Fatal(err)
	}
	row, err := st.ImageByRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if row.DownloadBytes != want {
		t.Fatalf("after schema %d import with %d bytes: got %d, want %d", schema, measured, row.DownloadBytes, want)
	}
}

func TestCatalogDownloadSizeCorrection(t *testing.T) {
	run := func(t *testing.T, seed bool) {
		st, err := store.Open(":memory:", bus.New())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		apply := func(schema int, measured, want int64) {
			t.Helper()
			checkImportedDownloadSize(t, st, schema, "amd64", false, measured, want)
		}
		if seed {
			apply(4, 1234, 1234)
			apply(2, 0, 1234)
			apply(3, 0, 1234)
			apply(4, 5678, 5678)
		}
		apply(4, 0, 0)
		apply(4, 0, 0)
	}
	t.Run("previously measured", func(t *testing.T) { run(t, true) })
	t.Run("new database", func(t *testing.T) { run(t, false) })
}

func TestCatalogDownloadSizeValidation(t *testing.T) {
	for _, tt := range []struct {
		schema int
		value  int64
		reason string
	}{
		{2, 0, ""}, {3, 0, ""}, {4, 0, ""}, {4, 7, ""},
		{4, -1, "must be non-negative"},
		{2, 7, "requires schema 4"}, {3, 7, "requires schema 4"},
	} {
		t.Run(fmt.Sprintf("schema%d_%d", tt.schema, tt.value), func(t *testing.T) {
			data := fmt.Sprintf(`{"schema_version":%d,"catalog_version":"test","series":[{"name":"fixture","versions":[{"image_ref":"x/y:r1","download_size_bytes":%d}]}]}`, tt.schema, tt.value)
			_, err := manifest.Parse([]byte(data))
			if tt.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.reason) || !strings.Contains(err.Error(), "x/y:r1") {
				t.Fatalf("expected %q for x/y:r1, got %v", tt.reason, err)
			}
		})
	}
}

func TestLegacyImportDropsDownloadSizeWhenTagOrPlatformChanges(t *testing.T) {
	for _, tt := range []struct {
		name                             string
		existingRolling, incomingRolling bool
		newArch                          string
	}{
		{"existing rolling tag", true, false, "amd64"},
		{"incoming rolling tag", false, true, "amd64"},
		{"changed architecture", false, false, "arm64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open(":memory:", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			checkImportedDownloadSize(t, st, 4, "amd64", tt.existingRolling, 1234, 1234)
			checkImportedDownloadSize(t, st, 3, tt.newArch, tt.incomingRolling, 0, 0)
		})
	}
}
