package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Catalog writes: community/custom images, an uploaded system manifest, and
// "check for updates" against the distribution channel (ADR 0040).

type imageInput struct {
	SeriesName       string               `json:"series_name" yaml:"series_name"`
	Purpose          string               `json:"purpose" yaml:"purpose"`
	Name             string               `json:"name" yaml:"name"`
	Engine           string               `json:"engine" yaml:"engine"`
	EngineVersion    string               `json:"engine_version" yaml:"engine_version"`
	Framework        string               `json:"framework" yaml:"framework"`
	FrameworkVersion string               `json:"framework_version" yaml:"framework_version"`
	Variant          string               `json:"-" yaml:"-"`
	Arch             string               `json:"arch" yaml:"arch"`
	ImageRef         string               `json:"image_ref" yaml:"image_ref"`
	Source           string               `json:"source" yaml:"source"`
	RegistrySources  []string             `json:"registry_sources" yaml:"registry_sources"`
	RollingTag       bool                 `json:"rolling_tag" yaml:"rolling_tag"`
	Deprecated       bool                 `json:"deprecated" yaml:"deprecated"`
	DeprecatedSince  string               `json:"deprecated_since" yaml:"deprecated_since"`
	CommonArgs       []dto.ImageCommonArg `json:"common_args" yaml:"common_args"`
	CommonEnv        []dto.ImageCommonEnv `json:"common_env" yaml:"common_env"`
	Notes            string               `json:"notes" yaml:"notes"`
	Enabled          *bool                `json:"enabled" yaml:"enabled"`
}

type manifestSeriesInput struct {
	Name       string               `json:"name" yaml:"name"`
	Engine     string               `json:"engine" yaml:"engine"`
	Purpose    string               `json:"purpose" yaml:"purpose"`
	Framework  string               `json:"framework" yaml:"framework"`
	Source     string               `json:"source" yaml:"source"`
	Notes      string               `json:"notes" yaml:"notes"`
	Enabled    *bool                `json:"enabled" yaml:"enabled"`
	CommonArgs []dto.ImageCommonArg `json:"common_args" yaml:"common_args"`
	CommonEnv  []dto.ImageCommonEnv `json:"common_env" yaml:"common_env"`
	Versions   []imageInput         `json:"versions" yaml:"versions"`
}

type imageImportPayload struct {
	Series []manifestSeriesInput `json:"series" yaml:"series"`
}

// importImages accepts two bodies. A manifest (schema_version 2 or 3) is the
// system catalog uploaded by hand, applied exactly like "check for updates".
// A body without schema_version is the community/custom form the image page
// submits. Any other schema_version is an old catalog format and is refused.
func (a *apiHandlers) importImages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, imageImportMaxBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body) > imageImportMaxBytes {
		writeProductError(w, http.StatusRequestEntityTooLarge, ErrorImageImportTooLarge, "image import payload is too large", map[string]any{"maximum_mib": imageImportMaxBytes >> 20})
		return
	}
	var probe struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.SchemaVersion != nil {
		a.applyOfficialManifest(w, body)
		return
	}
	var req imageImportPayload
	if err := decodeImageImport(body, r.Header.Get("Content-Type"), &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inputs := flattenManifest(req)
	if len(inputs) == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorImageSeriesVersionRequired, "series with at least one version is required", nil)
		return
	}
	for _, input := range inputs {
		if !validateName(w, input.Name) || !validateTextField(w, "series_name", input.SeriesName, deployment.MaxNameRunes) {
			return
		}
	}
	result, err := a.upsertImageInputs(inputs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(a.imageSeriesResult(result)))
}

// imageImportMaxBytes bounds an uploaded catalog body. The system manifest is
// a few dozen KiB; the community/custom form is far smaller.
const imageImportMaxBytes = 8 << 20

// applyOfficialManifest imports a system catalog manifest and writes the
// import counts. A conflict with a community or custom row fails the whole
// import before anything is written.
func (a *apiHandlers) applyOfficialManifest(w http.ResponseWriter, body []byte) {
	result, err := imagecatalog.ImportManifest(a.d.Store, body)
	switch {
	case errors.Is(err, imagecatalog.ErrManifestInvalid):
		writeProductError(w, http.StatusBadRequest, ErrorImageManifestInvalid, err.Error(), map[string]any{"detail": err.Error()})
		return
	case errors.Is(err, imagecatalog.ErrConflict):
		writeProductError(w, http.StatusBadRequest, ErrorImageImportConflict, "image manifest conflicts with an existing image: "+err.Error(), map[string]any{"detail": err.Error()})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The import is committed; the version is only what the update view shows
	// as current, so failing to note it does not fail the import.
	_ = a.d.Store.RecordImageCatalogImport(result.CatalogVersion)
	series := make([]dto.ImageSeries, 0)
	if rows, listErr := a.d.Store.ListImageSeries(); listErr == nil {
		for i := range rows {
			if rows[i].Source == "official" {
				series = append(series, a.imageSeriesDTO(&rows[i], true))
			}
		}
	}
	skipped := make([]dto.ImageImportConflict, 0, len(result.Skipped))
	for _, conflict := range result.Skipped {
		skipped = append(skipped, dto.ImageImportConflict{Kind: conflict.Kind, Name: conflict.Name, Reason: conflict.Reason})
	}
	writeJSON(w, http.StatusOK, dto.ImageManifestImportResult{
		CatalogVersion: result.CatalogVersion,
		Created:        result.Created, Updated: result.Updated,
		Deleted: result.Deleted, Deprecated: result.Deprecated,
		Skipped: skipped, Series: series,
	})
}

func decodeImageImport(body []byte, contentType string, out any) error {
	var payload imageImportPayload
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	var jsonErr error
	if contentType != "application/yaml" && contentType != "text/yaml" && contentType != "application/x-yaml" {
		jsonErr = json.Unmarshal(body, &payload)
		if jsonErr == nil {
			return assignImageImportPayload(out, payload)
		}
	}
	if err := yaml.Unmarshal(body, &payload); err != nil {
		if jsonErr != nil {
			detail := fmt.Sprintf("json: %v; yaml: %v", jsonErr, err)
			return wrapProductError(ErrorImageImportInvalid, "invalid image import JSON/YAML: "+detail, map[string]any{"detail": detail}, err)
		}
		return err
	}
	return assignImageImportPayload(out, payload)
}

func assignImageImportPayload(out any, payload imageImportPayload) error {
	switch target := out.(type) {
	case *imageImportPayload:
		*target = payload
	default:
		return productError(ErrorImageImportUnsupported, "unsupported image import payload", nil)
	}
	return nil
}

func flattenManifest(payload imageImportPayload) []imageInput {
	var inputs []imageInput
	for _, series := range payload.Series {
		for _, version := range series.Versions {
			version.SeriesName = series.Name
			if version.Engine == "" {
				version.Engine = series.Engine
			}
			if version.Purpose == "" {
				version.Purpose = series.Purpose
			}
			if version.Framework == "" {
				version.Framework = series.Framework
			}
			if version.Source == "" {
				version.Source = series.Source
			}
			if version.Notes == "" {
				version.Notes = series.Notes
			}
			if version.Enabled == nil {
				version.Enabled = series.Enabled
			}
			if len(version.CommonArgs) == 0 {
				version.CommonArgs = series.CommonArgs
			}
			if len(version.CommonEnv) == 0 {
				version.CommonEnv = series.CommonEnv
			}
			inputs = append(inputs, version)
		}
	}
	return inputs
}

// syncImagesManifest is "check for updates": it imports the system image
// catalog the distribution channel publishes (ADR 0040). Any request body is
// ignored; where the catalog comes from is not the caller's to choose.
func (a *apiHandlers) syncImagesManifest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), updatesTimeout)
	defer cancel()
	fetched, ok := a.channelIndex(ctx, w)
	if !ok {
		return
	}
	catalog := fetched.Index.ImageCatalog
	if catalog == nil {
		writeNotPublished(w, "image_catalog")
		return
	}
	body, ok := a.channelFile(ctx, w, fetched, catalog.File)
	if !ok {
		return
	}
	a.applyOfficialManifest(w, body)
}

func (a *apiHandlers) upsertImageInputs(inputs []imageInput) ([]dto.Image, error) {
	result := make([]dto.Image, 0, len(inputs))
	for _, in := range inputs {
		if err := validateImageInput(in); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.EngineVersion) == "" {
			in.EngineVersion = community.InferEngineVersion(in.ImageRef)
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		source := in.Source
		seriesName, err := a.resolveInputSeriesName(in, source)
		if err != nil {
			return nil, err
		}
		if seriesName == "" {
			seriesName = strings.TrimSpace(in.Name)
		}
		if seriesName == "" {
			seriesName = in.Engine
		}
		purpose := strings.TrimSpace(in.Purpose)
		if purpose == "" {
			purpose = store.ImagePurposeInference
		}
		commonArgs, _ := json.Marshal(in.CommonArgs)
		commonEnv, _ := json.Marshal(in.CommonEnv)
		series, err := a.d.Store.UpsertImageSeries(&store.ImageSeries{Name: seriesName, Engine: in.Engine, Purpose: purpose, Framework: in.Framework, Source: source, Enabled: enabled, Notes: in.Notes, CommonArgs: string(commonArgs), CommonEnv: string(commonEnv)})
		if err != nil {
			return nil, err
		}
		framework := series.Framework
		if framework == "" {
			framework = in.Framework
		}
		// The framework version is one of the three facts a catalog entry
		// exists to declare, and it gates automatic selection — unlike the
		// engine version, which is display metadata and may be inferred from
		// the tag. A gating fact must be declared, not guessed: an entry that
		// states a framework but no version is exactly the half-declared row
		// that once made automatic image selection flip between call sites.
		if framework != "" && strings.TrimSpace(in.FrameworkVersion) == "" {
			return nil, productError(ErrorImageFrameworkVersionRequired, "framework_version is required when a framework is declared", map[string]any{"image": in.ImageRef, "framework": framework})
		}
		sources, _ := json.Marshal(in.RegistrySources)
		// A known rolling name is rolling whatever the form said; the form can
		// only add rolling to a tag the name does not reveal.
		img, err := a.d.Store.UpsertImage(&store.Image{SeriesID: series.ID, Name: in.Name, Engine: in.Engine, EngineVersion: in.EngineVersion, Framework: framework, FrameworkVersion: in.FrameworkVersion, Arch: in.Arch, ImageRef: in.ImageRef, Source: source, RegistrySources: string(sources), Notes: in.Notes, Enabled: enabled, RollingTag: in.RollingTag || community.IsRollingTag(in.ImageRef)})
		if err != nil {
			return nil, err
		}
		result = append(result, a.imageDTO(img, true))
	}
	return result, nil
}

// resolveInputSeriesName keeps community/custom repositories grouped by both
// repository and framework. Explicit names are operator-owned and therefore
// reject a framework collision instead of silently changing the series.
func (a *apiHandlers) resolveInputSeriesName(in imageInput, source string) (string, error) {
	name := strings.TrimSpace(in.SeriesName)
	auto := name == "" && (source == "community" || source == "custom")
	if auto {
		name = deriveInputSeriesName(in.ImageRef)
	}
	if name == "" {
		return name, nil
	}
	incoming := strings.ToLower(strings.TrimSpace(in.Framework))
	existing, err := a.d.Store.ImageSeriesByName(name)
	if err == store.ErrNotFound {
		return name, nil
	}
	if err != nil {
		return "", err
	}
	current := strings.ToLower(strings.TrimSpace(existing.Framework))
	if incoming == "" || current == "" || incoming == current {
		return name, nil
	}
	if !auto {
		return "", fmt.Errorf("image series %q already uses framework %q; use a different series name", name, existing.Framework)
	}

	base := name + " · " + incoming
	candidate := base
	for suffix := 2; ; suffix++ {
		row, lookupErr := a.d.Store.ImageSeriesByName(candidate)
		if lookupErr == store.ErrNotFound {
			return candidate, nil
		}
		if lookupErr != nil {
			return "", lookupErr
		}
		if strings.EqualFold(strings.TrimSpace(row.Framework), incoming) {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s %d", base, suffix)
	}
}

func deriveInputSeriesName(ref string) string {
	name := strings.TrimSpace(ref)
	lastSlash := strings.LastIndex(name, "/")
	if colon := strings.LastIndex(name, ":"); colon > lastSlash {
		name = name[:colon]
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 {
		host := strings.ToLower(parts[0])
		if strings.Contains(host, ".") || strings.Contains(host, ":") || host == "localhost" {
			parts = parts[1:]
		}
	}
	name = strings.TrimSpace(strings.Join(parts, "/"))
	if name == "" {
		return "custom-image"
	}
	return name
}

// imageSeriesResult keeps mutating catalog endpoints on the same two-level
// wire format as GET /images. The store
// upsert path intentionally returns version rows so callers can still inspect
// the exact refs that were accepted; the public response groups those rows by
// their newly persisted series.
func (a *apiHandlers) imageSeriesResult(rows []dto.Image) []dto.ImageSeries {
	seen := make(map[uint]struct{}, len(rows))
	out := make([]dto.ImageSeries, 0, len(rows))
	for _, row := range rows {
		if row.SeriesID == 0 {
			continue
		}
		if _, ok := seen[row.SeriesID]; ok {
			continue
		}
		seen[row.SeriesID] = struct{}{}
		if series, err := a.d.Store.GetImageSeries(row.SeriesID); err == nil {
			out = append(out, a.imageSeriesDTO(series, true))
		}
	}
	return out
}

func validateImageInput(in imageInput) error {
	ref := strings.TrimSpace(in.ImageRef)
	if ref == "" {
		return productError(ErrorImageRefRequired, "image_ref is required", nil)
	}
	if strings.Contains(strings.ToLower(ref), "@sha256:") {
		return productError(ErrorDigestImageUnsupported, "digest image refs are not supported; use a tagged image reference", nil)
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	if !strings.Contains(name, ":") {
		return productError(ErrorImageTagRequired, "image_ref must include an explicit tag", map[string]any{"image": ref})
	}
	purpose := strings.TrimSpace(in.Purpose)
	if purpose == "" {
		purpose = store.ImagePurposeInference
	}
	if purpose != store.ImagePurposeInference && purpose != store.ImagePurposeEvaluation {
		return productError(ErrorImagePurposeInvalid, "purpose must be inference or evaluation", map[string]any{"purpose": purpose})
	}
	if strings.TrimSpace(in.Engine) == "" {
		return productError(ErrorImageEngineRequired, "engine is required", nil)
	}
	// System images come only from a catalog manifest.
	if strings.EqualFold(strings.TrimSpace(in.Source), "official") {
		return productError(ErrorImageImportUnsupported, "system images can only be imported from a catalog manifest", nil)
	}
	return nil
}
