package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// evaluateInferenceTemplates answers what POST /inference-templates/import
// would find in the same body (YAML or a .gpuworks-catalog.tar.gz package,
// ?overwrite as for import) without writing anything: not the store, not the
// package storage. It checks parsing, validation against the component models
// the server already has, conversion to rows and the rule that assets and
// documentation need a package; it does not look for a name collision with an
// existing user spec (the import answers that with 409) and does not fetch
// weight sizes. The answer is 200 whether or not the document is valid.
func (a *apiHandlers) evaluateInferenceTemplates(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	overwrite, _ := strconv.ParseBool(r.URL.Query().Get("overwrite"))
	result := dto.InferenceTemplateEvaluation{Errors: []messages.Issue{}, Warnings: []messages.Issue{}}
	catalog, err := a.evaluateCatalog(raw, overwrite)
	if err != nil {
		result.Errors = append(result.Errors, catalogIssue(err))
	} else if issues := catalogWarningIssues(catalog); len(issues) > 0 {
		result.Warnings = issues
	}
	result.Valid = len(result.Errors) == 0
	writeJSON(w, http.StatusOK, result)
}

// evaluateCatalog is the read-only half of importInferenceTemplates.
func (a *apiHandlers) evaluateCatalog(raw []byte, overwrite bool) (inferencecatalog.FlatCatalog, error) {
	var catalog inferencecatalog.FlatCatalog
	packaged := len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b
	if packaged {
		archive, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(raw))
		if err != nil {
			return catalog, err
		}
		catalog = archive.Package.Catalog
	} else {
		decoded, err := inferencecatalog.DecodeYAML(raw)
		if err != nil {
			return catalog, err
		}
		catalog = decoded
		if err := a.validateWithComponentModels(&catalog); err != nil {
			return catalog, err
		}
		if catalogRequiresLocalFiles(catalog) && !overwrite {
			return catalog, productError(ErrorCatalogPackageRequiredForFiles, "catalog assets and documentation require a .gpuworks-catalog.tar.gz package, or overwrite of an existing packaged model", nil)
		}
	}
	if _, err := a.importedStoreModels(catalog); err != nil {
		return catalog, err
	}
	return catalog, nil
}

// catalogInvalid is err as the import answers it: a product error as
// itself, anything else (the parser's or validator's text) as CATALOG_INVALID,
// the code the evaluate endpoint gives the same document.
func catalogInvalid(err error) error {
	var product *messages.ProductError
	if errors.As(err, &product) {
		return err
	}
	return wrapProductError(messages.ErrorCatalogInvalid, err.Error(), map[string]any{"detail": err.Error()}, err)
}

// catalogIssue renders an error the way an error response does: the product
// error's own code and params, or CATALOG_INVALID with the text as fallback
// and as params.detail.
func catalogIssue(err error) messages.Issue {
	var product *messages.ProductError
	if errors.As(err, &product) {
		return messages.Issue{Warning: messages.NewWarning(product.Code(), product.Params()), Error: product.Error()}
	}
	return messages.Issue{Warning: messages.NewWarning(messages.ErrorCatalogInvalid, map[string]any{"detail": err.Error()}), Error: err.Error()}
}

// catalogWarningIssues is the catalog's non-blocking findings in wire form;
// nil when there are none.
func catalogWarningIssues(catalog inferencecatalog.FlatCatalog) []messages.Issue {
	warnings := inferencecatalog.CatalogWarnings(catalog)
	if len(warnings) == 0 {
		return nil
	}
	issues := make([]messages.Issue, 0, len(warnings))
	for _, warning := range warnings {
		issues = append(issues, catalogIssue(warning))
	}
	return issues
}
