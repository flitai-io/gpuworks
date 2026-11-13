package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// previewImageArchiveURL reports what a `docker save` archive published over
// plain HTTP contains, so an operator pastes an address and sees the real
// image reference instead of guessing it from the file name. The archive is
// never downloaded: the server reads its headers and its tail.
func (a *apiHandlers) previewImageArchiveURL(w http.ResponseWriter, r *http.Request) {
	var req dto.ImageArchivePreviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLRequired, "an image archive URL is required", nil)
		return
	}
	normalized, err := imagecatalog.NormalizeArchiveURL(req.URL)
	if err != nil {
		if strings.TrimSpace(req.URL) == "" {
			writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLRequired, "an image archive URL is required", nil)
			return
		}
		writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLInvalid, "invalid image archive URL: "+err.Error(), map[string]any{"detail": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	client := imagecatalog.ArchiveHTTPClient()
	// MetadataClient bounds each attempt, including its response body.
	client.Timeout = 0
	client.Transport = a.outboundTransport()
	client = outbound.MetadataClient(client)
	defer client.CloseIdleConnections()
	peek, err := imagecatalog.PeekArchiveURL(ctx, client, normalized)
	if err != nil {
		writeArchivePeekError(w, normalized, err)
		return
	}
	tags := peek.RepoTags
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, http.StatusOK, dto.ImageArchivePreview{
		URL: normalized, SizeBytes: peek.SizeBytes, RepoTags: tags, Previewable: peek.Previewable,
	})
}

// writeArchivePeekError keeps a policy refusal, an upstream HTTP status and a
// transport failure apart. The response names the host and the status code,
// never the URL.
func writeArchivePeekError(w http.ResponseWriter, normalized string, err error) {
	host := imagecatalog.ArchiveURLHost(normalized)
	if errors.Is(err, outbound.ErrTargetNeedsAllow) {
		writeProductError(w, http.StatusForbidden, ErrorImageArchiveURLPolicyDenied, "this archive host is not allowed yet",
			map[string]any{"host": host, "allow_host": imagecatalog.ArchiveURLHostname(normalized), "approvable": true, "config_key": "outbound_allow_private_hosts"})
		return
	}
	if errors.Is(err, outbound.ErrTargetDenied) {
		writeProductError(w, http.StatusForbidden, ErrorImageArchiveURLPolicyBlocked, "this archive host cannot be allowed",
			map[string]any{"host": host})
		return
	}
	if status, ok := imagecatalog.UpstreamStatus(err); ok {
		writeProductError(w, http.StatusBadGateway, ErrorImageArchiveURLUpstreamStatus, "the archive server returned an unexpected status",
			map[string]any{"host": host, "upstream_status": status})
		return
	}
	if imagecatalog.ArchiveURLInvalid(err) {
		writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLInvalid, "invalid image archive URL: "+err.Error(), map[string]any{"detail": err.Error()})
		return
	}
	writeProductError(w, http.StatusBadGateway, ErrorImageArchiveURLUnreachable, "the image archive could not be read",
		map[string]any{"host": host, "detail": "the image archive could not be read"})
}

// decodeImageNodeSyncRequest reads the distribution request body. A missing or
// unreadable body is a plain pull, so decoding never fails the call.
func decodeImageNodeSyncRequest(r *http.Request) dto.ImageNodeSyncRequest {
	var req dto.ImageNodeSyncRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	req.Source = strings.TrimSpace(req.Source)
	if strings.EqualFold(req.Source, "default") {
		req.Source = ""
	}
	req.SourceNode = strings.TrimSpace(req.SourceNode)
	if req.RateLimitKiB < 0 {
		req.RateLimitKiB = -1
	}
	req.ArchiveURL = strings.TrimSpace(req.ArchiveURL)
	return req
}

// fetchImageForNode starts one distribution request: a copy from a named node,
// a download of an archive URL declared on the image, or else a pull from
// wherever the node's pull source rules say. A registry is never named here:
// the pull settings decide, the same way they do for a launch.
func (a *apiHandlers) fetchImageForNode(w http.ResponseWriter, r *http.Request, req dto.ImageNodeSyncRequest) {
	imageID := parseUint(chi.URLParam(r, "id"))
	if req.SourceNode != "" {
		if a.d.PeerSync.SyncImageWithRate == nil {
			writeProductError(w, http.StatusServiceUnavailable, ErrorImageSyncUnavailable, "direct image sync is not configured", nil)
			return
		}
		if req.RateLimitKiB < 0 {
			writeProductError(w, http.StatusBadRequest, ErrorImageSyncUnavailable, "rate_limit_kib_s must be non-negative", nil)
			return
		}
		row, err := a.d.PeerSync.SyncImageWithRate(r.Context(), imageID, chi.URLParam(r, "uuid"), req.SourceNode, req.RateLimitKiB)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, dto.ImageNodeSyncResponse{State: row.State, TransferID: row.TransferID, Message: row.Message})
		return
	}
	fetch := imagecatalog.FetchRequest{Source: req.Source, Relay: req.Relay, Force: req.Force}
	if req.Source != "" && !fetch.IsURLFetch() {
		writeProductError(w, http.StatusBadRequest, ErrorImageSourceRemoved,
			"a registry can no longer be named for a pull; nodes pull through the pull source rules",
			map[string]any{"source": req.Source, "route": "/api/v1/pull/source-rules"})
		return
	}
	if a.d.ImageFetch.Pull == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorImageSyncUnavailable, "image acquisition is not configured", nil)
		return
	}
	if fetch.IsURLFetch() {
		declared, ok := a.declaredArchiveURL(w, imageID, req.ArchiveURL)
		if !ok {
			return
		}
		fetch.ArchiveURL = declared
	}
	row, err := a.d.ImageFetch.Pull(r.Context(), imageID, chi.URLParam(r, "uuid"), fetch)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, dto.ImageNodeSyncResponse{State: row.State, TransferID: row.TransferID, Message: row.Message})
}

// declaredArchiveURL resolves the requested archive URL against the image's
// declared registry_sources. Only an address already recorded on the image can
// be fetched, so this endpoint cannot be used to make a node download an
// arbitrary URL.
func (a *apiHandlers) declaredArchiveURL(w http.ResponseWriter, imageID uint, raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLRequired, "an image archive URL is required", nil)
		return "", false
	}
	normalized, err := imagecatalog.NormalizeArchiveURL(raw)
	if err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLInvalid, "invalid image archive URL: "+err.Error(), map[string]any{"detail": err.Error()})
		return "", false
	}
	image, err := a.d.Store.GetImage(imageID)
	if err != nil {
		status := http.StatusInternalServerError
		if err == store.ErrNotFound {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return "", false
	}
	for _, entry := range imageArchiveURLs(image) {
		if entry == normalized {
			return normalized, true
		}
	}
	writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLNotDeclared,
		"this image archive URL is not declared on the image; add it to the image's registry sources first",
		map[string]any{"url": normalized, "image": image.ImageRef})
	return "", false
}

// imageArchiveURLs projects the archive URLs declared on an image, normalized
// the one way NormalizeArchiveURL spells them. Entries that are registry
// references, and entries that are not usable URLs, are skipped.
func imageArchiveURLs(image *store.Image) []string {
	if image == nil {
		return nil
	}
	var entries []string
	_ = json.Unmarshal([]byte(image.RegistrySources), &entries)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !imagecatalog.IsArchiveURL(entry) {
			continue
		}
		normalized, err := imagecatalog.NormalizeArchiveURL(entry)
		if err != nil {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

// normalizeRegistrySources spells every declared source the one way the rest
// of the system reads it. Archive URLs are normalized and validated here, so a
// URL that reaches registry_sources is always one a node can be asked to
// fetch; registry references are only trimmed, because their own validation
// lives in the pull path.
func normalizeRegistrySources(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if imagecatalog.IsArchiveURL(entry) {
			normalized, err := imagecatalog.NormalizeArchiveURL(entry)
			if err != nil {
				return nil, err
			}
			entry = normalized
		}
		out = append(out, entry)
	}
	return out, nil
}
