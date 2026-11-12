package api

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// storageModelFiles lists the model files a storage holds, for its detail
// page: the shared copy of each model first, then node rows (local copies or
// nfs_cache node caches) by model and node name.
func (a *apiHandlers) storageModelFiles(w http.ResponseWriter, r *http.Request) {
	row, err := a.d.Store.StorageByID(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	files, err := a.d.Store.ModelFilesInStorage(row.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	deployments, err := a.d.Store.DeploymentsReadingFiles(row.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	models := map[uint]*store.Model{}
	nodes := map[uint]*store.Node{}
	nodeOf := func(id uint) *store.Node {
		if id == 0 {
			return nil
		}
		if n, ok := nodes[id]; ok {
			return n
		}
		n, err := a.d.Store.NodeByID(id)
		if err != nil {
			n = nil
		}
		nodes[id] = n
		return n
	}
	out := make([]dto.StorageModelFile, 0, len(files))
	for _, mf := range files {
		model, ok := models[mf.ModelID]
		if !ok {
			if m, err := a.d.Store.GetModel(mf.ModelID); err == nil {
				model = m
			}
			models[mf.ModelID] = model
		}
		item := dto.StorageModelFile{
			ID: mf.ID, ModelID: mf.ModelID, Path: mf.Path, State: mf.State, Message: mf.Message,
			Acquisition: mf.Acquisition, SizeBytes: mf.SizeBytes, Downloaded: mf.Downloaded,
			Progress: deployment.TransferProgress(mf.Downloaded, mf.SizeBytes), LastUsedAt: mf.LastUsedAt,
			Cache:         mf.NodeID != 0 && row.Type == proto.StorageNFSCache,
			DeploymentIDs: append([]uint{}, deployments[mf.ID]...),
		}
		if model != nil {
			item.ModelName, item.Source, item.SourceID = model.Name, model.Source, model.SourceID
			if item.ModelName == "" {
				item.ModelName = model.SourceID
			}
		}
		if n := nodeOf(mf.NodeID); n != nil {
			item.NodeUUID, item.NodeName = n.UUID, nodeDisplayName(n)
		}
		if n := nodeOf(mf.WriterNodeID); n != nil {
			item.WriterNode = nodeDisplayName(n)
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].NodeUUID == "") != (out[j].NodeUUID == "") {
			return out[i].NodeUUID == ""
		}
		if out[i].ModelName != out[j].ModelName {
			return out[i].ModelName < out[j].ModelName
		}
		return out[i].NodeName < out[j].NodeName
	})
	writeJSON(w, http.StatusOK, dto.NewList(out))
}
