package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) listImages(w http.ResponseWriter, r *http.Request) {
	series, err := a.d.Store.ListImageSeries()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if source := strings.TrimSpace(r.URL.Query().Get("source")); source != "" {
		filtered := series[:0]
		for _, row := range series {
			if row.Source == source {
				filtered = append(filtered, row)
			}
		}
		series = filtered
	}
	out := make([]dto.ImageSeries, 0, len(series))
	for i := range series {
		out = append(out, a.imageSeriesDTO(&series[i], false))
	}
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

func (a *apiHandlers) getImage(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	series, err := a.d.Store.GetImageSeries(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, a.imageSeriesDTO(series, true))
}

func (a *apiHandlers) listImageVersions(w http.ResponseWriter, r *http.Request) {
	images, err := a.d.Store.ListImages()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if source := strings.TrimSpace(r.URL.Query().Get("source")); source != "" {
		filtered := images[:0]
		for _, row := range images {
			if row.Source == source {
				filtered = append(filtered, row)
			}
		}
		images = filtered
	}
	writeJSON(w, http.StatusOK, dto.NewList(a.imageDTOs(images, false)))
}

func (a *apiHandlers) patchImageSeries(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled    *bool                 `json:"enabled"`
		CommonArgs *[]dto.ImageCommonArg `json:"common_args"`
		CommonEnv  *[]dto.ImageCommonEnv `json:"common_env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorImageSeriesPatchInvalid, "invalid image series patch", nil)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	if req.CommonArgs != nil || req.CommonEnv != nil {
		current, err := a.d.Store.GetImageSeries(id)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err)
			return
		}
		if current.Source == "official" {
			writeProductError(w, http.StatusBadRequest, ErrorOfficialImageSeriesManaged, "system image series common candidates are maintained by the catalog", nil)
			return
		}
		args := []dto.ImageCommonArg{}
		env := []dto.ImageCommonEnv{}
		if req.CommonArgs != nil {
			args = *req.CommonArgs
		} else {
			_ = json.Unmarshal([]byte(current.CommonArgs), &args)
		}
		if req.CommonEnv != nil {
			env = *req.CommonEnv
		} else {
			_ = json.Unmarshal([]byte(current.CommonEnv), &env)
		}
		for _, item := range args {
			if strings.TrimSpace(item.Flag) == "" {
				writeProductError(w, http.StatusBadRequest, ErrorImageCommonArgFlagRequired, "common_args flag is required", nil)
				return
			}
		}
		for _, item := range env {
			if strings.TrimSpace(item.Key) == "" {
				writeProductError(w, http.StatusBadRequest, ErrorImageCommonEnvKeyRequired, "common_env key is required", nil)
				return
			}
		}
		argsJSON, _ := json.Marshal(args)
		envJSON, _ := json.Marshal(env)
		row, err := a.d.Store.UpdateImageSeriesCommon(id, string(argsJSON), string(envJSON))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, a.imageSeriesDTO(row, true))
		return
	}
	if req.Enabled == nil {
		writeProductError(w, http.StatusBadRequest, ErrorImageSeriesPatchEmpty, "enabled or common candidates are required", nil)
		return
	}
	row, err := a.d.Store.SetImageSeriesEnabled(id, *req.Enabled)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, a.imageSeriesDTO(row, true))
}

func (a *apiHandlers) patchImageVersion(w http.ResponseWriter, r *http.Request) {
	// A version carries acquisition facts only (ADR 0031): whether it is
	// offered, and the addresses this cluster fetches it from. Any other key is
	// refused rather than ignored, so a client still sending launch defaults
	// learns they are gone.
	var req struct {
		Enabled         *bool    `json:"enabled"`
		RegistrySources []string `json:"registry_sources"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorImageVersionPatchInvalid, "invalid image version patch", nil)
		return
	}
	if req.Enabled == nil && req.RegistrySources == nil {
		writeProductError(w, http.StatusBadRequest, ErrorImageVersionPatchEmpty, "enabled or registry_sources is required", nil)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	var row *store.Image
	var err error
	// An acquisition address is deployment data, so it may be added to an
	// official version (which is otherwise catalog-owned and read-only): the
	// catalog cannot know which archive an operator can reach.
	if req.RegistrySources != nil {
		normalized, sourcesErr := normalizeRegistrySources(req.RegistrySources)
		if sourcesErr != nil {
			writeProductError(w, http.StatusBadRequest, ErrorImageArchiveURLInvalid, "invalid image archive URL: "+sourcesErr.Error(), map[string]any{"detail": sourcesErr.Error()})
			return
		}
		encoded, _ := json.Marshal(normalized)
		row, err = a.d.Store.UpdateImageRegistrySources(id, string(encoded))
	}
	if err == nil && req.Enabled != nil {
		row, err = a.d.Store.SetImageEnabled(id, *req.Enabled)
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, a.imageDTO(row, true))
}

func writeImageDeleteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrReferenced):
		writeProductError(w, http.StatusConflict, ErrorImageInUse, "The image is still referenced by a deployment, an unfinished instance, or a model. Delete those references first.", nil)
	case strings.Contains(err.Error(), "official"):
		writeProductError(w, http.StatusBadRequest, ErrorOfficialImageDeleteForbidden, "System images are managed by the catalog and can only be deprecated, not deleted directly.", nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (a *apiHandlers) deleteImage(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.DeleteImage(parseUint(chi.URLParam(r, "id"))); err != nil {
		writeImageDeleteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiHandlers) deleteImageSeries(w http.ResponseWriter, r *http.Request) {
	if err := a.d.Store.DeleteImageSeries(parseUint(chi.URLParam(r, "id"))); err != nil {
		writeImageDeleteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiHandlers) imageDTO(image *store.Image, withNodes bool) dto.Image {
	return a.imageDTOs([]store.Image{*image}, withNodes)[0]
}

// imageDTOs renders versions together: reference counts, node rows and the
// node list are each read once for the whole set. withNodes adds the per-node
// rows; without it a version carries only its presence counts.
func (a *apiHandlers) imageDTOs(images []store.Image, withNodes bool) []dto.Image {
	out := make([]dto.Image, 0, len(images))
	if len(images) == 0 {
		return out
	}
	refs := make([]string, 0, len(images))
	ids := make([]uint, 0, len(images))
	for i := range images {
		refs = append(refs, images[i].ImageRef)
		ids = append(ids, images[i].ID)
	}
	counts, _ := a.d.Store.ImageReferenceCounts(refs)
	nodeRows, _ := a.d.Store.ImageNodesForImages(ids)
	rowsByImage := make(map[uint][]store.ImageNode, len(images))
	for _, row := range nodeRows {
		rowsByImage[row.ImageVersionID] = append(rowsByImage[row.ImageVersionID], row)
	}
	// Only a per-node listing needs the node rows themselves.
	var nodes []store.Node
	totalNodes, _ := a.d.Store.CountNodes()
	if withNodes {
		nodes, _ = a.d.Store.NodeIdentities()
	}
	seriesByID := map[uint]*store.ImageSeries{}
	for i := range images {
		image := &images[i]
		purpose := image.Purpose
		framework := image.Framework
		effectiveEnabled := image.Enabled
		series, cached := seriesByID[image.SeriesID]
		if !cached {
			series, _ = a.d.Store.GetImageSeries(image.SeriesID)
			seriesByID[image.SeriesID] = series
		}
		if series != nil {
			purpose = series.Purpose
			// The series is authoritative for framework. The version field is kept
			// only as deprecated migration storage and must not leak stale values.
			framework = series.Framework
			effectiveEnabled = effectiveEnabled && series.Enabled
		}
		item := dto.Image{ID: image.ID, SeriesID: image.SeriesID, Purpose: purpose, Name: image.Name, Engine: image.Engine, EngineVersion: image.EngineVersion, Framework: framework, FrameworkVersion: image.FrameworkVersion, DriverRequirement: accelerator.DriverRequirement(framework, image.FrameworkVersion), Arch: image.Arch, ImageRef: image.ImageRef, Source: image.Source, Deprecated: image.Deprecated, Enabled: image.Enabled, EffectiveEnabled: effectiveEnabled, Notes: image.Notes, SizeBytes: image.SizeBytes, DeprecatedSince: image.DeprecatedSince, RollingTag: image.RollingTag, UpstreamGone: image.UpstreamGone, ReferenceCount: counts[image.ImageRef], TotalNodes: totalNodes}
		_ = json.Unmarshal([]byte(image.RegistrySources), &item.RegistrySources)
		for _, row := range rowsByImage[image.ID] {
			if row.State == store.ImageStatePresent {
				item.PresentNodes++
				if row.RuntimeSizeBytes != nil && *row.RuntimeSizeBytes >= 0 {
					item.LocalRuntimeSizeBytes += *row.RuntimeSizeBytes
					item.LocalSizeKnownNodes++
				}
			}
		}
		if withNodes {
			item.Nodes = a.imageNodeDTOs(rowsByImage[image.ID], nodes)
		}
		out = append(out, item)
	}
	return out
}

func (a *apiHandlers) imageNodeDTOs(rows []store.ImageNode, nodes []store.Node) []dto.ImageNode {
	byNode := make(map[uint]store.ImageNode, len(rows))
	for _, row := range rows {
		byNode[row.NodeID] = row
	}
	out := make([]dto.ImageNode, 0, len(nodes))
	for _, node := range nodes {
		row, ok := byNode[node.ID]
		item := dto.ImageNode{NodeUUID: node.UUID, NodeName: nodeDisplayName(&node), NodeState: node.ConnectionState(), State: store.ImageStateAbsent}
		if ok {
			item.State, item.SizeBytes, item.TransferredBytes, item.RateBPS, item.RateLimitKiB, item.LastRateLimitKiB, item.TransferID, item.Source, item.Message = row.State, row.SizeBytes, row.TransferredBytes, row.RateBPS, row.RateLimitKiB, row.LastRateLimitKiB, row.TransferID, row.Source, row.Message
			item.ImageDigestID = row.ImageDigestID
			item.TransferErrorCode = row.TransferErrorCode
			if !row.UpdatedAt.IsZero() {
				item.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
			}
			if row.SourceNodeID != 0 {
				if source, err := a.d.Store.NodeByID(row.SourceNodeID); err == nil {
					item.SourceNodeUUID, item.SourceNodeName = source.UUID, nodeDisplayName(source)
				}
			}
		}
		out = append(out, item)
	}
	return out
}

func (a *apiHandlers) imageSeriesDTO(series *store.ImageSeries, withVersions bool) dto.ImageSeries {
	out := dto.ImageSeries{ID: series.ID, Name: series.Name, Engine: series.Engine, Purpose: series.Purpose, Framework: series.Framework, Source: series.Source, Notes: series.Notes, Enabled: series.Enabled}
	_ = json.Unmarshal([]byte(series.CommonArgs), &out.CommonArgs)
	_ = json.Unmarshal([]byte(series.CommonEnv), &out.CommonEnv)
	versions, _ := a.d.Store.VersionsForSeries(series.ID)
	if len(versions) == 0 {
		return out
	}
	ids := make([]uint, 0, len(versions))
	for i := range versions {
		ids = append(ids, versions[i].ID)
	}
	rows, _ := a.d.Store.ImageNodesForImages(ids)
	seen := make(map[uint]bool)
	for _, row := range rows {
		if row.State == store.ImageStatePresent {
			seen[row.NodeID] = true
		}
	}
	if withVersions {
		out.Versions = a.imageDTOs(versions, true)
	}
	out.TotalNodes, _ = a.d.Store.CountNodes()
	out.VersionCount = len(versions)
	out.PresentNodes = len(seen)
	return out
}
