package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

const onlineImageCatalog = `{"schema_version":3,"catalog_version":"2026-10-11.0nl1ne00","series":[{"name":"vLLM · CUDA","engine":"vllm","purpose":"inference","framework":"cuda","versions":[` +
	`{"name":"cuda13.0-vllm0.99.0-r1","image_ref":"gpuworks/gpuworks-runner:cuda13.0-vllm0.99.0-r1","engine_version":"0.99.0","framework_version":"13.0","arch":"multi"}]}]}`

// The official catalogs are last-import-wins (ADR 0040): a restart of the same
// build keeps an online update, and only a new build brings its embedded
// catalogs back.
func TestEmbeddedCatalogsApplyOncePerBuild(t *testing.T) {
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	saved := version.Version
	t.Cleanup(func() { version.Version = saved })
	seedCatalog, err := imagecatalog.Seed()
	if err != nil {
		t.Fatal(err)
	}
	embeddedDigest := func() string {
		_, digest, _, err := seed.Published()
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}()
	start := func() bool {
		t.Helper()
		embedded, err := startEmbeddedCatalogs(st, log)
		if err != nil {
			t.Fatal(err)
		}
		if err := embedded.finish(st); err != nil {
			t.Fatal(err)
		}
		return embedded.due
	}
	state := func() store.OfficialCatalogState {
		t.Helper()
		got, err := st.OfficialCatalogState()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	upstreamDigest := func() string {
		t.Helper()
		digest, err := st.UpstreamInferencePackageDigest()
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}

	version.Version = "rel-1"
	if !start() {
		t.Fatal("a fresh database must apply the embedded catalogs")
	}
	if got := state(); got.SeededBuild != version.String() || got.ImageCatalogVersion != seedCatalog.CatalogVersion {
		t.Fatalf("state after the first start = %+v", got)
	}
	if upstreamDigest() != embeddedDigest {
		t.Fatalf("upstream templates are not the embedded package")
	}

	// An online update of both catalogs.
	if _, err := imagecatalog.ImportManifest(st, []byte(onlineImageCatalog)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordImageCatalogImport("2026-10-11.0nl1ne00"); err != nil {
		t.Fatal(err)
	}
	online := strings.Repeat("ab", 32)
	if err := st.DB().Model(&store.InferenceSpecRow{}).Where("source = ?", "upstream").Update("package_digest", online).Error; err != nil {
		t.Fatal(err)
	}

	if start() {
		t.Fatal("a restart of the same build must not apply the embedded catalogs again")
	}
	if got := state().ImageCatalogVersion; got != "2026-10-11.0nl1ne00" {
		t.Fatalf("image catalog after a restart = %s", got)
	}
	if _, err := st.ImageByRef("gpuworks/gpuworks-runner:cuda13.0-vllm0.99.0-r1"); err != nil {
		t.Fatalf("the online image catalog was rolled back: %v", err)
	}
	if upstreamDigest() != online {
		t.Fatal("the online template catalog was rolled back")
	}

	version.Version = "rel-2"
	if !start() {
		t.Fatal("a new build must apply its embedded catalogs")
	}
	if got := state(); got.SeededBuild != version.String() || got.ImageCatalogVersion != seedCatalog.CatalogVersion {
		t.Fatalf("state after the upgrade = %+v", got)
	}
	if upstreamDigest() != embeddedDigest {
		t.Fatal("the upgrade did not bring the embedded templates back")
	}
}
