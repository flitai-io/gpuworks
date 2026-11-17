package api

import (
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

// Storage-files transfers: one PUT streams a body to the agent, one GET
// streams a file or a packed directory back (ADR 0053). Neither is buffered
// here; both need the node's data lane attached now.

// uploadStorageFile streams the request body onto the storage. Every check
// that can fail without the agent runs before the body is read, so a browser
// gets a JSON error instead of a reset upload.
func (a *apiHandlers) uploadStorageFile(w http.ResponseWriter, r *http.Request) {
	storageTransferRequestID(w, r)
	q := r.URL.Query()
	rel, err := writableStorageFilePath(q.Get("path"))
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	extract := strings.TrimSpace(q.Get("extract"))
	if extract != "" && !proto.StorageArchiveFormatValid(extract) {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "extract must be tar, tar.gz or zip", map[string]any{"field": "extract", "detail": "extract must be tar, tar.gz or zip"})
		return
	}
	overwrite := false
	if raw := strings.TrimSpace(q.Get("overwrite")); raw != "" {
		overwrite, err = strconv.ParseBool(raw)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "overwrite must be true or false", map[string]any{"field": "overwrite", "detail": "overwrite must be true or false"})
			return
		}
	}
	target, err := a.storageFilesTargetFor(r, q.Get("node_uuid"), storageFilesNeed{write: true, stream: true})
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	size := int64(-1)
	if r.ContentLength >= 0 {
		size = r.ContentLength
	}
	hdr := proto.StreamHeader{
		StorageID: target.storage.ID, Rel: rel, Size: size, Extract: extract, Overwrite: overwrite,
		Staging: proto.StorageUploadStagingRel(a.d.Store.ControlPlaneIdentity().ID, auth.NewUUID()),
	}
	status, err := a.d.NodeStreams.UploadStorageFile(r.Context(), target.node.UUID, hdr, r.Body)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client is gone; nobody reads an answer
		}
		writeStorageFilesErr(w, a.storageFilesTransferErr(target, err))
		return
	}
	if status.State != proto.StorageFilesDone {
		code := status.Code
		if code == "" {
			code = proto.StorageFilesErrFailed
		}
		writeStorageFilesErr(w, storageFilesAgentErr(target, proto.NewStorageFilesError(code, "%s", status.Error)))
		return
	}
	writeJSON(w, http.StatusOK, dto.StorageFileUploadResult{Path: rel, Bytes: status.Bytes, Files: status.Files})
}

// storageFilesTransferErr maps a failed stream: a lane that is gone is the
// retryable condition the precheck reports; anything else broke mid-way.
func (a *apiHandlers) storageFilesTransferErr(target *storageFilesTarget, err error) error {
	if !a.d.NodeStreams.DataLaneAttached(target.node.UUID) {
		return storageFilesRefusal(http.StatusServiceUnavailable, ErrorStorageFileDataLaneUnavailable,
			"the data connection to "+nodeDisplayName(target.node)+" dropped during the transfer: "+err.Error(), map[string]any{"node": nodeDisplayName(target.node)})
	}
	return storageFilesRefusal(http.StatusBadGateway, ErrorStorageFileOperationFailed, "transfer with "+nodeDisplayName(target.node)+" failed: "+err.Error(),
		map[string]any{"node": nodeDisplayName(target.node)})
}

// downloadStorageFile streams a file as is, or a directory packed as zip or
// tar.gz. A refusal in the agent's head becomes a JSON error; once the bytes
// flow, a failure can only cut the connection, which the browser shows as a
// failed download.
func (a *apiHandlers) downloadStorageFile(w http.ResponseWriter, r *http.Request) {
	requestID := storageTransferRequestID(w, r)
	q := r.URL.Query()
	rel, err := cleanStorageFilePath(q.Get("path"))
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	format := strings.TrimSpace(q.Get("format"))
	if format == "" {
		format = proto.StorageArchiveZip
	}
	if format != proto.StorageArchiveZip && format != proto.StorageArchiveTarGz {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "format must be zip or tar.gz", map[string]any{"field": "format", "detail": "format must be zip or tar.gz"})
		return
	}
	target, err := a.storageFilesTargetFor(r, q.Get("node_uuid"), storageFilesNeed{stream: true})
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	head, body, err := a.d.NodeStreams.OpenStorageDownload(r.Context(), target.node.UUID, proto.StreamHeader{StorageID: target.storage.ID, Rel: rel, Format: format})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeStorageFilesErr(w, a.storageFilesTransferErr(target, err))
		return
	}
	if body == nil {
		writeStorageFilesErr(w, storageFilesAgentErr(target, proto.NewStorageFilesError(head.Code, "%s", head.Error)))
		return
	}
	defer body.Close()
	name := strings.TrimSpace(head.Name)
	if name == "" || strings.ContainsAny(name, "/\\") {
		// The agent names the root "storage"; without a name, the path's
		// last element, or the storage's own name for the root.
		name = path.Base(rel)
		if rel == "" {
			name = target.storage.Name
		}
	}
	h := w.Header()
	switch {
	case !head.Dir:
		h.Set("Content-Type", "application/octet-stream")
		if head.Size >= 0 {
			h.Set("Content-Length", strconv.FormatInt(head.Size, 10))
		}
	case format == proto.StorageArchiveTarGz:
		name += ".tar.gz"
		h.Set("Content-Type", "application/gzip")
	default:
		name += ".zip"
		h.Set("Content-Type", "application/zip")
	}
	h.Set("Content-Disposition", storageFileDisposition(name))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// The browser starts its download now, not after the first buffer of a
	// directory archive that may take a while to fill.
	_ = http.NewResponseController(w).Flush()
	expected := head.Size
	if head.Dir {
		expected = -1
	}
	n, stage, direction, err := copyStorageDownload(w, body, expected)
	if err != nil {
		// The status line is out; only a cut connection tells the client
		// the download is incomplete.
		if a.d.Log != nil {
			a.d.Log.Warn("storage file download cut short", "request_id", requestID, "observation_scope", "gateway",
				"storage", target.storage.Name, "storage_id", target.storage.ID, "node", target.node.UUID, "path", rel,
				"stage", stage, "direction", direction, "bytes", n, "expected_bytes", expected,
				"data_lane_attached", a.d.NodeStreams.DataLaneAttached(target.node.UUID),
				"err", storageTransferErrorDetail(err, target.node.Token))
		}
		panic(http.ErrAbortHandler)
	}
}

// storageFileDisposition names the download; mime encodes a non-ASCII name
// as RFC 2231 filename*.
func storageFileDisposition(name string) string {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment"
}
