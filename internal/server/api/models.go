package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listModels answers GET /models. With page or page_size it returns one
// dto.ModelPage (see modelPage); without them, every model the filters match,
// in full with its per-location presence, as the deploy page and scripts read
// it. The filters and order apply either way: a filter never goes unheard.
func (a *apiHandlers) listModels(w http.ResponseWriter, r *http.Request) {
	query, filter, order, err := parseModelListQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	models, err := a.d.Store.ListModels()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !query.paged {
		writeJSON(w, http.StatusOK, dto.NewList(modelsMatching(a.modelDTOs(models), query, filter, order)))
		return
	}
	writeJSON(w, http.StatusOK, modelPage(a.modelDTOs(models), query, filter, order))
}

func (a *apiHandlers) createModel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	var req struct {
		Source          string `json:"source"`
		SourceID        string `json:"source_id"`
		Revision        string `json:"revision"`
		Name            string `json:"name"`
		Arch            string `json:"arch"`
		README          string `json:"readme"`
		ConfigRaw       string `json:"config_raw"`
		WeightBytes     int64  `json:"weight_bytes"`
		LogoData        string `json:"logo_data"`
		BrowserMetadata bool   `json:"browser_metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	if req.Source == "" {
		req.Source = "huggingface"
	}
	req.Source = strings.ToLower(strings.TrimSpace(req.Source))
	req.SourceID = strings.TrimSpace(req.SourceID)
	req.Revision = strings.TrimSpace(req.Revision)
	// ModelScope uses master as its default branch; accepting Hugging Face's
	// main silently produces an empty file listing and a zero-byte download.
	if req.Source == "modelscope" && strings.EqualFold(req.Revision, "main") {
		req.Revision = "master"
	}
	if req.SourceID == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceIDRequired, "source_id is required", nil)
		return
	}
	// A repository id names nothing on a plain file server: an HTTP directory
	// model is only well formed with the directory it is served from, and
	// /models/register-url is the one place that directory is read and stored.
	if req.Source == modelmeta.SourceURL {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceURLRequired,
			"register a model served from an HTTP directory through the directory URL", map[string]any{"source": req.Source})
		return
	}
	if req.Name == "" {
		req.Name = req.SourceID
	}
	if !validateName(w, req.Name) {
		return
	}
	entity := &store.Model{
		Source: req.Source, SourceID: req.SourceID, Revision: req.Revision,
		Name: req.Name, Arch: req.Arch,
	}
	// A browser can reach Hugging Face through the operator's local proxy even
	// when the control-plane server cannot. Keep that metadata and avoid
	// repeating the remote fetch, which otherwise blocks registration until the
	// server-side network timeout expires.
	var notice *dto.ProductMessage
	if req.BrowserMetadata && strings.EqualFold(req.Source, "huggingface") {
		a.applyClientModelMetadata(r.Context(), entity, req.ConfigRaw, req.WeightBytes, req.README, req.LogoData)
	} else {
		// Best-effort metadata fetch (config.json + weight size) so the estimator
		// works before download. Network failures leave fields unknown, and the
		// answer says why.
		if meta, err := a.deployments().FetchModelMeta(r.Context(), req.Source, req.SourceID, req.Revision, "", ""); err == nil {
			notice = a.metadataNotice(meta, req.Source, "")
			a.applyFetchedMetadata(r.Context(), entity, meta)
			// A registration without a chosen ref would otherwise follow a moving
			// default branch: every node could download different bytes under the
			// same identity. Pin the commit the branch points at now when the
			// upstream told us; an unreachable repository stays unpinned and the
			// registration still succeeds.
			if req.Revision == "" && meta != nil && strings.TrimSpace(meta.ResolvedRevision) != "" {
				entity.Revision = strings.TrimSpace(meta.ResolvedRevision)
			}
		}
	}
	initRegisteredModelContract(entity)
	m, err := a.d.Store.UpsertModel(entity)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := a.modelDTO(m, true)
	out.MetadataNotice = notice
	writeJSON(w, http.StatusCreated, out)
}

func (a *apiHandlers) deleteModel(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if id == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorModelIDInvalid, "invalid model id", nil)
		return
	}
	if err := a.d.Store.DeleteModel(id); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, store.ErrCacheDeletionPending):
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, store.ErrReferenced):
			writeProductError(w, http.StatusConflict, ErrorModelInUse, "The model is referenced by a deployment. Delete the related deployments first.", nil)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateModelInferenceConfig stores the model's editable operation contract
// and input modalities. Empty operations are accepted, but they mean "none
// declared": the next template deployment of the model fills them again.
func (a *apiHandlers) updateModelInferenceConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Operations      []inferenceapi.Operation `json:"operations"`
		InputModalities []string                 `json:"input_modalities"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorModelInferenceConfigInvalid, "invalid model inference config JSON", map[string]any{"detail": err.Error()})
		return
	}
	operations, modalities, err := inferenceapi.NormalizeModelConfig(req.Operations, req.InputModalities)
	if err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorModelInferenceConfigInvalid, err.Error(), map[string]any{"detail": err.Error()})
		return
	}
	if operations == nil {
		operations = []inferenceapi.Operation{}
	}
	operationsJSON, err := json.Marshal(operations)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	modalitiesJSON, err := json.Marshal(modalities)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	model, err := a.d.Store.SetModelInferenceConfig(parseUint(chi.URLParam(r, "id")), string(operationsJSON), string(modalitiesJSON))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, a.modelDTO(model, true))
}

func (a *apiHandlers) getModel(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	m, err := a.d.Store.GetModel(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, a.modelDTO(m, true))
}

func (a *apiHandlers) syncModelToNode(w http.ResponseWriter, r *http.Request) {
	if a.d.SyncModel == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorModelSyncUnavailable, "model sync is not configured", nil)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	nodeUUID := chi.URLParam(r, "uuid")
	var req struct {
		SourceNode   string `json:"source_node"`
		RateLimitKiB *int   `json:"rate_limit_kib_s"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	if a.d.PeerSync.SyncModelWithOptions != nil {
		rate := -1
		if req.RateLimitKiB != nil {
			rate = *req.RateLimitKiB
		}
		if req.RateLimitKiB != nil && rate < 0 {
			writeProductError(w, http.StatusBadRequest, ErrorModelSyncUnavailable, "rate_limit_kib_s must be non-negative", nil)
			return
		}
		mf, err := a.d.PeerSync.SyncModelWithOptions(r.Context(), id, nodeUUID, req.SourceNode, rate)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"state": mf.State, "transfer_id": mf.TransferID, "message": mf.Message})
		return
	}
	mf, err := a.d.SyncModel(r.Context(), id, nodeUUID)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"state": mf.State, "transfer_id": mf.TransferID, "message": mf.Message,
	})
}

func (a *apiHandlers) probeModelToNode(w http.ResponseWriter, r *http.Request) {
	if a.d.PeerSync.ProbeModel == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorModelSyncUnavailable, "model direct probe is not configured", nil)
		return
	}
	var req struct {
		SourceNode string `json:"source_node"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	if strings.TrimSpace(req.SourceNode) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSyncUnavailable, "source_node is required for direct model probe", nil)
		return
	}
	if err := a.d.PeerSync.ProbeModel(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"), req.SourceNode); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "ready"})
}

func (a *apiHandlers) cancelModelSync(w http.ResponseWriter, r *http.Request) {
	if a.d.CancelModelSync == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorModelSyncUnavailable, "model sync is not configured", nil)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	nodeUUID := chi.URLParam(r, "uuid")
	mf, err := a.d.CancelModelSync(r.Context(), id, nodeUUID)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state": mf.State, "transfer_id": mf.TransferID, "message": mf.Message,
	})
}

func writeModelFileReferenced(w http.ResponseWriter, err error) {
	params := map[string]any{}
	var referenced *store.ModelFileReferencedError
	if errors.As(err, &referenced) {
		params["deployment_ids"] = referenced.DeploymentIDs
	}
	writeProductError(w, http.StatusConflict, ErrorModelInUse, err.Error(), params)
}

func (a *apiHandlers) nodeFSList(w http.ResponseWriter, r *http.Request) {
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
		return
	}
	uuid := chi.URLParam(r, "uuid")
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path = "/"
	}
	out, err := callNodeFSList(r.Context(), a.d.CallNode, uuid, path)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	items := out.Entries
	if items == nil {
		items = []fsEntry{}
	}
	writeJSON(w, http.StatusOK, fsListing{Path: out.Path, Items: items, Total: len(items)})
}
