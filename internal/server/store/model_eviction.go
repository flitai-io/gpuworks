package store

import (
	"errors"
	"fmt"
	"slices"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

type ModelFileEviction struct {
	LogicalBytes  int64 `json:"logical_bytes"`
	PhysicalBytes int64 `json:"physical_bytes"`
}

// ModelFileReferencedError identifies every active deployment that prevents a
// node-local model cache eviction. Is keeps errors.Is(err, ErrReferenced)
// compatible with older callers while allowing APIs to expose stable IDs.
type ModelFileReferencedError struct {
	DeploymentIDs []uint
}

func (e *ModelFileReferencedError) Error() string {
	return fmt.Sprintf("model file is referenced by active deployments %v", e.DeploymentIDs)
}

func (e *ModelFileReferencedError) Is(target error) bool {
	return target == ErrReferenced
}

func (s *Store) CheckModelFileEviction(ref FileRef) error {
	return checkModelFileEviction(s.db, s.resolveRef(ref))
}

// EvictModelFile removes one node-local model cache record after verifying no
// active deployment on that node still references it. The caller is expected
// to remove the corresponding filesystem path before invoking this method.
func (s *Store) EvictModelFile(ref FileRef) (ModelFileEviction, error) {
	ref = s.resolveRef(ref)
	var out ModelFileEviction
	var deletedID uint
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := checkModelFileEviction(tx, ref); err != nil {
			return err
		}
		var mf ModelFile
		err := refQuery(tx, ref).First(&mf).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		out.LogicalBytes = mf.SizeBytes
		out.PhysicalBytes = mf.Downloaded
		deletedID = mf.ID
		return tx.Delete(&mf).Error
	})
	if err == nil && deletedID != 0 {
		s.publishModelFile(deletedID, "deleted")
	}
	return out, err
}

// checkModelFileEviction refuses removing a row a deployment uses.
func checkModelFileEviction(db *gorm.DB, ref FileRef) error {
	var row ModelFile
	if err := refQuery(db, ref).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return referencedBy(db, []ModelFile{row})
}

// CheckModelFileRowEviction is CheckModelFileEviction for a row by id, for
// the cache eviction that picks rows rather than references.
func (s *Store) CheckModelFileRowEviction(id uint) error {
	var row ModelFile
	if err := s.db.First(&row, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return referencedBy(s.db, []ModelFile{row})
}

// CheckModelFilesInStorage refuses removing a model from a storage while any
// of its rows there is used: the storage's own copy and, for an nfs_cache
// storage, every node's cache of it.
func (s *Store) CheckModelFilesInStorage(modelID, storageID uint) error {
	var rows []ModelFile
	if err := s.db.Where("model_id = ? AND storage_id = ?", modelID, storageID).Find(&rows).Error; err != nil {
		return err
	}
	return referencedBy(s.db, rows)
}

func referencedBy(db *gorm.DB, rows []ModelFile) error {
	for _, row := range rows {
		if row.State == proto.MFDeleting {
			return ErrCacheDeletionPending
		}
	}
	inUse, err := modelFilesInUse(db, rows)
	if err != nil {
		return err
	}
	var ids []uint
	for _, deps := range inUse {
		ids = append(ids, deps...)
	}
	if ids = sortedUnique(ids); len(ids) == 0 {
		return nil
	}
	return &ModelFileReferencedError{DeploymentIDs: ids}
}

// modelFilesInUse is the one answer to "which deployments use these model
// file rows", by row id. A row is used by the deployments with a live
// instance recorded as reading it; a node's own row (a local file, a cache
// entry) is also used by every active deployment of the model that is placed
// on that node or has a live instance there, since a node reads whichever copy
// it has and may be about to read one it just received.
func modelFilesInUse(db *gorm.DB, rows []ModelFile) (map[uint][]uint, error) {
	out := map[uint][]uint{}
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]uint, 0, len(rows))
	models := map[uint]bool{}
	for _, row := range rows {
		ids = append(ids, row.ID)
		if row.NodeID != 0 {
			models[row.ModelID] = true
		}
	}
	var reads []struct {
		ModelFileID  uint
		DeploymentID uint
	}
	if err := liveReads(db).Where("instance_model_files.model_file_id IN ?", ids).
		Distinct("instance_model_files.model_file_id", "instances.deployment_id").Scan(&reads).Error; err != nil {
		return nil, err
	}
	for _, read := range reads {
		out[read.ModelFileID] = append(out[read.ModelFileID], read.DeploymentID)
	}
	if len(models) > 0 {
		modelIDs := make([]uint, 0, len(models))
		for id := range models {
			modelIDs = append(modelIDs, id)
		}
		var deployments []Deployment
		if err := db.Select("id", "model_id", "node_id", "node_ids").
			Where("model_id IN ? AND desired_state = ?", modelIDs, DeployActive).Find(&deployments).Error; err != nil {
			return nil, err
		}
		depIDs := make([]uint, 0, len(deployments))
		for _, dep := range deployments {
			depIDs = append(depIDs, dep.ID)
		}
		type placed struct{ dep, node uint }
		running := map[placed]bool{}
		if len(depIDs) > 0 {
			var live []struct {
				DeploymentID uint
				NodeID       uint
			}
			if err := db.Model(&Instance{}).Distinct("deployment_id", "node_id").
				Where("deployment_id IN ? AND state NOT IN ?", depIDs, endedInstanceStates).Scan(&live).Error; err != nil {
				return nil, err
			}
			for _, l := range live {
				running[placed{l.DeploymentID, l.NodeID}] = true
			}
		}
		for _, row := range rows {
			if row.NodeID == 0 {
				continue
			}
			for _, dep := range deployments {
				if dep.ModelID == row.ModelID && (deploymentReferencesNode(dep, row.NodeID) || running[placed{dep.ID, row.NodeID}]) {
					out[row.ID] = append(out[row.ID], dep.ID)
				}
			}
		}
	}
	for id, deps := range out {
		out[id] = sortedUnique(deps)
	}
	return out, nil
}

// deploymentReferencesNode covers both legacy single-node deployments and
// distributed deployments whose participating nodes are stored in NodeIDs.
// Malformed NodeIDs fail closed: an active deployment with an invalid
// participant list must not allow a cache eviction that could race it.
func deploymentReferencesNode(dep Deployment, nodeID uint) bool {
	if dep.NodeID == nodeID {
		return true
	}
	ids, err := dep.StoredNodeIDs()
	if err != nil {
		return true
	}
	for _, id := range ids {
		if id == nodeID {
			return true
		}
	}
	return false
}

// sortedUnique is ids sorted, each once, in a new slice.
func sortedUnique(ids []uint) []uint {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
