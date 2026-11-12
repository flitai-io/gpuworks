package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImportInferenceTemplatePackagePersistsContentAddressedAssets(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	packages, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	asset := []byte("print('ready')\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(asset))
	model := inferencecatalog.InferenceModel{
		ModelID: "acme/asset-model", Source: "user", ReadmeZHCN: "README.zh-CN.md",
		Specs: []inferencecatalog.InferenceSpec{{
			Name: "asset", Engine: "vllm", Image: "vllm:test",
			Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "acme/asset-model"}}},
			GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia"},
			Assets:  []inferencecatalog.Asset{{File: "assets/serve.py", SHA256: digest, UnpackTo: "/opt/gpuworks/serve.py"}},
		}},
	}
	modelYAML, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	packageFS := fstest.MapFS{
		"manifest.yaml":                           {Data: []byte("schema_version: 1\npublished_at: 2026-08-31T00:00:00Z\nmodels: 1\nspecs: 1\n")},
		"models/acme/asset-model/model.yaml":      {Data: modelYAML},
		"models/acme/asset-model/README.zh-CN.md": {Data: []byte("# 本地使用说明\n")},
		"models/acme/asset-model/assets/serve.py": {Data: asset},
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: database, CatalogPackages: packages}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", bytes.NewReader(archive.Bytes()))
	recorder := httptest.NewRecorder()
	handler.importInferenceTemplates(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	stored, err := database.InferenceModelByModelID(model.ModelID)
	if err != nil || stored.PackageDigest == "" || len(stored.Specs) != 1 || stored.Specs[0].Source != "user" || stored.Specs[0].PackageDigest != stored.PackageDigest {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	got, err := packages.ReadAsset(stored.PackageDigest, model.ModelID, "assets/serve.py")
	if err != nil || !bytes.Equal(got, asset) {
		t.Fatalf("asset=%q err=%v", got, err)
	}
	readme, err := packages.ReadDocumentation(stored.PackageDigest, model.ModelID, "README.zh-CN.md")
	if err != nil || string(readme) != "# 本地使用说明\n" {
		t.Fatalf("readme=%q err=%v", readme, err)
	}
	detailRouter := chi.NewRouter()
	detailRouter.Get("/api/v1/inference-templates/*", handler.getInferenceTemplate)
	detailRecorder := httptest.NewRecorder()
	detailRouter.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/acme/asset-model", nil))
	var detail dto.InferenceTemplate
	if detailRecorder.Code != http.StatusOK || json.Unmarshal(detailRecorder.Body.Bytes(), &detail) != nil || detail.ReadmeZHCNContent != "# 本地使用说明\n" {
		t.Fatalf("detail status=%d body=%s parsed=%+v", detailRecorder.Code, detailRecorder.Body.String(), detail)
	}
	if len(detail.Specs) != 1 || len(detail.Specs[0].Assets) != 1 || detail.Specs[0].Assets[0].SizeBytes != int64(len(asset)) {
		t.Fatalf("detail asset facts=%+v", detail.Specs)
	}

	exportRouter := chi.NewRouter()
	exportRouter.Get("/api/v1/inference-templates/export/{digest}", handler.exportInferenceTemplatePackage)
	exportRequest := httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/export/"+stored.PackageDigest, nil)
	exportRecorder := httptest.NewRecorder()
	exportRouter.ServeHTTP(exportRecorder, exportRequest)
	if exportRecorder.Code != http.StatusOK {
		t.Fatalf("export status/body=%d %s", exportRecorder.Code, exportRecorder.Body.String())
	}
	if got := exportRecorder.Header().Get("Content-Type"); got != "application/gzip" {
		t.Fatalf("export content type=%q", got)
	}
	if !bytes.Equal(exportRecorder.Body.Bytes(), archive.Bytes()) {
		t.Fatal("imported package did not export to the same deterministic archive")
	}
	exported, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(exportRecorder.Body.Bytes()))
	if err != nil || len(exported.Package.Catalog.Models) != 1 || exported.Package.Catalog.Models[0].ModelID != model.ModelID {
		t.Fatalf("exported package=%+v err=%v", exported.Package.Manifest, err)
	}

	cloneRecorder := httptest.NewRecorder()
	handler.cloneInferenceTemplate(cloneRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/clone", bytes.NewBufferString(`{"source_model_id":"acme/asset-model","model_id":"me/asset-model-copy"}`)))
	if cloneRecorder.Code != http.StatusCreated {
		t.Fatalf("clone status/body=%d %s", cloneRecorder.Code, cloneRecorder.Body.String())
	}
	cloned, err := database.InferenceModelByModelID("me/asset-model-copy")
	if err != nil || cloned.PackageDigest == "" || cloned.PackageDigest == stored.PackageDigest || cloned.Source != "user" || len(cloned.Specs) != 1 || cloned.Specs[0].Source != "user" || cloned.Specs[0].PackageDigest != cloned.PackageDigest {
		t.Fatalf("cloned model=%+v err=%v", cloned, err)
	}
	clonedAsset, err := packages.ReadAsset(cloned.PackageDigest, cloned.ModelID, "assets/serve.py")
	if err != nil || !bytes.Equal(clonedAsset, asset) {
		t.Fatalf("cloned asset=%q err=%v", clonedAsset, err)
	}
	clonedReadme, err := packages.ReadDocumentation(cloned.PackageDigest, cloned.ModelID, "README.zh-CN.md")
	if err != nil || string(clonedReadme) != "# 本地使用说明\n" {
		t.Fatalf("cloned README=%q err=%v", clonedReadme, err)
	}
	var clonedArchive bytes.Buffer
	if err := packages.Pack(cloned.PackageDigest, &clonedArchive); err != nil {
		t.Fatal(err)
	}
	clonedPackage, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(clonedArchive.Bytes()))
	if err != nil || len(clonedPackage.Package.Catalog.Models) != 1 || clonedPackage.Package.Catalog.Models[0].ModelID != cloned.ModelID {
		t.Fatalf("cloned package=%+v err=%v", clonedPackage.Package.Manifest, err)
	}
	snapshot, _ := json.Marshal(map[string]any{"runtime_assets": []map[string]any{{
		"file": "assets/serve.py", "package_digest": cloned.PackageDigest, "model_id": cloned.ModelID,
	}}})
	deployment, err := database.CreateDeployment(&store.Deployment{Name: "asset-package-reference", CatalogSelectionSnapshot: string(snapshot)})
	if err != nil {
		t.Fatal(err)
	}

	editable, err := inferencecatalog.FromFlatStoreModel(*cloned)
	if err != nil {
		t.Fatal(err)
	}
	editable.DisplayTitle = "Edited asset model"
	editable.Specs[0].Args = []string{"--edited", "true"}
	editedYAML, err := inferencecatalog.MarshalYAML(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{editable}})
	if err != nil {
		t.Fatal(err)
	}
	editRecorder := httptest.NewRecorder()
	handler.importInferenceTemplates(editRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import?overwrite=true", bytes.NewReader(editedYAML)))
	if editRecorder.Code != http.StatusOK {
		t.Fatalf("edit status/body=%d %s", editRecorder.Code, editRecorder.Body.String())
	}
	edited, err := database.InferenceModelByModelID(cloned.ModelID)
	if err != nil || edited.DisplayTitle != "Edited asset model" || edited.PackageDigest == "" || edited.PackageDigest == cloned.PackageDigest || len(edited.Specs) != 1 || edited.Specs[0].PackageDigest != edited.PackageDigest {
		t.Fatalf("edited model=%+v err=%v", edited, err)
	}
	editedAsset, err := packages.ReadAsset(edited.PackageDigest, edited.ModelID, "assets/serve.py")
	if err != nil || !bytes.Equal(editedAsset, asset) {
		t.Fatalf("edited asset=%q err=%v", editedAsset, err)
	}
	editedReadme, err := packages.ReadDocumentation(edited.PackageDigest, edited.ModelID, "README.zh-CN.md")
	if err != nil || string(editedReadme) != "# 本地使用说明\n" {
		t.Fatalf("edited README=%q err=%v", editedReadme, err)
	}
	// Editing moves the catalog row to a new immutable package, but the older
	// package remains executable while a deployment snapshot references it.
	var preservedArchive bytes.Buffer
	if err := packages.Pack(cloned.PackageDigest, &preservedArchive); err != nil {
		t.Fatalf("deployment-referenced package was pruned: %v", err)
	}
	if err := database.DeleteDeployment(deployment.ID); err != nil {
		t.Fatal(err)
	}
	handler.pruneUnreferencedCatalogPackages()
	preservedArchive.Reset()
	if err := packages.Pack(cloned.PackageDigest, &preservedArchive); err == nil {
		t.Fatal("orphaned historical package survived garbage collection")
	}
	if err := packages.Pack(edited.PackageDigest, &preservedArchive); err != nil {
		t.Fatalf("current catalog package was pruned: %v", err)
	}
}

func TestSameNamedBuiltInAndUserSpecsReadAssetsFromTheirOwnPackages(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog-source-assets.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	packages, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	const modelID = "acme/shared-assets"
	modelPackage := func(source, image string, asset []byte) (inferencecatalog.InferenceModel, fstest.MapFS) {
		assetDigest := fmt.Sprintf("%x", sha256.Sum256(asset))
		model := inferencecatalog.InferenceModel{ModelID: modelID, Source: source, DisplayTitle: "Canonical title", Specs: []inferencecatalog.InferenceSpec{{
			Name: "general", Engine: "vllm", Image: image,
			Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID}}},
			GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia"},
			Assets:  []inferencecatalog.Asset{{File: "assets/launch.py", SHA256: assetDigest, UnpackTo: "/opt/gpuworks/launch.py"}},
		}}}
		modelYAML, marshalErr := inferencecatalog.MarshalModelYAML(model)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return model, fstest.MapFS{
			"manifest.yaml":                              {Data: []byte("schema_version: 1\npublished_at: 2026-09-01T00:00:00Z\nmodels: 1\nspecs: 1\n")},
			"models/acme/shared-assets/model.yaml":       {Data: modelYAML},
			"models/acme/shared-assets/assets/launch.py": {Data: asset},
		}
	}

	upstreamAsset := []byte("print('built-in')\n")
	upstreamModel, upstreamFS := modelPackage("upstream", "upstream/vllm:v1", upstreamAsset)
	upstreamDigest, _, err := packages.Put(upstreamFS)
	if err != nil {
		t.Fatal(err)
	}
	upstreamRows, err := inferencecatalog.ToFlatStoreModels(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{upstreamModel}})
	if err != nil {
		t.Fatal(err)
	}
	upstreamRows[0].PackageDigest = upstreamDigest
	upstreamRows[0].Specs[0].PackageDigest = upstreamDigest
	if err := database.ReconcileInferenceCatalog(upstreamRows); err != nil {
		t.Fatal(err)
	}

	userAsset := []byte("print('user')\n")
	_, userFS := modelPackage("user", "user/vllm:v1", userAsset)
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, userFS); err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: database, CatalogPackages: packages}}
	recorder := httptest.NewRecorder()
	handler.importInferenceTemplates(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", bytes.NewReader(archive.Bytes())))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	var imported struct {
		PackageDigest string `json:"package_digest"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &imported); err != nil || imported.PackageDigest == "" || imported.PackageDigest == upstreamDigest {
		t.Fatalf("imported=%+v err=%v", imported, err)
	}
	stored, err := database.InferenceModelByModelID(modelID)
	if err != nil || stored.Source != "upstream" || stored.PackageDigest != upstreamDigest || len(stored.Specs) != 2 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	bySource := map[string]store.InferenceSpecRow{}
	for _, spec := range stored.Specs {
		bySource[spec.Source] = spec
	}
	if bySource["upstream"].PackageDigest != upstreamDigest || bySource["user"].PackageDigest != imported.PackageDigest {
		t.Fatalf("spec package ownership=%+v", bySource)
	}
	for source, want := range map[string][]byte{"upstream": upstreamAsset, "user": userAsset} {
		got, err := handler.readInferenceTemplateSpecAsset(*stored, bySource[source], "assets/launch.py")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s asset=%q want=%q err=%v", source, got, want, err)
		}
	}
	// A deployment naming the user spec runs the user spec, whose assets are
	// the ones its own package holds (read above).
	evaluated, err := handler.deployments().Evaluate(context.Background(), dto.EvaluateRequest{DeployRequest: dto.DeployRequest{TemplateID: stored.ID, TemplateSpecSource: "user", TemplateSpecName: "general"}})
	if err != nil {
		t.Fatal(err)
	}
	if evaluated.TemplateSpecSource != "user" || evaluated.TemplateSpecName != "general" || evaluated.Image != "user/vllm:v1" {
		t.Fatalf("evaluated spec=%s/%s image=%q", evaluated.TemplateSpecSource, evaluated.TemplateSpecName, evaluated.Image)
	}
}

func TestFailedCatalogImportCleanupNeverDeletesACommittedConcurrentReference(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog-cleanup.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	packages, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	model := inferencecatalog.InferenceModel{ModelID: "gpuworks/cleanup", Source: "user", Specs: []inferencecatalog.InferenceSpec{{
		Name: "default", Engine: "vllm", Image: "example/vllm:test",
		Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "gpuworks/cleanup"}}},
		GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia"},
	}}}
	packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, created, err := packages.Put(packageFS)
	if err != nil || !created {
		t.Fatalf("first put digest=%s created=%v err=%v", digest, created, err)
	}
	handler := &apiHandlers{d: Deps{Store: database, CatalogPackages: packages}}
	handler.removeCatalogPackageOnFailure(digest, true)
	if err := packages.Pack(digest, io.Discard); err == nil {
		t.Fatal("unreferenced failed-import package survived cleanup")
	}
	digest, created, err = packages.Put(packageFS)
	if err != nil || !created {
		t.Fatalf("second put digest=%s created=%v err=%v", digest, created, err)
	}
	rows, err := inferencecatalog.ToFlatStoreModels(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}})
	if err != nil {
		t.Fatal(err)
	}
	rows[0].PackageDigest = digest
	if err := database.ImportUserInferenceCatalog(rows, false); err != nil {
		t.Fatal(err)
	}
	handler.removeCatalogPackageOnFailure(digest, true)
	if err := packages.Pack(digest, io.Discard); err != nil {
		t.Fatalf("committed concurrent package reference was deleted: %v", err)
	}
}
