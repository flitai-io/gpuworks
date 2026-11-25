package imagecatalog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The server seeds the official catalog on every start, writing each series'
// common_args/common_env. A database from the benchmark branch (37, 38 and 40
// recorded under benchmark names) lacked those columns, so the start failed.
func TestSyncSeedAfterBenchmarkLineageV40Upgrade(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "store", "testdata", "benchmark_lineage_v40.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		if err := db.Exec(line).Error; err != nil {
			t.Fatalf("fixture statement %q: %v", line, err)
		}
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(path, bus.New())
	if err != nil {
		t.Fatalf("open benchmark-lineage database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	result, err := SyncSeed(st)
	if err != nil {
		t.Fatalf("seed after upgrade: %v", err)
	}
	if result.Created == 0 {
		t.Fatalf("seed created no series or images: %+v", result)
	}
	series, err := st.ListImageSeries()
	if err != nil {
		t.Fatal(err)
	}
	withArgs := 0
	for _, row := range series {
		if row.CommonArgs != "" && row.CommonArgs != "[]" {
			withArgs++
		}
	}
	if withArgs == 0 {
		t.Fatalf("no seeded series kept its common args across %d series", len(series))
	}
}

func TestRenamedRunnerPreservesOperatorChoices(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "upgrade.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const prefix = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:"
	const single = prefix + "xre-vllm0.25.1-amd64-omni-slot26"
	const multi = prefix + "cuda13.0-vllm0.29.0-r1"
	const singleNew = prefix + "xre-vllm0.25.1-omni-r1"
	const arm = prefix + "cuda13.0-vllm0.29.0-arm64-slot01"
	const x86 = prefix + "cuda13.0-vllm0.29.0-amd64-slot01"
	const bare = prefix + "cuda13.0-vllm0.30.0"
	const historical = bare + "-r0"
	legacy, err := manifest.LegacyCatalog(SeedJSON())
	if err != nil {
		t.Fatal(err)
	}
	projected, err := manifest.Parse(legacy)
	if err != nil || projected.SchemaVersion != 3 {
		t.Fatalf("legacy schema=%d err=%v", projected.SchemaVersion, err)
	}
	var legacyBare manifest.ManifestVersion
	for _, s := range projected.Series {
		for _, v := range s.Versions {
			if v.ImageRef == bare {
				legacyBare = v
			}
			if v.DownloadBytes != 0 || v.SizeBytes != 0 {
				t.Fatalf("legacy disk requirement for %s = %d/%d", v.ImageRef, v.DownloadBytes, v.SizeBytes)
			}
		}
	}
	if legacyBare.Arch != "multi" || !legacyBare.RollingTag || len(legacyBare.RegistrySources) != 1 || legacyBare.RegistrySources[0] != historical {
		t.Fatalf("legacy unsuffixed runner = %+v", legacyBare)
	}
	// Start with only the old refs, as on a server that never saw the new tags.
	for _, tc := range []struct {
		ref, arch, src string
		enabled        bool
	}{
		{single, "amd64", "https://cache.invalid/kunlun.tar", false},
		{bare, "multi", "https://cache.invalid/both.tar", false},
		{arm, "arm64", "https://cache.invalid/arm.tar", false},
		{x86, "amd64", "https://cache.invalid/amd.tar", true},
	} {
		old, err := st.UpsertImage(&store.Image{ImageRef: tc.ref, Arch: tc.arch, Source: "official", Name: "old", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateImageRegistrySources(old.ID, `["`+tc.src+`"]`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetImageEnabled(old.ID, tc.enabled); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	dst, err := st.ImageByRef(singleNew)
	if err != nil || dst.Enabled || !strings.Contains(dst.RegistrySources, "kunlun.tar") || dst.SizeBytes != 0 || dst.DownloadBytes != 42027032085 {
		t.Fatalf("single-platform successor=%+v err=%v", dst, err)
	}
	if _, err := st.ImageByRef(single); err != store.ErrNotFound {
		t.Fatalf("obsolete same-arch row: %v", err)
	}
	r0, err := st.ImageByRef(historical)
	if err != nil || r0.Enabled || r0.Arch != "multi" || r0.RegistrySources != `["https://cache.invalid/both.tar"]` {
		t.Fatalf("historical multi-platform successor=%+v err=%v", r0, err)
	}
	if _, err := st.ImageByRef(bare); err != store.ErrNotFound {
		t.Fatalf("obsolete unsuffixed row: %v", err)
	}
	m, err := st.ImageByRef(multi)
	if err != nil || m.Enabled || strings.Contains(m.RegistrySources, "cache.invalid") {
		t.Fatalf("multi-platform successor=%+v err=%v", m, err)
	}
	for ref, archive := range map[string]string{arm: "arm.tar", x86: "amd.tar"} {
		old, err := st.ImageByRef(ref)
		if err != nil || old.Deprecated || !strings.Contains(old.RegistrySources, archive) {
			t.Fatalf("architecture-bound archive=%+v err=%v", old, err)
		}
	}
	if _, err := st.SetImageEnabled(m.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	m, err = st.ImageByRef(multi)
	if err != nil || !m.Enabled {
		t.Fatalf("repeated import lost switch: %+v %v", m, err)
	}
}

func TestAddingPublishedRepositoriesPreservesExistingImage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "copies.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const tag = "xre-vllm0.25.1-omni-r1"
	const ref = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:" + tag
	const archive = "https://cache.invalid/kunlun.tar"
	old, err := st.UpsertImage(&store.Image{Name: tag, ImageRef: ref, Source: "official", Arch: "amd64", Enabled: false, RegistrySources: encodeSources(t, []string{archive})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	row, err := st.ImageByRef(ref)
	if err != nil || row.ID != old.ID || row.Enabled {
		t.Fatalf("existing catalog image changed: %+v, %v", row, err)
	}
	mirrors := []string{"quay.io/gpuworks/gpuworks-runner:" + tag, "gpuworks/gpuworks-runner:" + tag}
	if got := decodeSources(t, row.RegistrySources); !slices.Equal(got, append(slices.Clone(mirrors), archive)) {
		t.Fatalf("unexpected sources after upgrade: %v", got)
	}
	for _, mirror := range mirrors {
		if _, err := st.ImageByRef(mirror); err != store.ErrNotFound {
			t.Fatalf("mirror %s became a separate image: %v", mirror, err)
		}
	}
}
