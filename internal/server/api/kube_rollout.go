package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/kubemanifest"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// kubeRolloutPollAfter is how long a client waits before asking again while
// the answer is not settled. Each read has the cloud issue a kubeconfig, so
// it is not asked for every few seconds.
const kubeRolloutPollAfter = 10

// kubeClusterRollout says, node by node, how the agents of an entry made
// from a cloud cluster stand, read from the cluster with the account's
// credentials. It only reads, and only when asked: the console asks while an
// apply's dialog or a stalled entry's troubleshooting is open.
func (a *apiHandlers) kubeClusterRollout(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	c, err := a.d.Store.KubeClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if c.ProviderID == nil {
		writeProductError(w, http.StatusConflict, ErrorKubeClusterNoSource, "the cluster was not connected from a cloud account; run the check command with kubectl", nil)
		return
	}
	state, err := a.d.Providers.ClusterRollout(r.Context(), *c.ProviderID, c.ProviderClusterID, c.Namespace)
	var opErr *provider.ClusterOperationError
	switch {
	case err == nil:
	case errors.As(err, &opErr):
		writeProductError(w, statusProviderFailed, ErrorKubeClusterOperationFailed, err.Error(),
			map[string]any{"step": opErr.Step, "reason": opErr.Err.Error()})
		return
	case errors.Is(err, store.ErrNotFound):
		writeProductError(w, http.StatusConflict, ErrorKubeClusterNoSource, "the cloud account the cluster was connected from no longer exists", nil)
		return
	case errors.Is(err, provider.ErrUnsupported):
		writeErr(w, http.StatusNotImplemented, err)
		return
	default:
		writeErr(w, statusProviderFailed, err)
		return
	}
	rows, done := kubemanifest.JudgeAgents(state)
	out := dto.KubeRollout{Done: done, Desired: state.Desired, Rows: make([]dto.KubeCheckRow, 0, len(rows))}
	if !done {
		out.PollAfterSeconds = kubeRolloutPollAfter
	}
	for _, row := range rows {
		out.Rows = append(out.Rows, dto.KubeCheckRow{Node: row.Node, Pod: row.Pod, Code: string(row.Code), Detail: row.Detail})
	}
	writeJSON(w, http.StatusOK, out)
}
