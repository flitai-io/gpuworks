package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Storage files (ADR 0053; contract in
// artifacts/work/2026-10-21-apps/storage-files-contract.md): the whole storage
// can be browsed and downloaded, only the regions proto.StorageRelWritable
// names can be changed. The server picks the node and checks everything it can
// before any byte moves; the agent resolves paths and touches the filesystem.

// storageFilesRPCTimeout bounds one metadata RPC. The agent runs it under its
// own storage watchdog, so this only catches a node that stops answering.
const storageFilesRPCTimeout = 30 * time.Second

// storageFilesTarget is a resolved storage-files request: the storage, the node
// that serves it, and whether that node can write the storage now.
type storageFilesTarget struct {
	storage  *store.Storage
	node     *store.Node
	writable bool
}

// storageFilesError is a refusal with its HTTP status.
type storageFilesError struct {
	status int
	err    error
}

func (e *storageFilesError) Error() string { return e.err.Error() }
func (e *storageFilesError) Unwrap() error { return e.err }

func storageFilesRefusal(status int, code, fallback string, params map[string]any) error {
	return &storageFilesError{status: status, err: productError(code, fallback, params)}
}

// writeStorageFilesErr answers err with its own status when it carries one.
func writeStorageFilesErr(w http.ResponseWriter, err error) {
	var refusal *storageFilesError
	if errors.As(err, &refusal) {
		writeErr(w, refusal.status, refusal.err)
		return
	}
	writeStoreErr(w, err)
}

// storageFilesNeed is what an operation needs from the node.
type storageFilesNeed struct {
	write  bool // the node must be able to write the storage
	stream bool // the data lane must be attached (upload, download)
}

// cleanStorageFilePath normalises a client path, or refuses it.
func cleanStorageFilePath(raw string) (string, error) {
	clean, err := proto.CleanStorageRel(raw)
	if err != nil {
		return "", storageFilesRefusal(http.StatusBadRequest, ErrorStorageFilePathInvalid, err.Error(), nil)
	}
	return clean, nil
}

// writableStorageFilePath is cleanStorageFilePath for a path an operation
// creates, moves or removes: it must lie inside a writable region.
func writableStorageFilePath(raw string) (string, error) {
	clean, err := cleanStorageFilePath(raw)
	if err != nil {
		return "", err
	}
	if !proto.StorageRelWritable(clean) {
		return "", storageFilesRefusal(http.StatusForbidden, ErrorStorageFileRegionReadonly,
			"only entries inside files/ and .gpuworks/apps/ can be changed", nil)
	}
	return clean, nil
}

// storageFilesTargetFor loads the storage named in the URL and the node that
// will serve the request: the named one, which must have the storage ready
// (and writable for a write), or for a shared storage the first fitting node
// whose data lane is attached. Every refusal here precedes reading a body.
func (a *apiHandlers) storageFilesTargetFor(r *http.Request, nodeUUID string, need storageFilesNeed) (*storageFilesTarget, error) {
	storage, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		return nil, err
	}
	if need.write {
		if err := a.d.Store.StorageAcceptsWrites(storage); err != nil {
			if errors.Is(err, store.ErrStorageDisabled) {
				return nil, storageFilesRefusal(http.StatusConflict, ErrorStorageDisabled, "storage "+storage.Name+" is read-only (disabled)", map[string]any{"storage": storage.Name})
			}
			if errors.Is(err, store.ErrStorageNotWritable) {
				return nil, storageFilesRefusal(http.StatusConflict, ErrorStorageNotWritable, "no node can write to storage "+storage.Name, map[string]any{"storage": storage.Name})
			}
			return nil, err
		}
	}
	if a.d.CallNode == nil || a.d.NodeStreams == nil {
		return nil, storageFilesRefusal(http.StatusServiceUnavailable, ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
	}
	var node *store.Node
	if uuid := strings.TrimSpace(nodeUUID); uuid != "" {
		node, err = a.namedStorageFilesNode(storage, uuid, need)
	} else if !proto.StorageShared(storage.Type) {
		return nil, storageFilesRefusal(http.StatusBadRequest, ErrorStorageFileNodeRequired, "a local storage is browsed on one node: name it with node_uuid", map[string]any{"storage": storage.Name})
	} else {
		node, err = a.pickStorageFilesNode(storage, need)
	}
	if err != nil {
		return nil, err
	}
	writable := false
	if storage.Enabled {
		if state, err := a.d.Store.StorageNodeStateFor(storage.ID, node.ID); err == nil {
			writable = state.State == proto.StorageStateReady && state.Writable
		}
	}
	return &storageFilesTarget{storage: storage, node: node, writable: writable}, nil
}

func (a *apiHandlers) namedStorageFilesNode(storage *store.Storage, uuid string, need storageFilesNeed) (*store.Node, error) {
	unavailable := func() error {
		return storageFilesRefusal(http.StatusConflict, ErrorStorageUnavailableOnNode, "storage "+storage.Name+" is not usable on node "+uuid,
			map[string]any{"storage": storage.Name, "node": uuid})
	}
	node, err := a.d.Store.NodeByUUID(uuid)
	if errors.Is(err, store.ErrNotFound) {
		return nil, unavailable()
	}
	if err != nil {
		return nil, err
	}
	if _, applies, err := a.d.Store.EffectiveStorageForNode(storage, node); err != nil {
		return nil, err
	} else if !applies {
		return nil, unavailable()
	}
	if a.d.Online != nil && !a.d.Online(node.UUID) {
		return nil, storageFilesRefusal(http.StatusConflict, ErrorNodeOffline, "node "+nodeDisplayName(node)+" is offline", map[string]any{"node": nodeDisplayName(node)})
	}
	state, err := a.d.Store.StorageNodeStateFor(storage.ID, node.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && state.State != proto.StorageStateReady) {
		return nil, unavailable()
	}
	if err != nil {
		return nil, err
	}
	if need.write && !state.Writable {
		return nil, storageFilesRefusal(http.StatusConflict, ErrorStorageNotWritable, "node "+nodeDisplayName(node)+" cannot write to storage "+storage.Name,
			map[string]any{"storage": storage.Name, "node": nodeDisplayName(node)})
	}
	if err := a.storageFilesNodeReady(node, need.stream); err != nil {
		return nil, err
	}
	return node, nil
}

// storageFilesNodeReady checks what the node itself must offer: an agent that
// serves storage files, and for a stream an attached data lane.
func (a *apiHandlers) storageFilesNodeReady(node *store.Node, stream bool) error {
	if !a.storageFilesCapable(node) {
		return storageFilesRefusal(http.StatusConflict, ErrorStorageFileAgentUnsupported, "the agent on "+nodeDisplayName(node)+" is too old for storage files; upgrade it",
			map[string]any{"node": nodeDisplayName(node)})
	}
	if stream && !a.d.NodeStreams.DataLaneAttached(node.UUID) {
		return storageFilesRefusal(http.StatusServiceUnavailable, ErrorStorageFileDataLaneUnavailable, "node "+nodeDisplayName(node)+" has no data connection for a transfer now; retry shortly",
			map[string]any{"node": nodeDisplayName(node)})
	}
	return nil
}

func (a *apiHandlers) storageFilesCapable(node *store.Node) bool {
	return a.d.NodeCaps != nil && slices.Contains(a.d.NodeCaps(node.UUID), proto.CapStorageFiles)
}

// pickStorageFilesNode picks a node for a shared storage: online, ready
// (writable for a write), an agent that serves storage files and an attached
// data lane, for every operation, so a metadata call never lands on a node
// that is half connected. A read prefers a node that could also write, so the
// listing's writable flags hold for the next action. When no node fits, the
// reason the candidates had is the answer.
func (a *apiHandlers) pickStorageFilesNode(storage *store.Storage, need storageFilesNeed) (*store.Node, error) {
	var rejected error
	accept := func(node *store.Node) bool {
		if err := a.storageFilesNodeReady(node, true); err != nil {
			if rejected == nil {
				rejected = err
			}
			return false
		}
		return true
	}
	if !need.write && storage.Enabled {
		if node, err := a.pickStorageNode(storage, true, accept); err == nil {
			return node, nil
		}
	}
	node, err := a.pickStorageNode(storage, need.write, accept)
	if err != nil && rejected != nil {
		return nil, rejected
	}
	if err != nil {
		var product interface{ Code() string }
		if errors.As(err, &product) {
			return nil, &storageFilesError{status: http.StatusConflict, err: err}
		}
	}
	return node, err
}

// storageFilesAgentErr maps an agent's refusal (an RPC error, or an
// upload/download answer's code) to a product error and its status. Anything
// that is not a proto.StorageFilesError is a failed operation.
func storageFilesAgentErr(target *storageFilesTarget, err error) error {
	refusal := &proto.StorageFilesError{Code: proto.StorageFilesErrFailed, Detail: err.Error()}
	errors.As(err, &refusal)
	detail := refusal.Detail
	switch refusal.Code {
	case proto.StorageFilesErrPathInvalid:
		return storageFilesRefusal(http.StatusBadRequest, ErrorStorageFilePathInvalid, detail, nil)
	case proto.StorageFilesErrRegionReadonly:
		return storageFilesRefusal(http.StatusForbidden, ErrorStorageFileRegionReadonly, detail, nil)
	case proto.StorageFilesErrNotFound:
		return storageFilesRefusal(http.StatusNotFound, ErrorStorageFileNotFound, detail, nil)
	case proto.StorageFilesErrExists:
		return storageFilesRefusal(http.StatusConflict, ErrorStorageFileExists, detail, nil)
	case proto.StorageFilesErrNotWritable:
		return storageFilesRefusal(http.StatusConflict, ErrorStorageNotWritable, detail, map[string]any{"storage": target.storage.Name})
	case proto.StorageFilesErrExtractFailed:
		return storageFilesRefusal(http.StatusUnprocessableEntity, ErrorStorageFileExtractFailed, detail, map[string]any{"detail": detail})
	default:
		return storageFilesRefusal(http.StatusBadGateway, ErrorStorageFileOperationFailed, "storage operation on "+nodeDisplayName(target.node)+" failed: "+detail,
			map[string]any{"node": nodeDisplayName(target.node), "detail": detail})
	}
}

func (a *apiHandlers) callStorageFiles(ctx context.Context, target *storageFilesTarget, method string, params any) (json.RawMessage, error) {
	callCtx, cancel := rpccontext.WithTimeout(ctx, storageFilesRPCTimeout)
	defer cancel()
	raw, err := a.d.CallNode(callCtx, target.node.UUID, method, params)
	if err != nil {
		return nil, storageFilesAgentErr(target, err)
	}
	return raw, nil
}

// listStorageFiles lists one directory, one page at a time.
func (a *apiHandlers) listStorageFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rel, err := cleanStorageFilePath(q.Get("path"))
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 0 {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "limit must be a non-negative integer", map[string]any{"field": "limit", "detail": "limit must be a non-negative integer"})
			return
		}
	}
	target, err := a.storageFilesTargetFor(r, q.Get("node_uuid"), storageFilesNeed{})
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	raw, err := a.callStorageFiles(r.Context(), target, proto.RPCStorageFilesList, proto.StorageFilesListParams{
		StorageID: target.storage.ID, Path: rel, Cursor: q.Get("cursor"), Limit: limit,
	})
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	var page proto.StorageFilesListResult
	if err := json.Unmarshal(raw, &page); err != nil {
		writeStorageFilesErr(w, storageFilesAgentErr(target, fmt.Errorf("bad list answer: %w", err)))
		return
	}
	writeJSON(w, http.StatusOK, storageFileListDTO(target, rel, page))
}

func storageFileListDTO(target *storageFilesTarget, rel string, page proto.StorageFilesListResult) dto.StorageFileList {
	out := dto.StorageFileList{
		Path: rel, NodeUUID: target.node.UUID, NextCursor: page.NextCursor,
		// A directory takes new entries when something below it is writable.
		Writable: target.writable && proto.StorageRelWritable(path.Join(rel, "x")),
		Items:    make([]dto.StorageFileItem, 0, len(page.Entries)),
	}
	for _, entry := range page.Entries {
		itemPath := path.Join(rel, entry.Name)
		out.Items = append(out.Items, dto.StorageFileItem{
			Name: entry.Name, Path: itemPath, Dir: entry.Dir, Symlink: entry.Symlink, Size: entry.Size,
			ModTime: entry.ModTime.UTC(), Writable: target.writable && proto.StorageRelWritable(itemPath),
		})
	}
	return out
}

func (a *apiHandlers) mkdirStorageFile(w http.ResponseWriter, r *http.Request) {
	var req dto.StorageFileMkdirRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	rel, err := writableStorageFilePath(req.Path)
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	target, err := a.storageFilesTargetFor(r, req.NodeUUID, storageFilesNeed{write: true})
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	if _, err := a.callStorageFiles(r.Context(), target, proto.RPCStorageFilesMkdir, proto.StorageFilesMkdirParams{StorageID: target.storage.ID, Path: rel}); err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.StorageFilePath{Path: rel})
}

func (a *apiHandlers) renameStorageFile(w http.ResponseWriter, r *http.Request) {
	var req dto.StorageFileRenameRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	from, err := writableStorageFilePath(req.From)
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	to, err := writableStorageFilePath(req.To)
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	if from == to || strings.HasPrefix(to, from+"/") {
		writeStorageFilesErr(w, storageFilesRefusal(http.StatusBadRequest, ErrorStorageFilePathInvalid, "an entry cannot be moved onto itself or into itself", nil))
		return
	}
	target, err := a.storageFilesTargetFor(r, req.NodeUUID, storageFilesNeed{write: true})
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	if _, err := a.callStorageFiles(r.Context(), target, proto.RPCStorageFilesRename, proto.StorageFilesRenameParams{StorageID: target.storage.ID, From: from, To: to}); err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.StorageFilePath{Path: to})
}

func (a *apiHandlers) deleteStorageFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rel, err := writableStorageFilePath(q.Get("path"))
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	target, err := a.storageFilesTargetFor(r, q.Get("node_uuid"), storageFilesNeed{write: true})
	if err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	if _, err := a.callStorageFiles(r.Context(), target, proto.RPCStorageFilesDelete, proto.StorageFilesDeleteParams{StorageID: target.storage.ID, Path: rel}); err != nil {
		writeStorageFilesErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
