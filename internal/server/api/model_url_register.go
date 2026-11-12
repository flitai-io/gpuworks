package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// urlModelPreview is what the register form shows after a directory URL is
// parsed and before anything is written. It mirrors the local-path preview:
// the operator sees the identity that will be stored plus enough of the
// model's own facts to confirm the URL points at the intended weights.
type urlModelPreview struct {
	SourceURL       string   `json:"source_url"`
	SourceID        string   `json:"source_id"`
	Name            string   `json:"name"`
	Arch            string   `json:"arch,omitempty"`
	Precision       string   `json:"precision,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	WeightBytes     int64    `json:"weight_bytes"`
	FilesCount      int      `json:"files_count"`
	InputModalities []string `json:"input_modalities,omitempty"`
}

func (a *apiHandlers) previewURLModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceURLRequired, "url is required", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	client := outbound.MetadataClient(&http.Client{Transport: a.outboundTransport()})
	defer client.CloseIdleConnections()
	preview, err := modelmeta.PreviewDirectoryWithClient(ctx, req.URL, client)
	if err != nil {
		writeDirectoryError(w, req.URL, err)
		return
	}
	writeJSON(w, http.StatusOK, urlPreviewDTO(preview))
}

func (a *apiHandlers) registerURLModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceURL string `json:"source_url"`
		SourceID  string `json:"source_id"`
		Name      string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) || !validateName(w, req.Name) {
		return
	}
	if strings.TrimSpace(req.SourceURL) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceURLRequired, "source_url is required", nil)
		return
	}
	// The directory is read again rather than trusted from the request: the
	// preview the operator confirmed may be minutes old, and registering a
	// directory that no longer holds a model would produce a row nothing can
	// deploy.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	client := outbound.MetadataClient(&http.Client{Transport: a.outboundTransport()})
	defer client.CloseIdleConnections()
	preview, err := modelmeta.PreviewDirectoryWithClient(ctx, req.SourceURL, client)
	if err != nil {
		writeDirectoryError(w, req.SourceURL, err)
		return
	}
	sourceID := strings.TrimSpace(req.SourceID)
	if sourceID == "" {
		sourceID = preview.SourceID
	}
	if sourceID == "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceIDRequired, "source_id is required", nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = preview.Name
	}
	if name == "" {
		name = sourceID
	}
	if !validateName(w, name) {
		return
	}
	// A directory index has no commit history, so the revision stays empty and
	// the artifact path keeps the unpinned identity. Everything else comes from
	// the same metadata path a Hugging Face registration uses, with the
	// directory itself as the endpoint.
	entity := &store.Model{
		Source: modelmeta.SourceURL, SourceID: sourceID, SourceURL: preview.SourceURL,
		Revision: "", Name: name,
	}
	var notice *dto.ProductMessage
	if meta, err := a.deployments().FetchModelMeta(r.Context(), entity.Source, entity.SourceID, entity.Revision, entity.ArtifactSubfolder, entity.SourceURL); err == nil {
		notice = a.metadataNotice(meta, entity.Source, entity.SourceURL)
		a.applyFetchedMetadata(r.Context(), entity, meta)
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

func urlPreviewDTO(preview *modelmeta.DirectoryPreview) urlModelPreview {
	return urlModelPreview{
		SourceURL: preview.SourceURL, SourceID: preview.SourceID, Name: preview.Name,
		Arch: preview.Arch, Precision: preview.Precision, ContextLength: preview.ContextLength,
		WeightBytes: preview.WeightBytes, FilesCount: preview.FilesCount,
		InputModalities: preview.InputModalities,
	}
}

// writeDirectoryError distinguishes invalid input from directory failures
// using the product errors a client renders, so the operator is told what
// to change rather than being shown a transport message.
func writeDirectoryError(w http.ResponseWriter, raw string, err error) {
	params := map[string]any{"url": strings.TrimSpace(raw), "detail": "the model source could not be read"}
	switch {
	case errors.Is(err, modelmeta.ErrDirectoryURLInvalid):
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceURLInvalid,
			"model directory URL must be an absolute HTTP or HTTPS URL without embedded credentials", nil)
	case errors.Is(err, modelmeta.ErrDirectoryNotAModel):
		writeProductError(w, http.StatusBadRequest, ErrorHTTPDirectoryNotAModel,
			"the directory lists no files, or its config.json does not parse", params)
	case errors.Is(err, modelmeta.ErrDirectoryTooLarge):
		writeProductError(w, http.StatusBadRequest, ErrorHTTPDirectoryTooLarge,
			"the directory has more files or nesting than a model repository can have", params)
	default:
		writeProductError(w, http.StatusBadGateway, ErrorHTTPDirectoryUnreachable,
			"the directory could not be read as an HTTP directory index", params)
	}
}

// refreshModelMetadataFor re-reads a model's metadata from its own source.
func (a *apiHandlers) refreshModelMetadataFor(ctx context.Context, model *store.Model) (*modelmeta.Meta, error) {
	return a.deployments().FetchModelMeta(ctx, model.Source, model.SourceID, model.Revision, model.ArtifactSubfolder, model.SourceURL)
}
