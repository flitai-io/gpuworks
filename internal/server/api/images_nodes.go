package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Per-node image actions: fetch, peer sync, retry, cancel, remove, inspect.

func (a *apiHandlers) syncImageToNode(w http.ResponseWriter, r *http.Request) {
	a.fetchImageForNode(w, r, decodeImageNodeSyncRequest(r))
}

func (a *apiHandlers) probeImageToNode(w http.ResponseWriter, r *http.Request) {
	if a.d.PeerSync.ProbeImage == nil && a.d.PeerSync.ProbeImageResult == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorImageSyncUnavailable, "image peer probe is not configured", nil)
		return
	}
	req := decodeImageNodeSyncRequest(r)
	if req.SourceNode == "" {
		writeProductError(w, http.StatusBadRequest, ErrorImageSyncUnavailable, "source_node is required for image peer probe", nil)
		return
	}
	if probe := a.d.PeerSync.ProbeImageResult; probe != nil {
		result, err := probe(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"), req.SourceNode)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		response := map[string]string{"state": "ready"}
		if result.Transport == "direct" || result.Transport == "relay" {
			response["transport"] = result.Transport
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	if err := a.d.PeerSync.ProbeImage(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"), req.SourceNode); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "ready"})
}

func (a *apiHandlers) retryImageToNode(w http.ResponseWriter, r *http.Request) {
	req := decodeImageNodeSyncRequest(r)
	// A retry of a peer transfer keeps the recorded source node and creates a
	// new transfer. It must not start a fresh sync through the generic fetch
	// path, which would lose the retry semantics and source validation.
	if req.SourceNode != "" {
		if a.d.PeerSync.SyncImageWithRate == nil {
			writeProductError(w, http.StatusServiceUnavailable, ErrorImageRetryUnavailable, "direct image retry is not configured", nil)
			return
		}
		if req.RateLimitKiB < 0 {
			writeProductError(w, http.StatusBadRequest, ErrorImageSyncUnavailable, "rate_limit_kib_s must be non-negative", nil)
			return
		}
		row, err := a.d.PeerSync.SyncImageWithRate(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"), req.SourceNode, req.RateLimitKiB)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"state": row.State, "transfer_id": row.TransferID, "message": row.Message})
		return
	}
	// A retry with nothing named retries the last copy from a node when that
	// is what failed, and pulls again otherwise.
	if req.Source == "" && req.ArchiveURL == "" && a.d.PeerSync.RetryImage != nil {
		if row, err := a.d.Store.ImageNode(parseUint(chi.URLParam(r, "id")), a.nodeIDOf(chi.URLParam(r, "uuid"))); err == nil && row.Source == "peer" && row.SourceNodeID != 0 {
			retried, err := a.d.PeerSync.RetryImage(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"))
			if err != nil {
				writeErr(w, http.StatusConflict, err)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"state": retried.State, "transfer_id": retried.TransferID, "message": retried.Message})
			return
		}
	}
	req.Force = req.Force || req.Source == ""
	a.fetchImageForNode(w, r, req)
}

// nodeIDOf is the id of the node a route names, or 0.
func (a *apiHandlers) nodeIDOf(uuid string) uint {
	node, err := a.d.Store.NodeByUUID(uuid)
	if err != nil {
		return 0
	}
	return node.ID
}

// cancelImageToNode stops a transfer that is still running on a node. A row
// the node already finished with comes back unchanged: cancelling ends work,
// it does not rewrite a result.
func (a *apiHandlers) cancelImageToNode(w http.ResponseWriter, r *http.Request) {
	if a.d.ImageFetch.Cancel == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorImageSyncUnavailable, "image cancellation is not configured", nil)
		return
	}
	row, err := a.d.ImageFetch.Cancel(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"state": row.State, "transfer_id": row.TransferID, "message": row.Message})
}

// removeImageFromNode deletes an image from one node. The node decides whether
// it can: a container still running from it makes the runtime refuse, and that
// refusal is passed back rather than worked around.
func (a *apiHandlers) removeImageFromNode(w http.ResponseWriter, r *http.Request) {
	if a.d.ImageFetch.Remove == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorImageSyncUnavailable, "image removal is not configured", nil)
		return
	}
	row, err := a.d.ImageFetch.Remove(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"state": row.State, "message": row.Message})
}

func (a *apiHandlers) inspectImageOnNode(w http.ResponseWriter, r *http.Request) {
	if a.d.InspectImage == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorImageInspectionUnavailable, "image inspection is not configured", nil)
		return
	}
	result, err := a.d.InspectImage(r.Context(), parseUint(chi.URLParam(r, "id")), chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
