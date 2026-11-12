package api

import (
	"net/http"
	"net/url"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

type peerTransferSnapshots interface {
	PeerTransferSnapshotDTO() dto.PeerTransferSnapshot
}

type peerTransferSnapshotFor interface {
	PeerTransferSnapshotForDTO(string) dto.PeerTransferSnapshot
}

func (a *apiHandlers) peerTransferSnapshot(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 1 || (len(query) == 1 && !query.Has("transfer_id")) {
		writeProductError(w, http.StatusBadRequest, ErrorPeerTransferQueryInvalid, "use one non-empty transfer_id of at most 128 characters", nil)
		return
	}
	id := ""
	if values, present := query["transfer_id"]; present {
		if len(values) != 1 || values[0] == "" || utf8.RuneCountInString(values[0]) > 128 {
			writeProductError(w, http.StatusBadRequest, ErrorPeerTransferQueryInvalid, "use one non-empty transfer_id of at most 128 characters", nil)
			return
		}
		id = values[0]
	}
	source, ok := a.d.NodeStreams.(peerTransferSnapshots)
	if !ok {
		writeProductError(w, http.StatusServiceUnavailable, ErrorPeerTransferObservationUnavailable, "peer transfer observations are unavailable", nil)
		return
	}
	var snapshot dto.PeerTransferSnapshot
	if filtered, ok := source.(peerTransferSnapshotFor); id != "" && ok {
		snapshot = filtered.PeerTransferSnapshotForDTO(id)
	} else {
		snapshot = source.PeerTransferSnapshotDTO()
	}
	if id != "" {
		selected := make([]dto.PeerTransferObservation, 0, 1)
		for _, item := range snapshot.Active {
			if item.TransferID == id {
				selected = append(selected, item)
			}
		}
		snapshot.Active = selected
	}
	writeJSON(w, http.StatusOK, snapshot)
}
