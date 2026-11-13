package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

// PrepareSharedModelDownload claims the observed shared location and assigns
// transfer, writer and owner atomically. The observation precedes the old
// writer's stop acknowledgement; a concurrent start must not replace its winner.
func (s *Store) PrepareSharedModelDownload(ref FileRef, path string, writerID, deploymentID uint, observed *ModelFile) (*ModelFile, error) {
	ref = s.resolveRef(ref)
	var row ModelFile
	err := s.db.Transaction(func(tx *gorm.DB) error {
		err := refQuery(tx, ref).First(&row).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			if observed != nil {
				return ErrModelDownloadInactive
			}
		case err != nil:
			return err
		default:
			if row.State == proto.MFDeleting {
				return ErrCacheDeletionPending
			}
			if observed == nil || row.ID != observed.ID || row.State != observed.State || row.TransferID != observed.TransferID || row.WriterNodeID != observed.WriterNodeID || row.DownloadDeploymentID != observed.DownloadDeploymentID {
				return ErrModelDownloadInactive
			}
			if row.Ready() || TransferUnderWay(&row) {
				return ErrModelDownloadInactive
			}
		}
		if err := sharedModelPathConflict(tx, ref, path); err != nil {
			return err
		}
		if row.ID == 0 {
			row = ModelFile{ModelID: ref.ModelID, StorageID: ref.StorageID, NodeID: ref.NodeID, Path: path}
			if err := tx.Create(&row).Error; err != nil {
				return modelFilePathWriteError(tx, ref, path, err)
			}
		}
		updates := transferReset(path, proto.MFPending, proto.ModelAcquireDownload, map[string]any{"revision": "", "transfer_id": auth.NewUUID(), "writer_node_id": writerID, "download_deployment_id": deploymentID})
		if err := tx.Model(&ModelFile{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			return modelFilePathWriteError(tx, ref, path, err)
		}
		return tx.First(&row, row.ID).Error
	})
	if err != nil {
		return nil, err
	}
	s.publishModelFile(row.ID, row.State)
	return &row, nil
}

// EventDeploymentComponentDownload: a deployment's create started the one
// write of a template component into a shared storage (ADR 0062). Params:
// component, model, bytes, storage, node (the writer), follows_primary (the
// request named no storage and the storage is the one the primary sits on).
const EventDeploymentComponentDownload = "DEPLOYMENT_COMPONENT_DOWNLOAD"
