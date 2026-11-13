package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestFlatInferenceCatalogMigrationDropsRetiredGraph(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-catalog.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, table := range []string{"inference_models", "inference_specs"} {
		if !st.DB().Migrator().HasTable(table) {
			t.Fatalf("missing flat catalog table %q", table)
		}
	}
	if !st.DB().Migrator().HasColumn(&Deployment{}, "template_spec_name") {
		t.Fatal("deployments missing flat template_spec_name provenance")
	}
	if !st.DB().Migrator().HasColumn(&Deployment{}, "template_spec_source") {
		t.Fatal("deployments missing flat template_spec_source provenance")
	}
	if st.DB().Migrator().HasColumn("deployments", "template_profile_key") {
		t.Fatal("deployments retains retired template_profile_key provenance")
	}
	for _, column := range []string{"model_id", "source", "readme_zh_cn", "readme_en", "package_digest", "unsupported", "kv_bytes_per_token"} {
		if !st.DB().Migrator().HasColumn(&InferenceModel{}, column) {
			t.Fatalf("inference_models missing column %q", column)
		}
	}
	if st.DB().Migrator().HasColumn(&InferenceModel{}, "kinds") {
		t.Fatal("retired inference_models.kinds column survived cutover")
	}
	if st.DB().Migrator().HasColumn(&InferenceSpecRow{}, "display_profile") {
		t.Fatal("retired inference_specs.display_profile column survived")
	}
	for _, column := range []string{"model_id", "source", "name", "package_digest", "display_name_zh_cn", "display_name_en", "weight_sources", "gpus", "pre_start_argv", "pre_start_script", "assets", "entrypoint", "placement", "api_contract", "components", "image_sources"} {
		if !st.DB().Migrator().HasColumn(&InferenceSpecRow{}, column) {
			t.Fatalf("inference_specs missing column %q", column)
		}
	}
	for _, table := range []string{
		"inference_templates", "template_variants", "chip_supports", "template_profiles",
		"template_evidence", "validation_runs", "chip_registry", "chip_classes",
	} {
		if st.DB().Migrator().HasTable(table) {
			t.Fatalf("retired catalog table %q remains", table)
		}
	}
	var marker SchemaMigration
	if err := st.DB().First(&marker, 66).Error; err != nil || marker.Name != "flat_inference_spec_catalog" {
		t.Fatalf("flat catalog migration marker = %+v, err=%v", marker, err)
	}
	marker = SchemaMigration{}
	if err := st.DB().First(&marker, 67).Error; err != nil || marker.Name != "inference_model_kinds" {
		t.Fatalf("kind migration marker = %+v, err=%v", marker, err)
	}
	marker = SchemaMigration{}
	if err := st.DB().First(&marker, 68).Error; err != nil || marker.Name != "inference_spec_display_profile" {
		t.Fatalf("display profile migration marker = %+v, err=%v", marker, err)
	}
	marker = SchemaMigration{}
	if err := st.DB().First(&marker, 69).Error; err != nil || marker.Name != "executable_inference_catalog_contracts" {
		t.Fatalf("executable catalog migration marker = %+v, err=%v", marker, err)
	}
	marker = SchemaMigration{}
	if err := st.DB().First(&marker, 72).Error; err != nil || marker.Name != "inference_spec_source_identity" {
		t.Fatalf("spec source identity migration marker = %+v, err=%v", marker, err)
	}
	if err := st.migrate(); err != nil {
		t.Fatalf("flat catalog migration is not idempotent: %v", err)
	}
	// Simulate a v65 database that still has the retired graph and ensure the
	// cutover removes it atomically on upgrade.
	for _, table := range []string{"validation_runs", "template_evidence", "template_profiles", "chip_supports", "template_variants", "inference_templates", "chip_classes", "chip_registry"} {
		if err := st.DB().Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY)").Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB().Exec("ALTER TABLE deployments ADD COLUMN template_profile_key TEXT").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Exec("CREATE INDEX idx_deployments_template_profile_key ON deployments(template_profile_key)").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Delete(&SchemaMigration{}, 66).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(); err != nil {
		t.Fatalf("flat catalog cutover failed: %v", err)
	}
	for _, table := range []string{"validation_runs", "template_evidence", "template_profiles", "chip_supports", "template_variants", "inference_templates", "chip_classes", "chip_registry"} {
		if st.DB().Migrator().HasTable(table) {
			t.Fatalf("retired table %q survived cutover", table)
		}
	}
	if st.DB().Migrator().HasColumn("deployments", "template_profile_key") {
		t.Fatal("retired template_profile_key survived cutover")
	}
	var retiredIndexes int64
	if err := st.DB().Raw("SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'idx_deployments_template_profile_key'").Scan(&retiredIndexes).Error; err != nil {
		t.Fatal(err)
	}
	if retiredIndexes != 0 {
		t.Fatal("retired template_profile_key index survived cutover")
	}
}

func TestInferenceSpecDisplayProfileMigrationRollsBackColumnAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:inference-spec-display-profile-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected failure after display profile column")
	err = applyMigration(db, migration{Version: 9068, Name: "display-profile-failure", Apply: func(tx *gorm.DB) error {
		if err := tx.Migrator().AddColumn(&inferenceSpecDisplayProfileColumn{}, "DisplayProfile"); err != nil {
			return err
		}
		return want
	}})
	if !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	if db.Migrator().HasColumn(&inferenceSpecDisplayProfileColumn{}, "DisplayProfile") {
		t.Fatal("display_profile column survived rollback")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 9068).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("migration marker count=%d err=%v", markers, err)
	}
}

func TestFlatInferenceCatalogMigrationRollsBackCutoverAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:flat-catalog-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"inference_templates", "template_variants", "chip_supports", "template_profiles", "template_evidence", "validation_runs", "chip_registry", "chip_classes"} {
		if err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY)").Error; err != nil {
			t.Fatal(err)
		}
	}
	want := errors.New("injected failure after flat cutover")
	err = applyMigration(db, migration{Version: 9066, Name: "flat-cutover-failure", Apply: func(tx *gorm.DB) error {
		if err := migrateFlatInferenceCatalog(tx); err != nil {
			return err
		}
		return want
	}})
	if !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	for _, table := range []string{"inference_templates", "template_variants", "chip_supports", "template_profiles", "template_evidence", "validation_runs", "chip_registry", "chip_classes"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("retired table %s was not restored by rollback", table)
		}
	}
	for _, table := range []string{"inference_models", "inference_specs"} {
		if db.Migrator().HasTable(table) {
			t.Fatalf("new table %s survived failed transaction", table)
		}
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 9066).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration marker count=%d err=%v", markers, err)
	}
}

func TestReconcileFlatInferenceCatalogReplacesSpecs(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-reconcile.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model := InferenceModel{ModelID: "Org/Model", DisplayTitle: "Model", Source: "upstream", Unsupported: `[]`, Specs: []InferenceSpecRow{
		{Name: "balanced", DisplayNameZHCN: " Ada 及以上 · BF16 ", DisplayNameEN: "Ada+ · BF16", Engine: "VLLM", Image: "vllm:test", Quantization: "bf16", WeightSources: `[{"provider":"huggingface","repo_id":"Org/Model"}]`, GPUs: `{"vendor":"nvidia","cc":">=8.9","display_target":"nvidia"}`, Args: `[]`, Env: `{}`},
		{Name: "throughput", Engine: "vLLM", Image: "vllm:test", Quantization: "bf16", WeightSources: `[{"provider":"modelscope","repo_id":"Org/Model"}]`, GPUs: `{"vendor":"nvidia"}`, Args: `[]`, Env: `{}`},
	}}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	model.Specs = model.Specs[:1]
	model.Specs[0].Image = "vllm:new"
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InferenceModelByModelID("ORG/MODEL")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Specs) != 1 || got.Specs[0].Name != "balanced" || got.Specs[0].Image != "vllm:new" || got.Specs[0].DisplayNameZHCN != "Ada 及以上 · BF16" || got.Specs[0].DisplayNameEN != "Ada+ · BF16" {
		t.Fatalf("reconciled model = %+v", got)
	}
}

func TestReconcileFlatInferenceCatalogRemovesStaleUpstreamKeepsUser(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-reconcile-stale.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	row := func(id, source string) InferenceModel {
		return InferenceModel{ModelID: id, Source: source, Specs: []InferenceSpecRow{{Name: "default", Engine: "vllm", Image: "test:image", Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`}}}
	}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{row("org/stale", "upstream"), row("org/user", "user")}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{row("org/new", "upstream")}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InferenceModelByModelID("org/stale"); err != ErrNotFound {
		t.Fatalf("stale upstream model error = %v", err)
	}
	if _, err := st.InferenceModelByModelID("org/user"); err != nil {
		t.Fatalf("user model was removed: %v", err)
	}
}

func TestEmbeddedUpgradeKeepsSameNamedUserSpecSeparatedBySource(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-source-identity.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const upstreamDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const userDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	initial := InferenceModel{ModelID: "org/shared", Source: "upstream", DisplayTitle: "Built-in v1", PackageDigest: upstreamDigest, Specs: []InferenceSpecRow{
		{Name: "general", Engine: "vllm", Image: "upstream/image:v1", Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`},
		{Name: "removed-in-v2", Engine: "sglang", Image: "upstream/old:v1", Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`},
	}}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{initial}); err != nil {
		t.Fatal(err)
	}
	user := InferenceModel{ModelID: initial.ModelID, Source: "user", PackageDigest: userDigest, Specs: []InferenceSpecRow{{
		Name: "general", Engine: "vllm", Image: "user/image:stable", Quantization: "int8", WeightSources: `[]`, GPUs: `{}`,
	}}}
	if err := st.ImportUserInferenceCatalog([]InferenceModel{user}, false); err != nil {
		t.Fatal(err)
	}
	upgraded := InferenceModel{ModelID: initial.ModelID, Source: "upstream", DisplayTitle: "Built-in v2", PackageDigest: upstreamDigest, Specs: []InferenceSpecRow{
		{Name: "general", Engine: "vllm", Image: "upstream/image:v2", Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`},
		{Name: "added-in-v2", Engine: "sglang", Image: "upstream/new:v2", Quantization: "bf16", WeightSources: `[]`, GPUs: `{}`},
	}}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{upgraded}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InferenceModelByModelID(initial.ModelID)
	if err != nil || got.Source != "upstream" || got.DisplayTitle != "Built-in v2" || len(got.Specs) != 3 {
		t.Fatalf("upgraded model=%+v err=%v", got, err)
	}
	byIdentity := map[string]InferenceSpecRow{}
	for _, spec := range got.Specs {
		byIdentity[spec.Source+"/"+spec.Name] = spec
	}
	if byIdentity["upstream/general"].Image != "upstream/image:v2" || byIdentity["upstream/added-in-v2"].Image != "upstream/new:v2" {
		t.Fatalf("upstream specs=%+v", byIdentity)
	}
	if byIdentity["user/general"].Image != "user/image:stable" || byIdentity["user/general"].PackageDigest != userDigest {
		t.Fatalf("user spec changed during upgrade: %+v", byIdentity["user/general"])
	}
	if _, stale := byIdentity["upstream/removed-in-v2"]; stale {
		t.Fatalf("removed upstream spec survived: %+v", byIdentity)
	}
}

func TestImportUserInferenceCatalogRequiresExplicitSameNameOverwrite(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-user-import.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	row := func(name, engine, image string) InferenceModel {
		return InferenceModel{ModelID: "org/custom", Source: "user", Specs: []InferenceSpecRow{{Name: name, Engine: engine, Image: image, Quantization: "bf16", WeightSources: `[{"provider":"huggingface","repo_id":"org/custom"}]`, GPUs: `{"vendor":"nvidia"}`, Args: `[]`, Env: `{}`}}}
	}
	if err := st.ImportUserInferenceCatalog([]InferenceModel{row("general", "vllm", "vllm:one")}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportUserInferenceCatalog([]InferenceModel{row("throughput", "vllm", "vllm:throughput")}, false); err != nil {
		t.Fatalf("second same-engine spec import: %v", err)
	}
	if err := st.ImportUserInferenceCatalog([]InferenceModel{row("general", "vllm", "vllm:two")}, false); !errors.Is(err, ErrInferenceCatalogConflict) {
		t.Fatalf("duplicate spec error=%v", err)
	}
	if err := st.ImportUserInferenceCatalog([]InferenceModel{row("general", "vllm", "vllm:two")}, true); err != nil {
		t.Fatalf("explicit overwrite: %v", err)
	}
	model, err := st.InferenceModelByModelID("org/custom")
	if err != nil {
		t.Fatal(err)
	}
	if model.Source != "user" || len(model.Specs) != 2 {
		t.Fatalf("imported model=%+v", model)
	}
	images := map[string]string{}
	for _, spec := range model.Specs {
		images[spec.Name] = spec.Image
	}
	if images["general"] != "vllm:two" || images["throughput"] != "vllm:throughput" {
		t.Fatalf("spec images=%v", images)
	}
}

// The reconcile update path names its columns explicitly, so a catalog field
// that is not on that list reaches a fresh database - where Create fills every
// column - and silently skips every row that already exists. A real upgrade is
// always the second case, which is how the deployed control plane ended up with
// the column and no values in it.
func TestReconcileFlatInferenceCatalogUpdatesKVBytesPerToken(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-reconcile-kv.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model := InferenceModel{ModelID: "Org/Model", DisplayTitle: "Model", Source: "upstream", Unsupported: `[]`}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	model.KVBytesPerToken = `{"vllm":262144,"sglang":262144}`
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InferenceModelByModelID("ORG/MODEL")
	if err != nil {
		t.Fatal(err)
	}
	if got.KVBytesPerToken != model.KVBytesPerToken {
		t.Fatalf("kv_bytes_per_token = %q after reconcile, want %q", got.KVBytesPerToken, model.KVBytesPerToken)
	}
}

// A spec's image sources are stored with it, replaced when the catalog is
// imported again, cleared when the new document declares none, and refused when
// the column is not JSON.
func TestReconcileFlatInferenceCatalogStoresAndReplacesImageSources(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "flat-reconcile-image-sources.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model := InferenceModel{ModelID: "Org/Model", DisplayTitle: "Model", Source: "upstream", Unsupported: `[]`, Specs: []InferenceSpecRow{
		{Name: "balanced", Engine: "vllm", Image: "ghcr.io/org/runner@sha256:aaaa", ImageSources: `["swr.example.com/org/runner@sha256:aaaa"]`, Quantization: "bf16",
			WeightSources: `[{"provider":"huggingface","repo_id":"Org/Model"}]`, GPUs: `{"vendor":"nvidia"}`},
	}}
	read := func() string {
		t.Helper()
		got, err := st.InferenceModelByModelID("org/model")
		if err != nil || len(got.Specs) != 1 {
			t.Fatalf("model = %+v err=%v", got, err)
		}
		return got.Specs[0].ImageSources
	}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != `["swr.example.com/org/runner@sha256:aaaa"]` {
		t.Fatalf("stored image_sources = %q", got)
	}
	model.Specs[0].ImageSources = `["ghcr.nju.edu.cn/org/runner:v1","swr.example.com/org/runner@sha256:aaaa"]`
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != model.Specs[0].ImageSources {
		t.Fatalf("replaced image_sources = %q", got)
	}
	model.Specs[0].ImageSources = ""
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "" {
		t.Fatalf("an import without sources left %q behind", got)
	}
	model.Specs[0].ImageSources = "not json"
	if err := st.ReconcileInferenceCatalog([]InferenceModel{model}); err == nil || !strings.Contains(err.Error(), "image_sources is invalid JSON") {
		t.Fatalf("a non-JSON image_sources column was accepted: %v", err)
	}
	if got := read(); got != "" {
		t.Fatalf("the refused import changed the stored value to %q", got)
	}
}

// The reverse lookup from a weight repository to its catalog model runs per
// candidate node and per rank during a deployment preview. It reads two columns
// of every spec in one query, never the whole catalog with every spec's text
// columns: the full load cost ~40 ms per lookup on the embedded catalog.
func TestListInferenceSpecWeightSourcesReadsOnlyTwoColumns(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "spec-weight-sources.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	spec := func(name, weightSources string) InferenceSpecRow {
		return InferenceSpecRow{Name: name, Engine: "vllm", Image: "vllm:test", Quantization: "bf16",
			WeightSources: weightSources, GPUs: `{"vendor":"nvidia"}`, Args: `[]`, Env: `{}`}
	}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{
		{ModelID: "org/a", Source: "upstream", Specs: []InferenceSpecRow{
			spec("one", `[{"provider":"huggingface","repo_id":"org/A"}]`),
			spec("two", `[{"provider":"modelscope","repo_id":"org/A-Int8"}]`),
		}},
		{ModelID: "org/b", Source: "upstream", Specs: []InferenceSpecRow{spec("one", `[]`)}},
	}); err != nil {
		t.Fatal(err)
	}
	models, err := st.ListInferenceModels()
	if err != nil || len(models) != 2 {
		t.Fatalf("ListInferenceModels = %+v, %v", models, err)
	}
	want := map[InferenceSpecWeightSources]bool{}
	for _, model := range models {
		for _, spec := range model.Specs {
			want[InferenceSpecWeightSources{ModelID: model.ID, WeightSources: spec.WeightSources}] = true
		}
	}

	var queries []string
	if err := st.db.Callback().Query().After("gorm:query").Register("test:capture_sql", func(tx *gorm.DB) {
		queries = append(queries, tx.Statement.SQL.String())
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListInferenceSpecWeightSources()
	if err != nil {
		t.Fatal(err)
	}
	if wantSQL := "SELECT `model_id`,`weight_sources` FROM `inference_specs`"; len(queries) != 1 || queries[0] != wantSQL {
		t.Fatalf("queries = %q, want exactly [%q]", queries, wantSQL)
	}
	got := map[InferenceSpecWeightSources]bool{}
	for _, row := range rows {
		got[row] = true
	}
	if len(rows) != 3 || len(got) != len(want) {
		t.Fatalf("rows = %+v, want %v", rows, want)
	}
	for row := range want {
		if !got[row] {
			t.Fatalf("rows = %+v, missing %+v", rows, row)
		}
	}
}
