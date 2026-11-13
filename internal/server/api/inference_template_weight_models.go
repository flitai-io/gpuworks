package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listTemplateWeightModels answers which registered models serve a spec's
// declared weight sources (GET /inference-templates/weight-models
// ?template_id=&spec_source=&spec_name=). The deploy form asks it as soon as
// a spec is chosen, before any evaluation, to offer the matching models and
// to know which one a choice of a declared source reuses. The match is
// deployment.TemplateWeightMatchesModel, the rule the create path enforces.
func (a *apiHandlers) listTemplateWeightModels(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	templateID, err := strconv.ParseUint(strings.TrimSpace(query.Get("template_id")), 10, 64)
	specSource, specName := strings.TrimSpace(query.Get("spec_source")), strings.TrimSpace(query.Get("spec_name"))
	if err != nil || templateID == 0 || specSource == "" || specName == "" {
		writeProductError(w, http.StatusBadRequest, ErrorTemplateWeightModelsQueryInvalid, "template_id, spec_source and spec_name are required", nil)
		return
	}
	raw, err := a.d.Store.InferenceSpecWeightSources(uint(templateID), specSource, specName)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sources, err := inferencecatalog.DecodeWeightSources(raw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	candidates, err := a.d.Store.ModelArtifactsBySourceIDs(deployment.WeightSourceRepoIDs(sources)...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.TemplateWeightModels{Models: []dto.ModelListItem{}, Sources: make([]dto.TemplateWeightMatch, 0, len(sources))}
	serving := map[uint]bool{}
	for _, source := range sources {
		match := dto.TemplateWeightMatch{Provider: source.Provider, RepoID: source.RepoID, Revision: source.Revision, Subfolder: source.Subfolder}
		for i := range candidates {
			candidate := &candidates[i]
			if !deployment.TemplateWeightMatchesModel(source, candidate) {
				continue
			}
			serving[candidate.ID] = true
			if match.ReuseModelID == 0 && strings.EqualFold(strings.TrimSpace(candidate.Source), strings.TrimSpace(source.Provider)) {
				match.ReuseModelID = candidate.ID
			}
		}
		out.Sources = append(out.Sources, match)
	}
	for i := range candidates {
		if !serving[candidates[i].ID] {
			continue
		}
		model, err := a.d.Store.GetModel(candidates[i].ID)
		if err != nil {
			continue // deleted since the lookup
		}
		row := a.modelDTO(model, true)
		presence := modelPresenceSummary(row)
		row.README, row.Nodes, row.Storages = "", nil, nil
		out.Models = append(out.Models, dto.ModelListItem{Model: row, Presence: presence})
	}
	writeJSON(w, http.StatusOK, out)
}
