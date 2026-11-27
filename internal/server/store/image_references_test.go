package store

import (
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestImageReferenceCountsBatchesAndUsesIndexes(t *testing.T) {
	st, err := Open("file:image_reference_counts?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	refs := make([]string, 0, 1200)
	for i := 0; i < 1200; i++ {
		refs = append(refs, fmt.Sprintf("example/batch:%d", i))
	}
	// One reference per batch of 500, plus an instance state on each side of
	// the "unfinished" line.
	for _, i := range []int{0, 600, 1199} {
		if err := st.db.Create(&Deployment{Name: fmt.Sprintf("d%d", i), State: "running", Image: refs[i]}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for state, ref := range map[string]string{
		proto.InstRunning:        refs[600],
		proto.InstStopped:        refs[1],
		proto.InstFailed:         refs[2],
		proto.InstPendingRemoval: refs[3],
	} {
		if err := st.db.Create(&Instance{UUID: "inst-" + state, State: state, Image: ref}).Error; err != nil {
			t.Fatal(err)
		}
	}
	counts, err := st.ImageReferenceCounts(refs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{refs[0]: 1, refs[600]: 2, refs[1199]: 1}
	if len(counts) != len(want) {
		t.Fatalf("counts = %v, want %v", counts, want)
	}
	for ref, n := range want {
		if counts[ref] != n {
			t.Fatalf("counts = %v, want %v", counts, want)
		}
	}

	// Each step of the three counts is an index search, never a scan.
	for _, query := range []string{
		"EXPLAIN QUERY PLAN SELECT image, COUNT(*) FROM deployments WHERE image IN ('a','b') GROUP BY image",
		"EXPLAIN QUERY PLAN SELECT image, COUNT(*) FROM instances WHERE image IN ('a','b') AND state NOT IN ('stopped','failed','pending_removal') GROUP BY image",
		"EXPLAIN QUERY PLAN SELECT image_versions.image_ref, COUNT(*) FROM models JOIN image_versions ON image_versions.id = models.image_id WHERE image_versions.image_ref IN ('a','b') GROUP BY image_versions.image_ref",
	} {
		var plan []struct {
			Detail string `gorm:"column:detail"`
		}
		if err := st.db.Raw(query).Scan(&plan).Error; err != nil {
			t.Fatal(err)
		}
		for _, step := range plan {
			if !strings.HasPrefix(step.Detail, "SEARCH ") {
				t.Fatalf("%s\nplan step %q scans a table", query, step.Detail)
			}
		}
	}
}

func TestImageCatalogSimplifyMigrationIsIdempotent(t *testing.T) {
	st, err := Open("file:image_catalog_simplify_migration?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSetting("image_catalog_version", `"v2"`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateImageCatalogSimplify(st.db); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.GetSetting("image_catalog_version"); got != "" {
		t.Fatalf("image_catalog_version = %q, want removed", got)
	}
	var indexes []string
	st.db.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND name IN ('idx_deployments_image','idx_instances_image') ORDER BY name").Scan(&indexes)
	if strings.Join(indexes, ",") != "idx_deployments_image,idx_instances_image" {
		t.Fatalf("indexes = %v", indexes)
	}
	var marker SchemaMigration
	if err := st.db.Where("name = ?", "image_catalog_simplify").First(&marker).Error; err != nil {
		t.Fatalf("migration marker: %v", err)
	}
}

func TestReconcilePreservesInstalledTemplateMirror(t *testing.T) {
	st, err := Open("file:installed-template-pin?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const canonical = "gpuworks/gpuworks-runner:old-r1"
	const mirror = "quay.io/" + canonical
	row, err := st.UpsertImage(&Image{ImageRef: canonical, Source: "official", Enabled: true, RegistrySources: `["` + mirror + `"]`})
	if err != nil {
		t.Fatal(err)
	}
	model := InferenceModel{ModelID: "org/held", Source: "upstream"}
	if err = st.DB().Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	spec := InferenceSpecRow{ModelID: model.ID, Source: "upstream", Name: "old", Engine: "vllm", Image: mirror, WeightSources: "[]", GPUs: "{}"}
	if err = st.DB().Create(&spec).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err = st.ReconcileMissingOfficialImages(nil, "next"); err != nil {
			t.Fatal(err)
		}
		got, err := st.ResolveImageRef(mirror)
		if err != nil || got.ID != row.ID || got.AutoSelect != "manual" || !got.Enabled || got.Deprecated || got.ImageRef != canonical {
			t.Fatalf("retained row %+v: %v", got, err)
		}
	}
	if err = st.DB().Delete(&spec).Error; err != nil {
		t.Fatal(err)
	}
	if deleted, _, err := st.ReconcileMissingOfficialImages(nil, "later"); err != nil || deleted != 1 {
		t.Fatalf("dropped template: deleted %d: %v", deleted, err)
	}
}

func TestReconcileWithdrawnImagesWithAliasesAndMalformedSources(t *testing.T) {
	st, err := Open("file:withdrawn-aliases?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const repo = "gpuworks/gpuworks-runner:"
	model := InferenceModel{ModelID: "org/model", Source: "upstream"}
	if err := st.DB().Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"upstream", "user", "broken", "unused"} {
		img, err := st.UpsertImage(&Image{ImageRef: repo + tag, Enabled: true, Source: "official"})
		if err != nil {
			t.Fatal(err)
		}
		switch tag {
		case "broken":
			if err := st.DB().Model(img).Update("registry_sources", "{").Error; err != nil {
				t.Fatal(err)
			}
		case "upstream", "user":
			spec := InferenceSpecRow{ModelID: model.ID, Source: tag, Name: tag, Engine: "vllm", Image: "docker.io/" + repo + tag, WeightSources: "[]", GPUs: "{}"}
			if err := st.DB().Create(&spec).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	deleted, dep, err := st.ReconcileMissingOfficialImages(nil, "next")
	if err != nil || deleted != 1 || dep != 1 {
		t.Fatalf("deleted=%d deprecated=%d err=%v", deleted, dep, err)
	}
	for _, tag := range []string{"upstream", "user", "broken"} {
		got, err := st.ResolveImageRef("docker.io/" + repo + tag)
		if err != nil || got.Deprecated != (tag == "user") || (got.AutoSelect == "manual") != (tag == "upstream") {
			t.Fatalf("%s: %+v %v", tag, got, err)
		}
	}
	if _, err := st.ImageByRef(repo + "unused"); err != ErrNotFound {
		t.Fatalf("unused: %v", err)
	}
}
