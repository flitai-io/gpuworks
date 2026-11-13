package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Scanning a shared storage is the one way a model library that already sits
// on it becomes known: one node walks the tree (storage.scan), the user picks
// directories, and importing writes only this control plane's database.

const scanPageTimeout = 50 * time.Second

func (a *apiHandlers) scanStorage(w http.ResponseWriter, r *http.Request) {
	row, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !proto.StorageShared(row.Type) {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "only a shared storage can be scanned; register a local directory instead", nil)
		return
	}
	var req struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
	node, err := a.storageReadNode(row)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	ctx, cancel := rpccontext.WithTimeout(r.Context(), scanPageTimeout+10*time.Second)
	defer cancel()
	raw, err := a.d.CallNode(ctx, node.UUID, proto.RPCStorageScan, proto.StorageScanParams{
		StorageID: row.ID, Cursor: req.Cursor, Limit: req.Limit, TimeoutSeconds: int(scanPageTimeout.Seconds()),
	})
	if err != nil {
		writeErr(w, http.StatusConflict, wrapProductError(ErrorStorageScanFailed, "scan on "+nodeDisplayName(node)+" failed: "+err.Error(), map[string]any{"node": node.UUID}, err))
		return
	}
	var page proto.StorageScanResult
	if err := json.Unmarshal(raw, &page); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	seen := scanSighting(req.Cursor, page)
	seen.Gone = a.goneFromStorage(r.Context(), row, node)
	if _, err := a.d.Store.ApplyStorageScan(row.ID, seen); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.annotateScan(row, node, page))
}

// scanSighting is what a scan page tells about the directories this control
// plane already records, so a rescan notices files deleted (or restored) by
// someone else.
func scanSighting(cursor string, page proto.StorageScanResult) store.ScanSighting {
	seen := store.ScanSighting{
		Covers: func(rel string) bool { return proto.ScanCovers(cursor, page, rel) },
		Found:  map[string]int64{}, Unsure: map[string]bool{},
	}
	for _, c := range page.Candidates {
		seen.Found[c.RelPath] = c.SizeBytes
	}
	for _, s := range page.Skipped {
		if proto.ScanSkipUndecided(s.Reason) {
			seen.Unsure[s.RelPath] = true
		}
	}
	return seen
}

// goneFromStorage confirms on the scanning node that a directory the page did
// not list is really absent. Anything but a definite "no such file" keeps the
// model file as it is: a slow or failing stat is not evidence of deletion.
func (a *apiHandlers) goneFromStorage(ctx context.Context, row *store.Storage, node *store.Node) func(string) bool {
	state, err := a.d.Store.StorageNodeStateFor(row.ID, node.ID)
	if err != nil {
		return nil
	}
	return func(rel string) bool {
		host, ok := store.SharedHostPath(state.Path, rel)
		if !ok {
			return false
		}
		statCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := a.d.CallNode(statCtx, node.UUID, proto.RPCFSStat, proto.FSStatParams{Path: host})
		return err != nil && strings.Contains(err.Error(), "no such file or directory")
	}
}

// storageReadNode is an online node on which the storage is ready.
func (a *apiHandlers) storageReadNode(row *store.Storage) (*store.Node, error) {
	if a.d.CallNode == nil {
		return nil, productError(ErrorNodeRPCUnavailable, "node RPC is not configured", nil)
	}
	return a.pickStorageNode(row, false, nil)
}

// annotateScan marks what this control plane already has: a directory the
// storage already records, and the existing model a new import would bind to.
func (a *apiHandlers) annotateScan(row *store.Storage, node *store.Node, page proto.StorageScanResult) dto.StorageScanResult {
	out := dto.StorageScanResult{NodeUUID: node.UUID, NextCursor: page.NextCursor, Complete: page.Complete,
		Candidates: []dto.StorageScanCandidate{}, Skipped: []dto.StorageScanSkipped{}}
	imported := map[string]uint{}
	holders := map[uint]*store.Model{}
	if files, err := a.d.Store.ModelFilesInStorage(row.ID); err == nil {
		for _, file := range files {
			if file.NodeID == 0 {
				imported[file.Path] = file.ModelID
			}
		}
	}
	for _, c := range page.Candidates {
		item := dto.StorageScanCandidate{RelPath: c.RelPath, SizeBytes: c.SizeBytes, Files: c.Files, Source: c.Source,
			SourceID: c.SourceID, Revision: c.Revision, Subfolder: c.Subfolder, Format: c.Format, Tasks: c.Tasks, MetadataIssues: c.MetadataIssues, Basis: c.Basis, Incomplete: c.Incomplete,
			IncompleteReason: c.IncompleteReason}
		// A recorded directory stays bound to the model holding it, even after
		// that model's source id was renamed (see importOne).
		var model *store.Model
		if holderID, ok := imported[c.RelPath]; ok {
			item.Imported = true
			if model, ok = holders[holderID]; !ok {
				model, _ = a.d.Store.GetModel(holderID)
				holders[holderID] = model
			}
		}
		if model == nil {
			model = a.bindingModel(c.Source, c.SourceID, c.Revision, c.Subfolder)
		}
		if model != nil {
			item.BindModelID, item.BindModelName = model.ID, model.Name
		}
		out.Candidates = append(out.Candidates, item)
	}
	for _, s := range page.Skipped {
		out.Skipped = append(out.Skipped, dto.StorageScanSkipped{RelPath: s.RelPath, Reason: s.Reason})
	}
	return out
}

// bindingModel is the existing model a scanned directory belongs to: the same
// identity when known, otherwise the oldest model registered under the same
// repository id, which is how inference templates match weights.
func (a *apiHandlers) bindingModel(source, sourceID, revision, subfolder string) *store.Model {
	if strings.TrimSpace(sourceID) == "" {
		return nil
	}
	if source != "" {
		if model, err := a.d.Store.FindModelArtifact(sourceID, source, revision, subfolder); err == nil {
			return model
		}
		if revision == "" {
			if model, err := a.d.Store.FindModelArtifactAnyRevision(sourceID, source, subfolder, 0); err == nil {
				return model
			}
		}
		return nil
	}
	if model, err := a.d.Store.FindModelBySourceID(sourceID, source); err == nil && model.ArtifactSubfolder == subfolder {
		return model
	}
	return nil
}

func (a *apiHandlers) importStorageModels(w http.ResponseWriter, r *http.Request) {
	row, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !proto.StorageShared(row.Type) {
		writeProductError(w, http.StatusBadRequest, ErrorStorageTypeInvalid, "only a shared storage can import scanned directories", nil)
		return
	}
	var req dto.StorageImportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Items) == 0 || len(req.Items) > 2000 {
		writeProductError(w, http.StatusBadRequest, ErrorStorageImportCountInvalid, "import 1-2000 directories at a time", map[string]any{"max": 2000})
		return
	}
	node, err := a.storageReadNode(row)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	state, err := a.d.Store.StorageNodeStateFor(row.ID, node.ID)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	out := dto.StorageImportResult{Imported: []dto.StorageImported{}, Failed: []dto.StorageImported{}}
	for _, item := range req.Items {
		result, err := a.importOne(r.Context(), row, node, state.Path, item)
		if err != nil {
			result.Error = err.Error()
			var productErr *messages.ProductError
			if errors.As(err, &productErr) {
				result.ErrorDetail = deployment.ProductMessage(err)
			}
			out.Failed = append(out.Failed, result)
			continue
		}
		out.Imported = append(out.Imported, result)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) importOne(ctx context.Context, row *store.Storage, node *store.Node, mount string, item dto.StorageImportItem) (dto.StorageImported, error) {
	result := dto.StorageImported{RelPath: item.RelPath}
	host, ok := store.SharedHostPath(mount, item.RelPath)
	if !ok {
		return result, errors.New("invalid relative path")
	}
	reg, err := callNodeParseModelMetadata(ctx, a.d.CallNode, node.UUID, host, row.ID)
	if err != nil {
		return result, err
	}
	nestedEntry := false
	if err := validateModelMetadata(reg.Metadata); err != nil {
		return result, err
	}
	if reg.Metadata != nil {
		if len(reg.Metadata.Entries) == 0 {
			return result, errors.New("directory does not identify exactly one model entry")
		}
		// This directory is an entry inside a confirmed repository. It holds
		// the entry's files directly, whereas a remote subfolder artifact keeps
		// that subfolder beneath its root. Do not equate those two layouts.
		nestedEntry = reg.Metadata.Entries[0].Path != "."
		if nestedEntry {
			item.Source, item.SourceID, item.Revision, item.Subfolder = "local", item.RelPath, "local", ""
		}
	}
	dir, err := a.modelDirFromMetadata(ctx, node.UUID, host, reg.Metadata)
	if err != nil {
		return result, err
	}
	item.SizeBytes = reg.SizeBytes
	// The model already recorded at this path owns it, whatever its source id
	// says now: a local model's source id can be renamed after import.
	holder := a.modelRecordedAt(row.ID, item.RelPath)
	var model *store.Model
	if item.ModelID != 0 {
		m, err := a.d.Store.GetModel(item.ModelID)
		if err != nil {
			return result, err
		}
		if nestedEntry && (holder == nil || holder.ID != m.ID) && (m.Source != "local" || m.SourceID != item.RelPath || m.ArtifactSubfolder != "") {
			return result, errors.New("a standalone directory cannot bind a repository-subfolder artifact")
		}
		model = m
	} else if holder != nil {
		model = holder
	} else if m := a.bindingModel(item.Source, item.SourceID, item.Revision, item.Subfolder); m != nil {
		model = m
	} else {
		created, err := a.modelFromParsedDir(dir, item)
		if err != nil {
			return result, err
		}
		model, result.Created = created, true
	}
	ref := store.FileRef{ModelID: model.ID, StorageID: row.ID}
	if current, err := a.d.Store.ModelFileAt(ref); err == nil && current.State == proto.MFPresent && current.Path != item.RelPath {
		return result, errors.New("this model is already imported from " + current.Path)
	}
	size := max(item.SizeBytes, 0)
	if _, err := a.d.Store.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, StorageID: row.ID, NodeID: 0, Path: item.RelPath, State: proto.MFPresent,
		Acquisition: proto.ModelAcquireImported, SizeBytes: size, Downloaded: size, Revision: item.Revision,
	}); err != nil {
		return result, deployment.ModelFilePathProductError(err)
	}
	if model.WeightBytes <= 0 && size > 0 {
		_ = a.d.Store.SetModelWeightBytes(model.ID, size)
	}
	result.ModelID = model.ID
	return result, nil
}

// modelRecordedAt is the model whose shared-storage row sits at relPath, or
// nil when the path is not recorded.
func (a *apiHandlers) modelRecordedAt(storageID uint, relPath string) *store.Model {
	file, err := a.d.Store.SharedModelFileAt(storageID, relPath)
	if err != nil {
		return nil
	}
	model, err := a.d.Store.GetModel(file.ModelID)
	if err != nil {
		return nil
	}
	return model
}

func (a *apiHandlers) modelFromParsedDir(dir modelDir, item dto.StorageImportItem) (*store.Model, error) {
	source := strings.ToLower(strings.TrimSpace(item.Source))
	if source == "" {
		source = "local"
	}
	revision := strings.TrimSpace(item.Revision)
	if source == "local" {
		revision = "local"
	}
	sourceID := strings.TrimSpace(item.SourceID)
	if sourceID == "" {
		sourceID = item.RelPath
	}
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = sourceID
	}
	model := dir.model(source, sourceID, revision, name, max(item.SizeBytes, 0))
	model.ArtifactSubfolder = item.Subfolder
	return a.d.Store.UpsertModel(model)
}
