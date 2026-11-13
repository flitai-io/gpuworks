package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// metadataSources is GET /pull/metadata-sources: where the console's browser
// reads a hub's metadata, so it follows the same rules as the management
// node instead of addresses of its own. A browser runs on the operator's
// machine, maybe behind the operator's own proxy, which is why it reads at
// all; which of these addresses it can reach is its own business.
func (a *apiHandlers) metadataSources(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source != "huggingface" && source != "modelscope" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "source must be huggingface or modelscope", map[string]any{"field": "source", "detail": "source must be huggingface or modelscope"})
		return
	}
	out := dto.MetadataSources{Source: source, Bases: []string{}}
	bases, err := planning.ServerFetchCandidates(a.d.Store, source, "")
	switch {
	case containsProductCode(err, ErrorModelMetadataForbidden):
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	default:
		out.Bases = bases
	}
	writeJSON(w, http.StatusOK, out)
}

// metadataNotice says why a read of source returned nothing usable, or nil
// when it did: the rules chosen for the management node forbid every
// address, or it reached none of the ones they give. Both point at the
// rules, where a site rule can be chosen for the management node.
func (a *apiHandlers) metadataNotice(meta *modelmeta.Meta, source, sourceURL string) *dto.ProductMessage {
	if hasFetchedMetadata(meta) {
		return nil
	}
	candidates, err := planning.ServerFetchCandidates(a.d.Store, source, sourceURL)
	if containsProductCode(err, ErrorModelMetadataForbidden) {
		return deployment.ProductMessage(err)
	}
	hosts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if parsed, err := url.Parse(candidate); err == nil && parsed.Host != "" {
			hosts = append(hosts, parsed.Host)
		}
	}
	return deployment.ProductMessage(productError(ErrorModelMetadataUnreachable, "the management node reached none of "+strings.Join(hosts, ", "),
		map[string]any{"source": source, "hosts": strings.Join(hosts, ", "), "route": "/api/v1/pull/source-rules"}))
}

func (a *apiHandlers) applyFetchedMetadata(ctx context.Context, entity *store.Model, meta *modelmeta.Meta) {
	if meta == nil {
		return
	}
	if meta.WeightBytes > 0 {
		entity.WeightBytes = meta.WeightBytes
	}
	if meta.Arch != "" {
		entity.Arch = meta.Arch
	}
	// Metadata is the repository's facts. The editable API contract
	// (operations, input modalities) is not one of them: a registration seeds
	// it once (initRegisteredModelContract) and a template deployment seeds it
	// from the template, so a refresh never touches it.
	if meta.Config != nil {
		entity.Dtype = meta.Config.TorchDtype
	}
	if meta.ConfigRaw != "" {
		entity.Meta = meta.ConfigRaw
	}
	if meta.README != "" {
		entity.README = meta.README
	}
	if meta.ContextLength > 0 {
		entity.ContextLength = meta.ContextLength
	}
	if meta.Precision != "" {
		entity.Precision = meta.Precision
	}
	if meta.OriginCreatedAt != nil {
		entity.OriginCreatedAt = meta.OriginCreatedAt
	}
	if meta.OriginUpdatedAt != nil {
		entity.OriginUpdatedAt = meta.OriginUpdatedAt
	}
	if cached := a.cacheModelLogo(ctx, meta.LogoURL); cached != "" {
		entity.LogoPath = cached
	}
}

// initRegisteredModelContract seeds the editable API contract of a model a
// person registers outside the inference catalog. Repository metadata seeds
// operations; config.json seeds input modalities (text without one). Only empty
// fields are seeded. A template's weights are not registered here; the template
// deploying them states their API.
func initRegisteredModelContract(entity *store.Model) {
	var cfg *modelconfig.Config
	if raw := strings.TrimSpace(entity.Meta); raw != "" {
		cfg, _ = modelconfig.Parse(strings.NewReader(raw))
	}
	if strings.TrimSpace(entity.Operations) == "" {
		if raw, err := json.Marshal(inferenceapi.OperationsForTasks(modelmetadata.InferTasks(modelmetadata.Documents{Config: entity.Meta, README: entity.README}).Tasks)); err == nil {
			entity.Operations = string(raw)
		}
	}
	if strings.TrimSpace(entity.InputModalities) == "" {
		if raw, err := json.Marshal(inferredModelModalities(cfg)); err == nil {
			entity.InputModalities = string(raw)
		}
	}
}

func inferredModelModalities(cfg *modelconfig.Config) []string {
	modalities := modelmeta.InputModalities(cfg)
	if len(modalities) == 0 {
		return []string{"text"}
	}
	return modalities
}

func (a *apiHandlers) applyClientModelMetadata(ctx context.Context, entity *store.Model, configRaw string, weightBytes int64, readme, logoData string) {
	meta := &modelmeta.Meta{
		ConfigRaw:   configRaw,
		WeightBytes: weightBytes,
		README:      clampModelREADME(readme),
	}
	if meta.WeightBytes < 0 {
		meta.WeightBytes = 0
	}
	if meta.ConfigRaw != "" {
		if cfg, err := modelconfig.Parse(strings.NewReader(meta.ConfigRaw)); err == nil {
			meta.Config = cfg
			meta.Arch = archOfConfig(cfg)
			meta.ContextLength = modelmeta.ContextLength(cfg, meta.ConfigRaw)
			meta.Precision = modelmeta.Precision(cfg, meta.ConfigRaw)
		}
	}
	a.applyFetchedMetadata(ctx, entity, meta)
	if cached := a.cacheUploadedModelLogo(logoData); cached != "" {
		entity.LogoPath = cached
	}
}

const maxModelREADMEBytes = 512 * 1024

func clampModelREADME(readme string) string {
	if len(readme) <= maxModelREADMEBytes {
		return readme
	}
	return readme[:maxModelREADMEBytes]
}

func (a *apiHandlers) cacheUploadedModelLogo(dataURL string) string {
	if strings.TrimSpace(a.d.ModelLogoDir) == "" {
		return ""
	}
	prefix, encoded, ok := strings.Cut(dataURL, ",")
	if !ok || !strings.HasSuffix(strings.ToLower(prefix), ";base64") || len(encoded) > 3<<20 {
		return ""
	}
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(body) == 0 || len(body) > 2<<20 {
		return ""
	}
	contentType := http.DetectContentType(body)
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[contentType]
	if ext == "" {
		return ""
	}
	name := fmt.Sprintf("%x%s", sha256.Sum256(body), ext)
	if err := os.WriteFile(filepath.Join(a.d.ModelLogoDir, name), body, 0o644); err != nil {
		return ""
	}
	return name
}

func hasFetchedMetadata(meta *modelmeta.Meta) bool {
	return meta != nil && (meta.ConfigRaw != "" || meta.README != "" || meta.WeightBytes > 0 ||
		meta.OriginCreatedAt != nil || meta.OriginUpdatedAt != nil || meta.LogoURL != "" || meta.ResolvedRevision != "")
}

func (a *apiHandlers) refreshModelMetadata(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	entity, err := a.d.Store.GetModel(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if strings.EqualFold(strings.TrimSpace(entity.Source), "local") {
		writeProductError(w, http.StatusBadRequest, ErrorLocalModelMetadataRefresh, "Refresh local model metadata by registering its node path again.", nil)
		return
	}
	var req struct {
		README          string `json:"readme"`
		ConfigRaw       string `json:"config_raw"`
		WeightBytes     int64  `json:"weight_bytes"`
		LogoData        string `json:"logo_data"`
		BrowserMetadata bool   `json:"browser_metadata"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.BrowserMetadata && strings.EqualFold(strings.TrimSpace(entity.Source), "huggingface") {
		// The browser sizes the whole repository, which is not the size of a
		// model registered from one subfolder of it; keep the stored size.
		if strings.TrimSpace(entity.ArtifactSubfolder) != "" {
			req.WeightBytes = 0
		}
		if req.ConfigRaw == "" && req.README == "" && req.WeightBytes <= 0 && req.LogoData == "" {
			writeProductError(w, http.StatusBadRequest, ErrorModelMetadataUnavailable, "The browser returned no usable model metadata.", nil)
			return
		}
		a.applyClientModelMetadata(r.Context(), entity, req.ConfigRaw, req.WeightBytes, req.README, req.LogoData)
		updated, err := a.d.Store.UpsertModel(entity)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, a.modelDTO(updated, true))
		return
	}
	meta, err := a.refreshModelMetadataFor(r.Context(), entity)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if notice := a.metadataNotice(meta, entity.Source, entity.SourceURL); notice != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": notice.Message, "code": notice.Code, "params": notice.Params})
		return
	}
	a.applyFetchedMetadata(r.Context(), entity, meta)
	updated, err := a.d.Store.UpsertModel(entity)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.modelDTO(updated, true))
}

func (a *apiHandlers) cacheModelLogo(ctx context.Context, rawURL string) string {
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(a.d.ModelLogoDir) == "" {
		return ""
	}
	parsedURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Transport: a.outboundTransport(), Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many logo redirects")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return fmt.Errorf("unsupported logo redirect scheme")
		}
		return nil
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil || len(body) == 0 || len(body) >= 2<<20 {
		return ""
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(body)
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[contentType]
	if ext == "" {
		return ""
	}
	name := fmt.Sprintf("%x%s", sha256.Sum256([]byte(parsedURL.String())), ext)
	if err := os.WriteFile(filepath.Join(a.d.ModelLogoDir, name), body, 0o644); err != nil {
		return ""
	}
	return name
}

func (a *apiHandlers) getModelLogo(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(a.d.ModelLogoDir) == "" {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	m, err := a.d.Store.GetModel(parseUint(chi.URLParam(r, "id")))
	if err != nil || m.LogoPath == "" {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	name := filepath.Base(m.LogoPath)
	if name != m.LogoPath || name == "." || name == ".." {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	f, err := os.Open(filepath.Join(a.d.ModelLogoDir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	defer f.Close()
	if stat, err := f.Stat(); err != nil || stat.Size() > 2<<20 {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	contentType := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}[strings.ToLower(filepath.Ext(name))]
	if contentType == "" {
		writeErr(w, http.StatusNotFound, store.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = io.Copy(w, f)
}
