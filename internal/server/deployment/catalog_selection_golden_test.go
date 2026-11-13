package deployment

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// catalogSelectionGoldens holds the stored form of the two classes of create
// request the writer distinguishes, each captured from the writer before the
// snapshot became a typed value (`git worktree add <dir> 2158af84`, then the
// same probe that is this test): a deployment without a catalog template and
// a deployment from one. The snapshot is a store column four packages read: a
// rewrite that changes its bytes rewrites history. Where a node gets the model
// and the image from is not part of the snapshot.
var catalogSelectionGoldens = []struct {
	name string
	path string
}{
	{"custom model", "testdata/catalog_selection_custom_model.golden"},
	{"template", "testdata/catalog_selection_template.golden"},
}

// catalogSelectionGoldenEnv regenerates the fixtures instead of comparing to them.
const catalogSelectionGoldenEnv = "GPUWORKS_UPDATE_CATALOG_SELECTION_GOLDEN"

func TestCatalogSelectionSnapshotBytesAreStable(t *testing.T) {
	// The create path of each class: the selection the api builds and the
	// image the deployment runs.
	snapshots := map[string]func() (string, error){
		"custom model": func() (string, error) {
			return catalogSelectionDocument(templateSelection{}, "custom/image:1")
		},
		"template": func() (string, error) {
			return catalogSelectionDocument(catalogSelectionFixture(), "registry.example/org/spec@sha256:abc")
		},
	}
	for _, golden := range catalogSelectionGoldens {
		t.Run(golden.name, func(t *testing.T) {
			snapshot, err := snapshots[golden.name]()
			if err != nil {
				t.Fatal(err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, []byte(snapshot), "", "  "); err != nil {
				t.Fatalf("snapshot is not JSON: %v\n%s", err, snapshot)
			}
			got := append(bytes.TrimRight(pretty.Bytes(), "\n"), '\n')
			if os.Getenv(catalogSelectionGoldenEnv) != "" {
				if err := os.MkdirAll(filepath.Dir(golden.path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden.path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden.path)
			if err != nil {
				t.Fatalf("read fixture (regenerate with %s=1): %v", catalogSelectionGoldenEnv, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s changed:\n--- want ---\n%s\n--- got ---\n%s", golden.path, want, got)
			}
		})
	}
}

// catalogSelectionFixture is rich on purpose: every field the writer can emit
// is set, so the fixture pins the whole document rather than the happy path.
func catalogSelectionFixture() templateSelection {
	return templateSelection{
		Model: &store.InferenceModel{ID: 7, ModelID: "org/fixture-8b", Unsupported: `["ppu"]`},
		Spec: &store.InferenceSpecRow{
			Source: "upstream", Name: "balanced", Scenario: "chat", Engine: "vllm", Quantization: "fp8",
			WeightBytes: 8589934592, RuntimeWeightBytes: 7516192768,
			GPUs: `{"min_count":1,"vendors":["nvidia"]}`,
		},
		Weight:         weightSource{Provider: "huggingface", RepoID: "org/fixture-8b", Revision: "main", Subfolder: "weights"},
		ExtraArgs:      []string{"--dtype", "bfloat16"},
		Env:            map[string]string{"VLLM_LOGGING_LEVEL": "INFO"},
		PreStartArgv:   []string{"--prepare"},
		PreStartScript: "set -a\nexport GPUWORKS_PREPARED=1",
		Assets:         []proto.RuntimeAsset{{File: "serve.py", Content: []byte("print(1)")}},
		Entrypoint:     []string{"vllm", "serve", "/model"},
		Placement:      json.RawMessage(`{"language":"starlark","resolver":"def resolve(gpus):\n  return gpus"}`),
		APIContract:    json.RawMessage(`{"operations":[{"type":"openai.chat"}]}`),
		Components: []deploy.ComponentBinding{{
			Name: "vision", Role: "encoder", Provider: "huggingface", RepoID: "org/vision",
			MountPath: "/vision", StoreModelID: 9, WeightBytes: 1073741824,
		}},
		MatchedBy: "spec",
		Diff:      []string{"params"},
	}
}
