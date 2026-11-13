package deployment

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// StartSharedDownloadOn starts the one download of m into a shared storage
// through a writer node: writerUUID, or the one sharedWriter picks when it is
// empty. owner is the deployment the download is for, none for an operator's.
// It answers the writer, nil when the storage already holds the model.
func (s *Service) StartSharedDownloadOn(m *store.Model, storage *store.Storage, writerUUID string, owner ...uint) (*store.Node, error) {
	deploymentID := uint(0)
	if len(owner) > 0 {
		deploymentID = owner[0]
	}
	ref := store.FileRef{ModelID: m.ID, StorageID: storage.ID}
	current, readErr := s.d.Store.ModelFileAt(ref)
	if readErr != nil && !errors.Is(readErr, store.ErrNotFound) {
		return nil, readErr
	}
	if readErr == nil {
		if current.Ready() {
			return nil, nil
		}
		if current.State == proto.MFCanceled && current.WriterNodeID != 0 {
			oldWriter, err := s.d.Store.NodeByID(current.WriterNodeID)
			if err != nil {
				return nil, conflict(err)
			}
			if s.d.CallNode == nil {
				return nil, conflict(errors.New("the previous download writer must acknowledge cancellation before retry"))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err = s.d.CallNode(ctx, oldWriter.UUID, proto.RPCModelDownloadStop, proto.ModelCopyStopParams{Key: store.ModelFileKey(current.ID), TransferID: current.TransferID})
			cancel()
			if err != nil {
				return nil, conflict(err)
			}
		}
		if store.TransferUnderWay(current) {
			writer, err := s.d.Store.NodeByID(current.WriterNodeID)
			if err == nil {
				if writerUUID != "" && writer.UUID != writerUUID {
					return nil, conflict(productError(messages.ErrorFieldInvalid, "cancel the running transfer before choosing another writer", map[string]any{"field": "node_uuids", "detail": "cancel the running transfer before choosing another writer"}))
				}
				if deploymentID == 0 {
					if err := s.d.Store.SetDownloadDeployment(current.ID, current.TransferID, 0); err != nil {
						return nil, err
					}
				}
				return writer, nil
			}
		}
	}
	var writer *store.Node
	var err error
	if writerUUID == "" {
		writer, err = s.sharedWriter(storage, m)
	} else {
		writer, err = s.selectedSharedWriter(storage, m, writerUUID)
	}
	if err != nil {
		return nil, conflict(err)
	}
	if _, err := planning.ModelWeightsFetch(s.d.Store, s.networks().Resolve, writer, m); err != nil {
		return nil, conflict(err)
	}
	_, err = s.d.Store.PrepareSharedModelDownload(ref, store.SharedModelRelPath(m), writer.ID, deploymentID, current)
	if err != nil {
		if errors.Is(err, store.ErrModelDownloadInactive) {
			return nil, conflict(productError(messages.ErrorFieldInvalid, "the download changed; refresh before retrying", map[string]any{"field": "node_uuids", "detail": "the download changed; refresh before retrying"}))
		}
		var occupied *store.ModelFilePathOccupiedError
		if errors.As(err, &occupied) {
			return nil, conflict(ModelFilePathProductError(err))
		}
		return nil, err
	}
	if s.d.NodeSync != nil {
		_ = s.d.NodeSync.SyncNode(writer.UUID)
	}
	return writer, nil
}

// sharedWriter picks the online node that downloads into a shared storage.
func (s *Service) sharedWriter(storage *store.Storage, m *store.Model) (*store.Node, error) {
	return planning.PickStorageNode(s.d.Store, s.d.Online, storage, true, func(node *store.Node) bool { return AdmitModelDownloadNode(m, node) == nil })
}
