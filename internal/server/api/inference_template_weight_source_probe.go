package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// weightSourceProbeTimeout bounds one test of a source: a site that does not
// answer is reported, not waited for.
const weightSourceProbeTimeout = 20 * time.Second

// probeTemplateWeightSource answers whether the deploy form may register and
// deploy from one of a spec's declared weight sources (GET
// /inference-templates/weight-source-probe?template_id=&spec_source=
// &spec_name=&provider=&repo_id=&revision=&subfolder=). The management node
// reads the source as a registration would, and the answer carries what the
// site said. A source whose choice reuses a registered model with files in
// place is not read at all: nothing would be downloaded, so a site without
// internet access still deploys it.
func (a *apiHandlers) probeTemplateWeightSource(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	field := func(name string) string { return strings.TrimSpace(query.Get(name)) }
	templateID, err := strconv.ParseUint(field("template_id"), 10, 64)
	if err != nil || templateID == 0 || field("spec_source") == "" || field("spec_name") == "" || field("provider") == "" || field("repo_id") == "" {
		writeProductError(w, http.StatusBadRequest, messages.ErrorWeightSourceProbeQueryInvalid, "template_id, spec_source, spec_name, provider and repo_id are required", nil)
		return
	}
	raw, err := a.d.Store.InferenceSpecWeightSources(uint(templateID), field("spec_source"), field("spec_name"))
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
	asked := inferencecatalog.WeightSource{Provider: field("provider"), RepoID: field("repo_id"), Revision: field("revision"), Subfolder: field("subfolder")}
	declared := false
	for _, source := range sources {
		declared = declared || source == asked
	}
	if !declared {
		writeProductError(w, http.StatusBadRequest, messages.ErrorWeightSourceNotDeclared, "the spec declares no such weight source",
			map[string]any{"provider": asked.Provider, "repo": asked.RepoID})
		return
	}
	present, err := a.weightSourceFilesInPlace(asked)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if present {
		writeJSON(w, http.StatusOK, dto.WeightSourceProbe{Proceed: true, Present: true})
		return
	}
	provider := strings.ToLower(asked.Provider)
	ctx, cancel := context.WithTimeout(r.Context(), weightSourceProbeTimeout)
	defer cancel()
	meta, err := a.deployments().FetchModelMeta(ctx, provider, asked.RepoID, asked.Revision, asked.Subfolder, "")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	out := dto.WeightSourceProbe{}
	if meta != nil {
		out.Host = hostOf(meta.Endpoint)
	}
	out.Reason = a.metadataNotice(meta, &store.Model{Source: provider, SourceID: asked.RepoID, Revision: asked.Revision})
	out.Proceed = out.Reason == nil
	writeJSON(w, http.StatusOK, out)
}

// weightSourceFilesInPlace reports whether choosing source reuses a
// registered model (the newest of its provider that serves it, as
// listTemplateWeightModels answers) whose files are ready on a node or in a
// shared storage.
func (a *apiHandlers) weightSourceFilesInPlace(source inferencecatalog.WeightSource) (bool, error) {
	candidates, err := a.d.Store.ModelArtifactsBySourceIDs(deployment.WeightSourceRepoIDs([]inferencecatalog.WeightSource{source})...)
	if err != nil {
		return false, err
	}
	for i := range candidates {
		candidate := &candidates[i]
		if !deployment.TemplateWeightOffersModel(source, candidate) || !strings.EqualFold(strings.TrimSpace(candidate.Source), strings.TrimSpace(source.Provider)) {
			continue
		}
		model, err := a.d.Store.GetModel(candidate.ID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil // deleted since the lookup
		}
		if err != nil {
			return false, err
		}
		presence := modelPresenceSummary(a.modelDTO(model, true))
		return presence.ReadyNodes+presence.CachedNodes+presence.ReadyStorages > 0, nil
	}
	return false, nil
}
