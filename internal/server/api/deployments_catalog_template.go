package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// CatalogTemplates writes a running deployment as an inference template. The
// implementation (internal/server/app) reads the deployment, sends the
// smallest request each cheap operation takes and builds the template with
// package templateexport; see docs/how-to/save-deployment-as-template.md.
type CatalogTemplates interface {
	// Export answers store.ErrNotFound for an unknown deployment and a
	// messages.ErrorTemplateExportUnavailable or deploy.ErrorLaunchNotExportable
	// product error for one a spec cannot describe.
	Export(ctx context.Context, deploymentID uint, includeEnv []string) (dto.CatalogTemplateExport, error)
}

// exportCatalogTemplate answers POST /deployments/{id}/catalog-template. Anyone
// who can see the deployment may export it; saving the result into the
// catalog is the import, which stays an administrator's.
func (a *apiHandlers) exportCatalogTemplate(w http.ResponseWriter, r *http.Request) {
	if a.d.CatalogTemplates == nil {
		http.NotFound(w, r)
		return
	}
	var body dto.CatalogTemplateExportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := a.d.CatalogTemplates.Export(r.Context(), parseUint(chi.URLParam(r, "id")), body.IncludeEnv)
	if err != nil {
		writeCatalogTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeCatalogTemplateError(w http.ResponseWriter, err error) {
	var product *messages.ProductError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.As(err, &product) && (product.Code() == messages.ErrorTemplateExportUnavailable || product.Code() == deploy.ErrorLaunchNotExportable):
		writeErr(w, http.StatusConflict, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}
