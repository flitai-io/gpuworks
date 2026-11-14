package api

import (
	"context"
	"encoding/json"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// deployments is the deployment use cases over the handlers' dependencies.
// The service keeps no state, so the handlers build it per request.
func (a *apiHandlers) deployments() *deployment.Service {
	// Without the app's fetcher the server reads a source itself, through
	// the outbound policy every request it makes obeys; a model diagnostic
	// still probes only with the app's fetcher, as it always has.
	readMeta := func(ctx context.Context, source, repo, revision, subfolder string, candidates []string) (*modelmeta.Meta, error) {
		client := &http.Client{Transport: a.outboundTransport()}
		defer client.CloseIdleConnections()
		return modelmeta.FetchFrom(ctx, source, repo, revision, subfolder, candidates, client, planning.ModelLogins(a.d.Store))
	}
	return deployment.New(deployment.Deps{
		Store: a.d.Store, Log: a.d.Log, Online: a.d.Online, UCXNetlinkHang: a.ucxNetlinkHangOf(), NodeSync: a.d.NodeSync, CallNode: a.d.CallNode,
		ImageExists: a.d.ImageExists, CatalogPackages: a.d.CatalogPackages, FetchModelMeta: a.d.FetchModelMeta, ReadModelMeta: readMeta,
		Networks: a.d.Networks, AfterAutomaticPick: afterAutomaticPick,
	})
}

// useCaseStatus is the HTTP status of a use case's refusal.
func useCaseStatus(err error) int {
	switch deployment.KindOf(err) {
	case deployment.KindInvalid:
		return http.StatusBadRequest
	case deployment.KindConflict:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// writeUseCaseErr answers a use case's refusal with the status its Kind
// maps to.
func writeUseCaseErr(w http.ResponseWriter, err error) {
	writeErr(w, useCaseStatus(err), err)
}

// afterAutomaticPick, when set, runs right after the server picked a node
// for a create and before the create re-checks it
// (deployment.Deps.AfterAutomaticPick).
var afterAutomaticPick func(nodeUUID string)

// createDeployment creates a deployment (deployment.Service.Create).
func (a *apiHandlers) createDeployment(w http.ResponseWriter, r *http.Request) {
	var req dto.DeployRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	created, err := a.deployments().Create(r.Context(), req)
	if err != nil {
		writeUseCaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.deploymentDTO(created, true))
}

// evaluate previews a create request for the deploy form
// (deployment.Service.Evaluate).
func (a *apiHandlers) evaluate(w http.ResponseWriter, r *http.Request) {
	var req dto.EvaluateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resp, err := a.deployments().Evaluate(r.Context(), req)
	if err != nil {
		writeUseCaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
