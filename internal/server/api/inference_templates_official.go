package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The official inference template catalog (ADR 0040): "check for updates"
// reads it from the distribution channel, an administrator can upload it, and
// both replace the upstream specs the way the embedded catalog does.

// syncInferenceTemplates is the inference template catalog's "check for
// updates": it applies the official catalog package the distribution channel
// publishes (ADR 0040). Any request body is ignored.
func (a *apiHandlers) syncInferenceTemplates(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), updatesTimeout)
	defer cancel()
	fetched, ok := a.channelIndex(ctx, w)
	if !ok {
		return
	}
	catalog := fetched.Index.TemplateCatalog
	if catalog == nil {
		writeNotPublished(w, "template_catalog")
		return
	}
	if catalog.SchemaVersion > inferencecatalog.PackageSchemaVersion {
		writeProductError(w, http.StatusConflict, ErrorUpdateCatalogIncompatible, "the published inference template catalog needs a newer server",
			map[string]any{"schema_version": catalog.SchemaVersion, "supported": inferencecatalog.PackageSchemaVersion})
		return
	}
	raw, ok := a.channelFile(ctx, w, fetched, catalog.File)
	if !ok {
		return
	}
	a.applyOfficialInferenceCatalog(w, raw)
}

// uploadOfficialInferenceCatalog applies an official catalog package an
// administrator uploads, for an installation that cannot reach the channel.
// It replaces the upstream catalog exactly as "check for updates" does; a
// user's own templates go through /inference-templates/import instead.
func (a *apiHandlers) uploadOfficialInferenceCatalog(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a.applyOfficialInferenceCatalog(w, raw)
}

// applyOfficialInferenceCatalog reconciles the upstream specs to a catalog
// package (ADR 0011: user specs are untouched). The last import wins; the
// embedded catalog comes back only when the server build changes (ADR 0040).
func (a *apiHandlers) applyOfficialInferenceCatalog(w http.ResponseWriter, raw []byte) {
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		writeProductError(w, http.StatusBadRequest, ErrorOfficialCatalogPackageRequired, "an official inference template catalog must be a .tar.gz catalog package", nil)
		return
	}
	if a.d.CatalogPackages == nil {
		writeOfficialCatalogRejected(w, errors.New("catalog package storage is unavailable"))
		return
	}
	// An official package is read tolerantly: the models and specs this build
	// cannot read are left out and reported, the rest is applied (a user's own
	// import stays strict, see decodeInferenceCatalog).
	archive, _, err := inferencecatalog.ReadPackageArchiveTolerant(bytes.NewReader(raw))
	if err != nil {
		writeOfficialCatalogRejected(w, err)
		return
	}
	pkg, skipped, digest, created, err := a.d.CatalogPackages.PutOfficial(archive.FS)
	if err != nil {
		writeOfficialCatalogRejected(w, err)
		return
	}
	models, err := a.importedStoreModels(pkg.Catalog)
	if err != nil {
		a.removeCatalogPackageOnFailure(digest, created)
		writeOfficialCatalogRejected(w, err)
		return
	}
	for index := range models {
		models[index].Source = "upstream"
		models[index].PackageDigest = digest
		for specIndex := range models[index].Specs {
			models[index].Specs[specIndex].Source = "upstream"
			models[index].Specs[specIndex].PackageDigest = digest
		}
	}
	// What was skipped keeps the row this server already has, if any: an
	// update must not take away a spec that works today.
	keep := make([]store.InferenceCatalogKeep, len(skipped))
	left := make([]dto.OfficialInferenceCatalogSkipped, len(skipped))
	for index, item := range skipped {
		keep[index] = store.InferenceCatalogKeep{ModelID: item.ModelID, Spec: item.Spec}
		left[index] = dto.OfficialInferenceCatalogSkipped{ModelID: item.ModelID, Spec: item.Spec, Reason: item.Reason}
	}
	if err := a.d.Store.ReconcileInferenceCatalogKeeping(models, keep); err != nil {
		a.removeCatalogPackageOnFailure(digest, created)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.pruneUnreferencedCatalogPackages()
	writeJSON(w, http.StatusOK, dto.OfficialInferenceCatalogResult{Models: len(models), PackageDigest: digest, Version: a.inferenceCatalogVersion(digest), Skipped: left})
}

// writeOfficialCatalogRejected reports an official package this server cannot
// read or validate. Templates are published against the newest server, so the
// usual cause is a field or rule this build predates; the parser's own error
// stays attached for the cases where it is not.
func writeOfficialCatalogRejected(w http.ResponseWriter, err error) {
	writeProductError(w, http.StatusBadRequest, ErrorOfficialCatalogNeedsNewerServer,
		"this inference template catalog requires the latest server: "+err.Error(),
		map[string]any{"error": err.Error()})
}
