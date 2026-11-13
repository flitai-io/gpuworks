package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// go test ./internal/server/api -run TestImportStorageModelsReportsPathConflict -count=1
func TestImportStorageModelsReportsPathConflict(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			r := newSaveToRig(t)
			root := t.TempDir()
			rel := "org/model"
			for _, directory := range []string{rel, "org/other"} {
				dir := filepath.Join(root, directory)
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				for name, body := range map[string]string{"config.json": `{"architectures":["Qwen3ForCausalLM"]}`, "model.safetensors": "weights"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := r.st.ApplyStorageStatus(r.node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: r.nfs.ID, State: proto.StorageStateReady, Path: root, Writable: true}}}); err != nil {
				t.Fatal(err)
			}
			owner, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: rel, Revision: "main"})
			if err != nil {
				t.Fatal(err)
			}
			good, err := r.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/other", Revision: "main"})
			if err != nil {
				t.Fatal(err)
			}
			ownerFile, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: owner.ID, StorageID: r.nfs.ID, Path: rel, State: proto.MFPresent})
			if err != nil {
				t.Fatal(err)
			}
			if existing {
				if _, err = r.st.UpsertModelFile(&store.ModelFile{ModelID: r.model.ID, StorageID: r.nfs.ID, Path: "old/path", State: proto.MFFailed}); err != nil {
					t.Fatal(err)
				}
			}
			r.h.d.CallNode = func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
				if uuid != r.node.UUID || method != proto.RPCRegisterPath {
					t.Fatalf("unexpected RPC: %s %s", uuid, method)
				}
				p := params.(map[string]any)
				dir := p["path"].(string)
				if !strings.HasPrefix(dir, root+string(filepath.Separator)) || p["storage_id"] != r.nfs.ID || p["inspect_metadata"] != true {
					t.Fatalf("unexpected params: %v", p)
				}
				report, err := modelscan.ParseMetadataWithin(ctx, root, dir)
				if err != nil {
					return nil, err
				}
				size, err := modelscan.ValidateModelDir(dir)
				if err != nil {
					return nil, err
				}
				return json.Marshal(registerPathResponse{Path: dir, SizeBytes: size, Metadata: &report})
			}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"items":[{"rel_path":"%s","model_id":%d},{"rel_path":"org/other","model_id":%d},{"rel_path":"../outside","model_id":%d}]}`, rel, r.model.ID, good.ID, good.ID)))
			route := chi.NewRouteContext()
			route.URLParams.Add("id", fmt.Sprint(r.nfs.ID))
			rec := httptest.NewRecorder()
			r.h.importStorageModels(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route)))
			var response dto.StorageImportResult
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			t.Logf("HTTP %d: %s", rec.Code, rec.Body.String())
			if rec.Code != http.StatusOK || len(response.Failed) != 2 || len(response.Imported) != 1 || response.Imported[0].ModelID != good.ID {
				t.Fatalf("batch response=%s", rec.Body.String())
			}
			var wire struct {
				Failed []struct {
					Error  string `json:"error"`
					Detail struct {
						Code    string         `json:"code"`
						Params  map[string]any `json:"params"`
						Message string         `json:"message"`
					} `json:"error_detail"`
				} `json:"failed"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			fail := wire.Failed[0]
			if fail.Detail.Code != "MODEL_FILE_PATH_OCCUPIED" || fail.Detail.Params["path"] != rel || fail.Detail.Params["model_id"] != float64(owner.ID) || fail.Detail.Params["storage_id"] != float64(r.nfs.ID) || fail.Error == "" || fail.Error != fail.Detail.Message || strings.Contains(fail.Error, "UNIQUE") {
				t.Fatalf("missing product message or legacy fallback: %s", rec.Body.String())
			}
			if got, err := r.st.ModelFileByID(ownerFile.ID); err != nil || !reflect.DeepEqual(got, ownerFile) {
				t.Fatalf("owner changed: %+v err=%v", got, err)
			}
			if got, err := r.st.ModelFileAt(store.FileRef{ModelID: good.ID, StorageID: r.nfs.ID}); err != nil || got.Path != "org/other" || got.State != proto.MFPresent || got.Acquisition != proto.ModelAcquireImported || got.Downloaded != got.SizeBytes || got.SizeBytes <= 0 {
				t.Fatalf("successful item was lost: %+v err=%v", got, err)
			}
			var raw struct {
				Imported []map[string]any `json:"imported"`
				Failed   []map[string]any `json:"failed"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			if raw.Failed[1]["error"] != "invalid relative path" || len(raw.Failed[1]) != 2 {
				t.Fatalf("plain error contract changed: %v", raw.Failed[1])
			}
			if _, ok := raw.Imported[0]["error_detail"]; ok {
				t.Fatal("successful result contains an error")
			}
		})
	}
}

// A local model renamed after import still owns its directory: importing that
// directory again binds it instead of registering a second model that the
// path's unique row would then refuse.
// go test ./internal/server/api -run TestImportStorageModelsBindsTheRenamedHolder -count=1
func TestImportStorageModelsBindsTheRenamedHolder(t *testing.T) {
	r := newSaveToRig(t)
	root := t.TempDir()
	rel := "Qwen3-0.6B"
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"config.json": `{"architectures":["Qwen3ForCausalLM"]}`, "model.safetensors": "weights"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.st.ApplyStorageStatus(r.node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: r.nfs.ID, State: proto.StorageStateReady, Path: root, Writable: true}}}); err != nil {
		t.Fatal(err)
	}
	holder, err := r.st.UpsertModel(&store.Model{Source: "local", SourceID: rel, Revision: "local", Name: "qwen"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: holder.ID, StorageID: r.nfs.ID, Path: rel, State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := r.st.SetModelSourceID(holder.ID, "Qwen/Qwen3-0.6B"); err != nil || !changed {
		t.Fatalf("rename: changed=%t err=%v", changed, err)
	}
	before, err := r.st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	r.h.d.CallNode = func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		p := params.(map[string]any)
		path := p["path"].(string)
		report, err := modelscan.ParseMetadataWithin(ctx, root, path)
		if err != nil {
			return nil, err
		}
		size, err := modelscan.ValidateModelDir(path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(registerPathResponse{Path: path, SizeBytes: size, Metadata: &report})
	}

	// The scan names the holder, not whatever the directory name would bind.
	scan := r.h.annotateScan(r.nfs, r.node, proto.StorageScanResult{Candidates: []proto.StorageScanCandidate{{RelPath: rel, Source: "local", SourceID: rel, Revision: "local"}}})
	if len(scan.Candidates) != 1 || !scan.Candidates[0].Imported || scan.Candidates[0].BindModelID != holder.ID || scan.Candidates[0].BindModelName != "qwen" {
		t.Fatalf("scan candidate = %+v", scan.Candidates)
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"items":[{"rel_path":"%s","source":"local","source_id":"%s"}]}`, rel, rel)))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", fmt.Sprint(r.nfs.ID))
	rec := httptest.NewRecorder()
	r.h.importStorageModels(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route)))
	var response dto.StorageImportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(response.Failed) != 0 || len(response.Imported) != 1 || response.Imported[0].ModelID != holder.ID || response.Imported[0].Created {
		t.Fatalf("import response=%s", rec.Body.String())
	}
	after, err := r.st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("import registered another model: %d -> %d", len(before), len(after))
	}
	if stored, _ := r.st.GetModel(holder.ID); stored.SourceID != "Qwen/Qwen3-0.6B" {
		t.Fatalf("holder changed: %+v", stored)
	}
}
