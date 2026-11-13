package planning

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A catalog model is one KV figure with several repositories under it, and the
// catalog declares the quantized ones as weight sources. A registered model that
// is one of those repositories has to find its way back to the figure; matching
// on its own id alone cannot, which is how a quantized release reported no KV
// cost while the model it quantizes had one.
func TestCatalogKVBytesPerTokenResolvesDeclaredWeightRepositories(t *testing.T) {
	st, err := store.Open("file:planning_catalog_kv_weight_repo?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	spec := func(weightSources string) []store.InferenceSpecRow {
		return []store.InferenceSpecRow{{
			Name: "nvidia-general", Engine: "vllm", Image: "vllm:test", Quantization: "bf16",
			GPUs: `{"vendor":"nvidia"}`, Args: `[]`, Env: `{}`, WeightSources: weightSources,
		}}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{
		{
			ModelID: "org/base", Source: "upstream", KVBytesPerToken: `{"vllm":262144,"sglang":131072}`,
			// Also declares org/direct, which has a catalog entry of its own: the
			// model's own entry has to win over the declaration.
			Specs: spec(`[{"provider":"huggingface","repo_id":"org/Base"},{"provider":"huggingface","repo_id":"org/Base-NVFP4"},{"provider":"huggingface","repo_id":"org/direct"}]`),
		},
		{
			ModelID: "org/direct", Source: "upstream", KVBytesPerToken: `{"vllm":65536}`,
			Specs: spec(`[{"provider":"huggingface","repo_id":"org/direct"}]`),
		},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		st       *store.Store
		sourceID string
		engine   string
		want     int64
	}{
		// The quantized repository resolves to the model that declares it, with
		// the lookup insensitive to the case the registry uses.
		{name: "declared quantized repository", st: st, sourceID: "ORG/Base-NVFP4", engine: "vllm", want: 262144},
		{name: "declared repository, other engine", st: st, sourceID: "org/base-nvfp4", engine: "sglang", want: 131072},
		{name: "own catalog entry wins over a declaration", st: st, sourceID: "org/direct", engine: "vllm", want: 65536},
		{name: "undeclared repository", st: st, sourceID: "org/Not-Declared-Anywhere", engine: "vllm"},
		// A repository name containing LIKE's wildcards must not match by accident.
		{name: "wildcard repository", st: st, sourceID: "org/Base%", engine: "vllm"},
		{name: "no store", sourceID: "org/Base-NVFP4", engine: "vllm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CatalogKVBytesPerToken(tc.st, nil, &store.Model{SourceID: tc.sourceID}, tc.engine)
			if got != tc.want {
				t.Fatalf("CatalogKVBytesPerToken(%q, %s) = %d, want %d", tc.sourceID, tc.engine, got, tc.want)
			}
		})
	}

	// The fallback runs per candidate node and per rank during a deployment
	// preview, so it must not list the catalog: it reads the specs' weight
	// sources alone and then loads the one model that declares the repository.
	var catalogScans []string
	if err := st.DB().Callback().Query().After("gorm:query").Register("test:catalog_scans", func(tx *gorm.DB) {
		if sql := tx.Statement.SQL.String(); strings.Contains(sql, "FROM `inference_models`") && !strings.Contains(sql, " WHERE ") {
			catalogScans = append(catalogScans, sql)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if got := CatalogKVBytesPerToken(st, nil, &store.Model{SourceID: "org/Base-NVFP4"}, "vllm"); got != 262144 || len(catalogScans) != 0 {
		t.Fatalf("CatalogKVBytesPerToken = %d with catalog scans %q, want 262144 and none", got, catalogScans)
	}
}
