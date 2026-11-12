package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/hardwarecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listInferenceTemplates answers GET /inference-templates: always one page
// (page and page_size as on every paged list, page 1 of 50 by default) of the
// templates the hardware and text filters match.
func (a *apiHandlers) listInferenceTemplates(w http.ResponseWriter, r *http.Request) {
	query, err := parseListQuery(r, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	models, err := a.d.Store.ListInferenceModels()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	filtered, err := filterInferenceModels(models, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	etag := inferenceTemplateETag(filtered, r.URL.RawQuery)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, pageOf(filtered, query, func(page []store.InferenceModel) []dto.InferenceTemplate {
		items := make([]dto.InferenceTemplate, 0, len(page))
		for _, model := range page {
			items = append(items, inferenceTemplateView(model))
		}
		return items
	}))
}

func (a *apiHandlers) listInferenceTemplateIndex(w http.ResponseWriter, r *http.Request) {
	models, err := a.d.Store.ListInferenceModels()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	models, err = filterInferenceModels(models, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hardware, projectCompatibility, status, err := a.templateCompatibilityGPUs(r)
	if err != nil {
		writeErr(w, status, err)
		return
	}
	items := make([]dto.InferenceTemplateIndexItem, 0, len(models))
	for _, model := range models {
		compatible := map[string]bool{}
		if projectCompatibility {
			compatible, err = compatibleTemplateProfiles(model, hardware)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
		}
		engines, scenarios := map[string]struct{}{}, map[string]struct{}{}
		profiles := make([]dto.InferenceProfileSummary, 0, len(model.Specs))
		for _, spec := range model.Specs {
			engines[spec.Engine] = struct{}{}
			if spec.Scenario != "" {
				scenarios[spec.Scenario] = struct{}{}
			}
			var gpus dto.GPUPredicate
			_ = json.Unmarshal([]byte(spec.GPUs), &gpus)
			sources, _ := inferencecatalog.DecodeWeightSources(spec.WeightSources)
			repoIDs := make([]string, 0, len(sources))
			seenRepoIDs := make(map[string]struct{}, len(sources))
			for _, source := range sources {
				repoID := strings.TrimSpace(source.RepoID)
				key := strings.ToLower(repoID)
				if repoID == "" || key == "" {
					continue
				}
				if _, exists := seenRepoIDs[key]; exists {
					continue
				}
				seenRepoIDs[key] = struct{}{}
				repoIDs = append(repoIDs, repoID)
			}
			var detectedCompatible *bool
			if projectCompatibility {
				value := compatible[spec.Source+"\x00"+spec.Name]
				detectedCompatible = &value
			}
			profiles = append(profiles, dto.InferenceProfileSummary{
				DetectedCompatible: detectedCompatible,
				Source:             spec.Source, Name: spec.Name, DisplayNameZHCN: spec.DisplayNameZHCN, DisplayNameEN: spec.DisplayNameEN, Scenario: spec.Scenario,
				Engine: spec.Engine, Image: spec.Image, Quantization: spec.Quantization, WeightBytes: spec.WeightBytes,
				WeightRepoIDs: repoIDs, GPUs: gpus,
			})
		}
		items = append(items, dto.InferenceTemplateIndexItem{ID: model.ID, ModelID: model.ModelID, DisplayTitle: model.DisplayTitle, Source: model.Source, Engines: sortedKeys(engines), Scenarios: sortedKeys(scenarios), Profiles: profiles})
	}
	answer := dto.InferenceTemplateIndex{Items: items}
	if projectCompatibility {
		answer.Hardware = []dto.DetectedTemplateChip{}
		for _, gpu := range hardware {
			cc := ""
			if gpu.CC > 0 {
				cc = strconv.FormatFloat(gpu.CC, 'f', -1, 64)
			}
			answer.Hardware = append(answer.Hardware, dto.DetectedTemplateChip{Vendor: gpu.Vendor, Name: gpu.Name, CC: cc, ArchID: gpu.ArchID})
		}
	}
	writeJSON(w, http.StatusOK, answer)
}

func (a *apiHandlers) listInferenceTemplateOrganizations(w http.ResponseWriter, r *http.Request) {
	models, err := a.d.Store.ListInferenceModels()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	models, err = filterInferenceModels(models, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	counts := map[string]int64{}
	for _, model := range models {
		organization := model.ModelID
		if slash := strings.IndexByte(organization, '/'); slash >= 0 {
			organization = organization[:slash]
		}
		counts[organization]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	items := make([]dto.InferenceTemplateOrganization, 0, len(keys))
	for _, key := range keys {
		items = append(items, dto.InferenceTemplateOrganization{Organization: key, Models: counts[key]})
	}
	writeJSON(w, http.StatusOK, dto.InferenceTemplateOrganizations{Items: items})
}

func (a *apiHandlers) getInferenceTemplate(w http.ResponseWriter, r *http.Request) {
	modelID := strings.Trim(strings.TrimSpace(chi.URLParam(r, "*")), "/")
	model, err := a.d.Store.InferenceModelByModelID(modelID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	view := inferenceTemplateView(*model)
	detected, err := a.d.Store.ListDistinctDetectedGPUs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	gpus := make([]chipmatch.GPU, 0, len(detected))
	for _, gpu := range detected {
		gpus = append(gpus, chipmatch.ReportedGPU(gpu.Name, gpu.Vendor, gpu.ArchID, gpu.CC))
	}
	support, err := detectedTemplateSupport(*model, gpus)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	view.DetectedSupport = &support
	view.ReadmeZHCNContent = a.readInferenceTemplateDocumentation(*model, model.ReadmeZHCN)
	view.ReadmeENContent = a.readInferenceTemplateDocumentation(*model, model.ReadmeEN)
	writeJSON(w, http.StatusOK, view)
}

func (a *apiHandlers) readInferenceTemplateDocumentation(model store.InferenceModel, file string) string {
	if strings.TrimSpace(file) == "" {
		return ""
	}
	var data []byte
	var err error
	if a.d.CatalogPackages != nil && model.PackageDigest != "" {
		data, err = a.d.CatalogPackages.ReadDocumentation(model.PackageDigest, model.ModelID, file)
	}
	if err != nil || len(data) > 512*1024 {
		return ""
	}
	return string(data)
}

func (a *apiHandlers) cloneInferenceTemplate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SourceModelID string `json:"source_model_id"`
		ModelID       string `json:"model_id"`
		DisplayTitle  string `json:"display_title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	source, err := a.d.Store.InferenceModelByModelID(input.SourceModelID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	modelID := strings.ToLower(strings.TrimSpace(input.ModelID))
	if modelID == "" {
		modelID = source.ModelID + "-copy"
		for suffix := 2; ; suffix++ {
			if _, err := a.d.Store.InferenceModelByModelID(modelID); err == store.ErrNotFound {
				break
			}
			modelID = fmt.Sprintf("%s-copy-%d", source.ModelID, suffix)
		}
	}
	if err := inferencecatalog.ValidateModelID(modelID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	clone := *source
	clone.ID, clone.ModelID, clone.Source = 0, modelID, "user"
	clone.PackageDigest = ""
	clone.Specs = append([]store.InferenceSpecRow(nil), source.Specs...)
	if title := strings.TrimSpace(input.DisplayTitle); title != "" {
		clone.DisplayTitle = title
	} else {
		clone.DisplayTitle = strings.TrimSpace(source.DisplayTitle + " Copy")
	}
	for index := range clone.Specs {
		clone.Specs[index].ID = 0
		clone.Specs[index].ModelID = 0
	}
	packageDigest, packageCreated, err := a.prepareInferenceTemplateClone(*source, &clone)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.d.Store.ImportUserInferenceCatalog([]store.InferenceModel{clone}, false); err != nil {
		a.removeCatalogPackageOnFailure(packageDigest, packageCreated)
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrInferenceCatalogConflict) {
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	created, err := a.d.Store.InferenceModelByModelID(modelID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.pruneUnreferencedCatalogPackages()
	writeJSON(w, http.StatusCreated, inferenceTemplateView(*created))
}

func (a *apiHandlers) summarizeInferenceTemplates(w http.ResponseWriter, r *http.Request) {
	models, err := a.d.Store.ListInferenceModels()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	models, err = filterInferenceModels(models, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var specs int64
	for _, model := range models {
		specs += int64(len(model.Specs))
	}
	writeJSON(w, http.StatusOK, dto.InferenceTemplateSummary{Deployable: specs, Models: int64(len(models))})
}

func (a *apiHandlers) listTemplateChips(w http.ResponseWriter, _ *http.Request) {
	rows := hardwarecatalog.List()
	items := make([]dto.TemplateChip, 0, len(rows))
	for _, row := range rows {
		items = append(items, dto.TemplateChip{Vendor: row.Vendor, Name: row.Name, CC: row.CC, Category: row.Category, Architecture: row.Architecture, ArchID: row.ArchID})
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

func (a *apiHandlers) listDetectedTemplateChips(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.d.Store.ListDistinctDetectedGPUs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.DetectedTemplateChip, 0, len(rows))
	for _, row := range rows {
		items = append(items, dto.DetectedTemplateChip{Vendor: row.Vendor, Name: row.Name, CC: row.CC, ArchID: row.ArchID})
	}
	writeJSON(w, http.StatusOK, dto.NewList(items))
}

func (a *apiHandlers) importInferenceTemplates(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	overwrite, _ := strconv.ParseBool(r.URL.Query().Get("overwrite"))
	catalog, digest, created, err := a.decodeInferenceCatalog(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if digest == "" {
		a.backfillInferenceCatalogWeightBytes(r, &catalog)
		if catalogRequiresLocalFiles(catalog) {
			if !overwrite {
				writeProductError(w, http.StatusBadRequest, ErrorCatalogPackageRequiredForFiles, "catalog assets and documentation require a .gpuworks-catalog.tar.gz package, or overwrite of an existing packaged model", nil)
				return
			}
			digest, created, err = a.materializeEditedInferenceCatalogPackage(catalog)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			storedPackage, loadErr := a.d.CatalogPackages.Load(digest)
			if loadErr != nil {
				a.removeCatalogPackageOnFailure(digest, created)
				writeErr(w, http.StatusBadRequest, loadErr)
				return
			}
			// The package also carries the component models it references;
			// the import writes back only the models this YAML edits.
			catalog = modelsNamedIn(storedPackage.Catalog, catalog)
		}
	}
	models, err := a.importedStoreModels(catalog)
	if err != nil {
		a.removeCatalogPackageOnFailure(digest, created)
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	for index := range models {
		models[index].PackageDigest = digest
		for specIndex := range models[index].Specs {
			models[index].Specs[specIndex].PackageDigest = digest
		}
	}
	if err := a.d.Store.ImportUserInferenceCatalog(models, overwrite); err != nil {
		a.removeCatalogPackageOnFailure(digest, created)
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrInferenceCatalogConflict) {
			status = http.StatusConflict
		}
		writeErr(w, status, err)
		return
	}
	a.pruneUnreferencedCatalogPackages()
	writeJSON(w, http.StatusOK, dto.InferenceTemplateImportResult{
		Models: len(models), PackageDigest: digest,
		Warnings: catalogWarningIssues(catalog),
	})
}

func (a *apiHandlers) exportInferenceTemplatePackage(w http.ResponseWriter, r *http.Request) {
	digest := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "digest")))
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		writeProductError(w, http.StatusBadRequest, ErrorCatalogPackageDigestInvalid, "invalid catalog package digest", nil)
		return
	}

	var archive bytes.Buffer
	if a.d.CatalogPackages == nil {
		err = errors.New("catalog package storage is unavailable")
	} else {
		err = a.d.CatalogPackages.Pack(digest, &archive)
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gpuworks-catalog-%s.gpuworks-catalog.tar.gz"`, digest[:12]))
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(archive.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive.Bytes())
}

func (a *apiHandlers) decodeInferenceCatalog(raw []byte) (inferencecatalog.FlatCatalog, string, bool, error) {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		if a.d.CatalogPackages == nil {
			return inferencecatalog.FlatCatalog{}, "", false, errors.New("catalog package storage is unavailable")
		}
		archive, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(raw))
		if err != nil {
			return inferencecatalog.FlatCatalog{}, "", false, err
		}
		digest, created, err := a.d.CatalogPackages.Put(archive.FS)
		if err != nil {
			return inferencecatalog.FlatCatalog{}, "", false, err
		}
		return archive.Package.Catalog, digest, created, nil
	}
	catalog, err := inferencecatalog.DecodeYAML(raw)
	if err != nil {
		return inferencecatalog.FlatCatalog{}, "", false, err
	}
	if err := a.validateWithComponentModels(&catalog); err != nil {
		return inferencecatalog.FlatCatalog{}, "", false, err
	}
	return catalog, "", false, nil
}

func (a *apiHandlers) removeCatalogPackageOnFailure(digest string, created bool) {
	if created && digest != "" && a.d.CatalogPackages != nil {
		// A concurrent request may have committed the same immutable digest after
		// this request created it but before this request's DB transaction failed.
		// Prefer a harmless orphan over deleting a package another deployment now
		// references; the normal GC will remove it once it is truly unreachable.
		referenced, err := a.d.Store.ReferencedCatalogPackageDigests()
		if err != nil {
			return
		}
		for _, reference := range referenced {
			if reference == digest {
				return
			}
		}
		_ = a.d.CatalogPackages.Remove(digest)
	}
}

// User YAML deliberately permits an omitted weight_bytes. The production
// metadata fetcher resolves the first reachable source, with the configured
// source preferred, and leaves the value unknown when every source fails.
func (a *apiHandlers) backfillInferenceCatalogWeightBytes(r *http.Request, catalog *inferencecatalog.FlatCatalog) {
	if catalog == nil || a.d.FetchModelMeta == nil {
		return
	}
	for modelIndex := range catalog.Models {
		for specIndex := range catalog.Models[modelIndex].Specs {
			spec := &catalog.Models[modelIndex].Specs[specIndex]
			if spec.WeightBytes > 0 {
				continue
			}
			sources := append([]inferencecatalog.WeightSource(nil), spec.Weights.Sources...)
			for _, source := range sources {
				meta, err := a.deployments().FetchModelMeta(r.Context(), source.Provider, source.RepoID, source.Revision, source.Subfolder, "")
				if err == nil && meta != nil && meta.WeightBytes > 0 {
					spec.WeightBytes = meta.WeightBytes
					break
				}
			}
		}
	}
}

func filterInferenceModels(models []store.InferenceModel, r *http.Request) ([]store.InferenceModel, error) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	engine := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("engine")))
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	gpuName := strings.TrimSpace(r.URL.Query().Get("gpu"))
	vendor := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("vendor")))
	ccRaw := strings.TrimSpace(r.URL.Query().Get("cc"))
	archID := strings.TrimSpace(r.URL.Query().Get("arch_id"))
	cc := 0.0
	if archID != "" && gpuName == "" {
		return nil, fmt.Errorf("arch_id requires gpu")
	}
	if ccRaw != "" {
		var err error
		cc, err = strconv.ParseFloat(ccRaw, 64)
		if err != nil || math.IsNaN(cc) || math.IsInf(cc, 0) || cc <= 0 || gpuName == "" {
			return nil, fmt.Errorf("cc must be positive and requires gpu")
		}
	}
	var result []store.InferenceModel
	for _, model := range models {
		if query != "" && !strings.Contains(strings.ToLower(model.ModelID+" "+model.DisplayTitle), query) {
			continue
		}
		copy := model
		copy.Specs = nil
		for _, spec := range model.Specs {
			if source != "" && spec.Source != source {
				continue
			}
			if engine != "" && spec.Engine != engine {
				continue
			}

			copy.Specs = append(copy.Specs, spec)
		}
		if gpuName != "" {
			var err error
			copy.Specs, err = matchingTemplateSpecs(copy, chipmatch.GPU{Name: gpuName, Vendor: vendor, CC: cc, ArchID: archID})
			if err != nil {
				return nil, err
			}
		}
		if len(copy.Specs) > 0 {
			result = append(result, copy)
		}
	}
	return result, nil
}

func inferenceTemplateView(model store.InferenceModel) dto.InferenceTemplate {
	out := dto.InferenceTemplate{ID: model.ID, ModelID: model.ModelID, DisplayTitle: model.DisplayTitle, ParameterCount: model.ParameterCount, Architecture: model.Architecture, ReadmeZHCN: model.ReadmeZHCN, ReadmeEN: model.ReadmeEN, ContextLength: model.ContextLength, ReleaseDate: model.ReleaseDate, Source: model.Source, Specs: []dto.InferenceSpecDTO{}}
	_ = json.Unmarshal([]byte(model.InputModalities), &out.InputModalities)
	_ = json.Unmarshal([]byte(model.Unsupported), &out.Unsupported)
	for _, spec := range model.Specs {
		view := dto.InferenceSpecDTO{ID: spec.ID, Source: spec.Source, Name: spec.Name, DisplayNameZHCN: spec.DisplayNameZHCN, DisplayNameEN: spec.DisplayNameEN, Scenario: spec.Scenario, Engine: spec.Engine, Image: spec.Image, Quantization: spec.Quantization, WeightBytes: spec.WeightBytes, RuntimeWeightBytes: spec.RuntimeWeightBytes, ShmBytes: spec.ShmBytes, MemlockUnlimited: spec.MemlockUnlimited, PreStartScript: spec.PreStartScript, PDMode: spec.PDMode}
		if sources, _ := inferencecatalog.DecodeWeightSources(spec.WeightSources); sources != nil {
			view.WeightSources = make([]dto.WeightSource, 0, len(sources))
			for _, source := range sources {
				view.WeightSources = append(view.WeightSources, dto.WeightSource(source))
			}
		}
		view.ImageSources, _ = inferencecatalog.DecodeImageSources(spec.ImageSources)
		_ = json.Unmarshal([]byte(spec.GPUs), &view.GPUs)
		_ = json.Unmarshal([]byte(spec.Args), &view.Args)
		_ = json.Unmarshal([]byte(spec.Env), &view.Env)
		_ = json.Unmarshal([]byte(spec.PreStartArgv), &view.PreStartArgv)
		_ = json.Unmarshal([]byte(spec.Assets), &view.Assets)
		_ = json.Unmarshal([]byte(spec.Entrypoint), &view.Entrypoint)
		// The image tag gives the release here; evaluate, which also reads the
		// image catalog, answers for the form once it runs.
		view.EngineDecidesParamKeys, _ = deploy.EngineDecidesParameterKeys(spec.Engine, deploy.LaunchShape{
			Entrypoint: view.Entrypoint, Args: view.Args, EngineVersion: deploy.ImageEngineVersion(spec.Engine, spec.Image, ""),
		})
		_ = json.Unmarshal([]byte(spec.Placement), &view.Placement)
		if raw, err := deployment.EffectiveTemplateAPIContract(model, spec); err == nil {
			_ = json.Unmarshal(raw, &view.API)
			enrichTemplateAPIContract(view.API, raw)
		}
		_ = json.Unmarshal([]byte(spec.Components), &view.Components)
		out.Specs = append(out.Specs, view)
	}
	return out
}

// enrichTemplateAPIContract fills in each operation's protocol facts and its
// usage example. The example is generated from the spec's whole declaration
// (raw): request defaults and a declared example input included, which the
// view itself does not carry.
func enrichTemplateAPIContract(contract *dto.APIContract, raw json.RawMessage) {
	if contract == nil {
		return
	}
	var declared inferenceapi.Contract
	_ = json.Unmarshal(raw, &declared)
	for index := range contract.Operations {
		operation := &contract.Operations[index]
		definition, ok := inferenceapi.Lookup(operation.Type)
		if !ok {
			continue
		}
		operation.RequestEncodings = definition.RequestEncodings
		operation.Renderer = definition.Renderer
		operation.Response = definition.Response
		if operation.Mode == "async" {
			operation.Async = definition.Async
		}
		source := inferenceapi.Operation{Type: operation.Type, Endpoint: operation.Endpoint, Mode: operation.Mode}
		for _, candidate := range declared.Operations {
			if candidate.Type == operation.Type {
				source = candidate
				break
			}
		}
		if example, err := inferenceapi.BuildExample(source, "<served-model>"); err == nil {
			operation.Example = &example
		}
	}
}
