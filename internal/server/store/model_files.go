package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// FileRef names one model-file row: a model in a storage on a node. NodeID 0
// is a shared storage's single row. StorageID 0 means the default local
// storage (DefaultLocalStorageID), which is what a reference meant before
// storages existed.
type FileRef struct {
	ModelID   uint
	StorageID uint
	NodeID    uint
}

// resolveRef fills a reference that names no storage with the default local
// storage. Production code names the storage; this serves only the model-file
// keys of agents from before storages (ParseModelFileKey's legacy form) and
// test fixtures written before storages existed.
func (s *Store) resolveRef(ref FileRef) FileRef {
	if ref.StorageID == 0 {
		ref.StorageID = s.DefaultLocalStorageID()
	}
	return ref
}

func refQuery(db *gorm.DB, ref FileRef) *gorm.DB {
	return db.Where("model_id = ? AND storage_id = ? AND node_id = ?", ref.ModelID, ref.StorageID, ref.NodeID)
}

func (s *Store) publishModelFile(id uint, reason string) {
	if s.Bus != nil {
		s.Bus.PublishChange("modelfile", ModelFileKey(id), reason)
	}
}

// UpsertModelFile records a model's files in a storage. A row without a
// storage belongs to the default local storage.
func (s *Store) UpsertModelFile(mf *ModelFile) (*ModelFile, error) {
	ref := s.resolveRef(FileRef{ModelID: mf.ModelID, StorageID: mf.StorageID, NodeID: mf.NodeID})
	mf.StorageID = ref.StorageID
	result, reason := mf, "created"
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing ModelFile
		err := refQuery(tx, ref).First(&existing).Error
		if err == nil {
			if existing.State == proto.MFDeleting {
				return ErrCacheDeletionPending
			}
			path := mf.Path
			if path == "" { // Struct updates leave zero-valued fields unchanged.
				path = existing.Path
			}
			if err := sharedModelPathConflict(tx, ref, path); err != nil {
				return err
			}
			mf.ID = existing.ID
			res := tx.Model(&existing).Where("state <> ?", proto.MFDeleting).Updates(mf)
			if res.Error != nil {
				return modelFilePathWriteError(tx, ref, path, res.Error)
			}
			if res.RowsAffected == 0 {
				return ErrCacheDeletionPending
			}
			if err := tx.First(&existing, existing.ID).Error; err != nil {
				return err
			}
			result, reason = &existing, "upsert"
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := sharedModelPathConflict(tx, ref, mf.Path); err != nil {
			return err
		}
		if err := tx.Create(mf).Error; err != nil {
			return modelFilePathWriteError(tx, ref, mf.Path, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishModelFile(result.ID, reason)
	return result, nil
}

// ModelFilesForModel lists a model's rows in every storage.
func (s *Store) ModelFilesForModel(modelID uint) ([]ModelFile, error) {
	var mfs []ModelFile
	err := s.db.Where("model_id = ?", modelID).Order("id asc").Find(&mfs).Error
	return mfs, err
}

// ModelFilesForModels reads the requested models' shared rows, the target's
// own rows and local-storage peer rows. Other nodes' caches cannot serve the
// target. A nodeID of zero retains every location before a target is selected.
func (s *Store) ModelFilesForModels(modelIDs []uint, nodeID uint) ([]ModelFile, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}
	query := s.reader().Where("model_id IN ?", modelIDs)
	if nodeID != 0 {
		localStorages := s.reader().Model(&Storage{}).Select("id").Where("type = ?", proto.StorageLocal)
		query = query.Where("node_id IN ? OR storage_id IN (?)", []uint{0, nodeID}, localStorages)
	}
	var files []ModelFile
	err := query.Find(&files).Error
	return files, err
}

// ModelFilesForModelOnNodes is ModelFilesForModel restricted to rows on the
// given nodes.
func (s *Store) ModelFilesForModelOnNodes(modelID uint, nodeIDs []uint) ([]ModelFile, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	var mfs []ModelFile
	err := s.db.Where("model_id = ? AND node_id IN ?", modelID, nodeIDs).Order("id asc").Find(&mfs).Error
	return mfs, err
}

// ModelFilesForNode lists the rows that live on a node: its local-storage
// files and its nfs_cache caches.
func (s *Store) ModelFilesForNode(nodeID uint) ([]ModelFile, error) {
	var mfs []ModelFile
	err := s.db.Where("node_id = ?", nodeID).Order("id asc").Find(&mfs).Error
	return mfs, err
}

// ModelFilesInStorage lists a storage's rows.
func (s *Store) ModelFilesInStorage(storageID uint) ([]ModelFile, error) {
	var mfs []ModelFile
	err := s.db.Where("storage_id = ?", storageID).Order("id asc").Find(&mfs).Error
	return mfs, err
}

// SharedModelFileAt returns the shared-storage row recorded at a path relative
// to the storage root (idx_mf_shared_path), or ErrNotFound.
func (s *Store) SharedModelFileAt(storageID uint, relPath string) (*ModelFile, error) {
	var mf ModelFile
	err := s.db.Where("storage_id = ? AND node_id = 0 AND path = ?", storageID, relPath).First(&mf).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &mf, err
}

// ModelFileByID loads one row.
func (s *Store) ModelFileByID(id uint) (*ModelFile, error) {
	var mf ModelFile
	err := s.db.First(&mf, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &mf, err
}

// ModelFileAt loads the row a reference names.
func (s *Store) ModelFileAt(ref FileRef) (*ModelFile, error) {
	ref = s.resolveRef(ref)
	var mf ModelFile
	err := refQuery(s.db, ref).First(&mf).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &mf, err
}

// ModelFileByModelNode returns a model's node-local file on a node in any
// local storage: a complete one first, the default local storage breaking
// ties. It serves the per-node views that predate storages; where a launch
// reads a model from is store.ModelSourceOn's to say.
func (s *Store) ModelFileByModelNode(modelID, nodeID uint) (*ModelFile, error) {
	if nodeID == 0 {
		return nil, ErrNotFound
	}
	var mfs []ModelFile
	err := s.db.Joins("JOIN storages ON storages.id = model_files.storage_id").
		Where("model_files.model_id = ? AND model_files.node_id = ? AND storages.type = ?", modelID, nodeID, proto.StorageLocal).
		Order("model_files.id asc").Find(&mfs).Error
	if err != nil {
		return nil, err
	}
	if len(mfs) == 0 {
		return nil, ErrNotFound
	}
	defaultID := s.DefaultLocalStorageID()
	best := 0
	rank := func(mf ModelFile) int {
		r := 0
		if proto.ModelFileStateReady(mf.State) {
			r += 2
		}
		if mf.StorageID == defaultID {
			r++
		}
		return r
	}
	// Rows are in id order, so of equally ranked rows the oldest wins and
	// every caller sees the same one.
	for i := range mfs {
		if rank(mfs[i]) > rank(mfs[best]) {
			best = i
		}
	}
	return &mfs[best], nil
}

// ResolveModelFileKey loads the row a reported key names.
func (s *Store) ResolveModelFileKey(key string) (*ModelFile, error) {
	parsed, ok := ParseModelFileKey(key)
	if !ok {
		return nil, errors.New("invalid model file key")
	}
	if !parsed.LegacyFormatted {
		return s.ModelFileByID(parsed.ID)
	}
	return s.ModelFileAt(FileRef{ModelID: parsed.LegacyModelID, NodeID: parsed.LegacyNodeID})
}

// UpdateModelFileStatus applies an agent status.modelfile report. A node may
// only report its own rows and the shared rows it was picked to write.
func (s *Store) UpdateModelFileStatus(nodeID uint, st proto.ModelFileStatus) error {
	current, err := s.ResolveModelFileKey(st.Key)
	if err != nil {
		return err
	}
	if current.NodeID != nodeID && !(current.NodeID == 0 && current.WriterNodeID == nodeID) {
		return errors.New("model file is not on the reporting node")
	}
	if current.State == proto.MFDeleting || current.State == proto.MFCanceled {
		return nil
	}
	if st.TransferID != "" && st.TransferID != current.TransferID {
		return nil
	}
	if st.TransferID == "" && current.State == proto.MFSyncing {
		return nil
	}
	if current.State == proto.MFAbsent && current.Acquisition == proto.ModelAcquireCopy {
		return nil // withdrawn intent, including a legacy report without an ID
	}
	// An owned wait/cancel must not be undone by the retired receiver's stop
	// report. A verified target completion remains authoritative.
	if (current.TransferErrorCode == "PEER_RELAY_BUSY" || current.TransferErrorCode == "PEER_TRANSFER_CANCELED") && !proto.ModelFileStateReady(st.State) {
		return nil
	}
	registeredMissing := current.Acquisition == proto.ModelAcquireRegistered &&
		st.Acquisition == proto.ModelAcquireRegistered && st.State == proto.MFAbsent
	if proto.ModelFileStateReady(current.State) &&
		st.State != proto.MFPresent && st.State != proto.MFRegistered && !registeredMissing {
		return nil
	}
	// A copy frame with a preparation observation moves counters only; the hub
	// keeps those in memory (prepobs), so only transitions reach SQLite.
	if st.Preparation != nil && st.State == proto.MFDownloading && current.State == st.State && (st.BytesTotal <= 0 || st.BytesTotal == current.SizeBytes) {
		s.publishModelFile(current.ID, bus.ReasonProgress)
		return nil
	}
	updates := map[string]any{
		"state":    st.State,
		"rate_bps": st.RateBytesPerS, "message": st.Message,
	}
	if proto.ModelFileStateReady(st.State) {
		updates["transfer_error_code"] = ""
	}
	if (current.RateLimit > 0 || current.State == proto.MFSyncing) && (st.State == proto.MFPresent || st.State == proto.MFFailed || st.State == proto.MFAbsent || st.State == proto.MFDownloadRequired) {
		updates["last_rate_limit"] = current.RateLimit
		updates["rate_limit"] = 0
	}
	if st.Revision != "" {
		updates["revision"] = st.Revision
	}
	// A reconnect/status replay may only confirm that an existing model is
	// present and therefore omit byte counters. Do not erase counters already
	// recorded by the completed transfer in that case.
	if st.BytesDone > 0 || st.State != proto.MFPresent {
		updates["downloaded"] = st.BytesDone
	}
	if st.TransferBytesDone > 0 || st.State != proto.MFPresent {
		updates["transferred_bytes"] = st.TransferBytesDone
	}
	if st.BytesTotal > 0 {
		updates["size_bytes"] = st.BytesTotal
	}
	// A shared row keeps its storage-relative path: the writer reports its own
	// absolute view of the mount, which other nodes do not share.
	if st.Path != "" && current.NodeID != 0 {
		updates["path"] = st.Path
	}
	if st.TransferID != "" {
		updates["transfer_id"] = st.TransferID
	}
	if st.Acquisition != "" {
		updates["acquisition"] = st.Acquisition
	}
	res := s.db.Model(&ModelFile{}).Where("id = ? AND state = ? AND COALESCE(transfer_id, '') = ? AND transfer_error_code = ?", current.ID, current.State, current.TransferID, current.TransferErrorCode).Updates(updates)
	if res.Error == nil && res.RowsAffected > 0 {
		// A four-times-a-second download tick moves byte counters and nothing
		// else. Marking it as progress is what stops every such tick from
		// re-queueing every deployment in the cluster; a state change still
		// carries the state, so a model becoming present still unblocks the
		// deployments waiting on it.
		reason := st.State
		if current.State == st.State && (st.State == proto.MFDownloading || st.State == proto.MFSyncing) {
			reason = bus.ReasonProgress
		}
		s.publishModelFile(current.ID, reason)
	}
	return res.Error
}

// BeginModelSync marks a row as receiving a peer transfer.
func (s *Store) BeginModelSync(ref FileRef, sourceNodeID uint, transferID string, expectedBytes int64, rate ...int) error {
	if transferID == "" {
		return errors.New("model sync transfer id is required")
	}
	ref = s.resolveRef(ref)
	updates := map[string]any{
		"state": proto.MFSyncing, "acquisition": proto.ModelAcquirePeer,
		"transfer_id": transferID, "transfer_source_node_id": sourceNodeID,
		"downloaded": 0, "transferred_bytes": 0, "rate_bps": 0, "message": "", "transfer_error_code": "",
	}
	if len(rate) > 0 {
		if rate[0] < 0 {
			return errors.New("model sync rate must be non-negative")
		}
		updates["rate_limit"] = rate[0]
	}
	if expectedBytes > 0 {
		updates["size_bytes"] = expectedBytes
	}
	return s.updateRef(ref, updates, proto.MFSyncing)
}

// transferReset is the set of columns every Prepare* below rewrites when it
// puts a row into a new state: where the model goes, the state and how it is
// acquired, and everything a previous transfer left behind. Each Prepare*
// adds the columns only it resets (extra wins over the base).
func transferReset(path, state, acquisition string, extra map[string]any) map[string]any {
	updates := map[string]any{
		"path": path, "state": state, "acquisition": acquisition,
		"transfer_id": "", "transfer_source_node_id": 0,
		"downloaded": 0, "transferred_bytes": 0, "rate_bps": 0, "message": "", "transfer_error_code": "",
	}
	for column, value := range extra {
		updates[column] = value
	}
	return updates
}

// PrepareModelDownload resets a row to a pending public download. Every
// preparation is a new transfer: its TransferID travels in the task and comes
// back on each status the agent reports, so UpdateModelFileStatus can drop the
// reports of a download an earlier preparation left running. An agent that
// predates download IDs reports none; those reports are still accepted.
func (s *Store) PrepareModelDownload(ref FileRef, path string) (*ModelFile, error) {
	return s.prepareModelFile(ref, path, transferReset(path, proto.MFPending, proto.ModelAcquireDownload, map[string]any{
		"revision": "", "transfer_id": auth.NewUUID(), "download_deployment_id": 0,
	}))
}

// PrepareModelPeerSync resets a row to a pending peer transfer.
func (s *Store) PrepareModelPeerSync(ref FileRef, path string) (*ModelFile, error) {
	return s.prepareModelFile(ref, path, transferReset(path, proto.MFPending, proto.ModelAcquireAuto, map[string]any{
		"rate_limit": 0,
	}))
}

// PrepareModelCopy resets a row to a pending same-node copy. A rate limit
// belongs to the copy that asked for it, so a new copy starts unlimited and
// the caller sets its own (SetModelCopyRate); a retry keeps the copy's limit.
func (s *Store) PrepareModelCopy(ref FileRef, path string) (*ModelFile, error) {
	return s.prepareModelFile(ref, path, transferReset(path, proto.MFPending, proto.ModelAcquireCopy, map[string]any{"transfer_id": auth.NewUUID(), "rate_limit": 0}))
}

// RegisterModelFile records files that are already on disk.
func (s *Store) RegisterModelFile(ref FileRef, path string, sizeBytes int64) (*ModelFile, error) {
	mf, err := s.prepareModelFile(ref, path, transferReset(path, proto.MFRegistered, proto.ModelAcquireRegistered, map[string]any{
		"size_bytes": sizeBytes, "downloaded": sizeBytes,
	}))
	return mf, err
}

func (s *Store) prepareModelFile(ref FileRef, path string, updates map[string]any) (*ModelFile, error) {
	ref = s.resolveRef(ref)
	var mf *ModelFile
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := sharedModelPathConflict(tx, ref, path); err != nil {
			return err
		}
		var err error
		mf, err = ensureModelFile(tx, ref, path)
		if err != nil {
			return modelFilePathWriteError(tx, ref, path, err)
		}
		res := tx.Model(&ModelFile{}).Where("id = ? AND state <> ?", mf.ID, proto.MFDeleting).Updates(updates)
		if res.Error != nil {
			return modelFilePathWriteError(tx, ref, path, res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrCacheDeletionPending
		}
		return tx.First(mf, mf.ID).Error
	})
	if err != nil {
		return nil, err
	}
	s.publishModelFile(mf.ID, mf.State)
	return mf, nil
}

func ensureModelFile(db *gorm.DB, ref FileRef, path string) (*ModelFile, error) {
	var mf ModelFile
	err := refQuery(db, ref).First(&mf).Error
	if err == nil {
		return &mf, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	mf = ModelFile{ModelID: ref.ModelID, StorageID: ref.StorageID, NodeID: ref.NodeID, Path: path}
	if err := db.Create(&mf).Error; err != nil {
		return nil, err
	}
	return &mf, nil
}

func (s *Store) updateRef(ref FileRef, updates map[string]any, reason string) error {
	var mf ModelFile
	if err := refQuery(s.db, ref).First(&mf).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if mf.State == proto.MFDeleting {
		return ErrCacheDeletionPending
	}
	res := s.db.Model(&ModelFile{}).Where("id = ? AND state <> ?", mf.ID, proto.MFDeleting).Updates(updates)
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(mf.ID, reason)
	}
	return res.Error
}

// SetModelSyncDownloadRequired parks an unfinished transfer until the user
// picks another source.
func (s *Store) SetModelSyncDownloadRequired(ref FileRef, transferID, message string) error {
	return s.SetModelSyncDownloadRequiredCode(ref, transferID, message, "")
}

// SetModelSyncSource records the user-selected peer for the next sync.
func (s *Store) SetModelSyncSource(ref FileRef, sourceNodeID uint) error {
	ref = s.resolveRef(ref)
	return refQuery(s.db.Model(&ModelFile{}), ref).
		Updates(map[string]any{"transfer_source_node_id": sourceNodeID, "acquisition": proto.ModelAcquirePeer}).Error
}

// SetModelSyncRate records a per-transfer rate limit.
func (s *Store) SetModelSyncRate(ref FileRef, rateLimitKiB int) error {
	if rateLimitKiB < 0 {
		return errors.New("model sync rate limit must be non-negative")
	}
	ref = s.resolveRef(ref)
	return refQuery(s.db.Model(&ModelFile{}), ref).Updates(map[string]any{"rate_limit": rateLimitKiB, "acquisition": proto.ModelAcquirePeer}).Error
}

// SetModelFileWriter picks the node that writes a shared row.
func (s *Store) SetModelFileWriter(id, writerNodeID uint) error {
	res := s.db.Model(&ModelFile{}).Where("id = ?", id).Update("writer_node_id", writerNodeID)
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(id, "writer")
	}
	return res.Error
}

// TouchModelFile records that an instance started or stopped on a row's files.
func (s *Store) TouchModelFile(id uint, at time.Time) error {
	if id == 0 {
		return nil
	}
	return s.db.Model(&ModelFile{}).Where("id = ?", id).UpdateColumn("last_used_at", at.UTC()).Error
}

// DeleteModelFileRow removes one row without touching disk.
func (s *Store) DeleteModelFileRow(id uint) error {
	res := s.db.Where("state <> ?", proto.MFDeleting).Delete(&ModelFile{}, id)
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(id, "deleted")
	}
	return res.Error
}

// SetModelFileState sets a row's state and message, e.g. a shared row a
// rescan no longer finds.
func (s *Store) SetModelFileState(id uint, state, message string) error {
	res := s.db.Model(&ModelFile{}).Where("id = ? AND state <> ?", id, proto.MFDeleting).Updates(map[string]any{"state": state, "message": message})
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(id, state)
	}
	return res.Error
}

// SetModelCopySource records where a copy reads from: a storage, and for a
// local source storage the node holding the files.
func (s *Store) SetModelCopySource(id, sourceStorageID, sourceNodeID uint) error {
	return s.db.Model(&ModelFile{}).Where("id = ?", id).Updates(map[string]any{
		"transfer_source_storage_id": sourceStorageID, "transfer_source_node_id": sourceNodeID,
		"acquisition": proto.ModelAcquireCopy,
	}).Error
}

// SyncingModelFile is the row of a model on a node that a peer transfer is
// currently filling, in whichever storage.
func (s *Store) SyncingModelFile(modelID, nodeID uint) (*ModelFile, error) {
	var mf ModelFile
	err := s.db.Where("model_id = ? AND node_id = ? AND state = ? AND transfer_id <> ''", modelID, nodeID, proto.MFSyncing).Order("id asc").First(&mf).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &mf, err
}

// NodeFilesDeletable reports whether a node's own files in the storage may be
// deleted: a read-only (disabled) local storage keeps its files, while an
// nfs_cache storage's node caches stay this control plane's to evict however
// the storage is set (docs/adr/0034-model-files-belong-to-storages.md).
func NodeFilesDeletable(storage *Storage) bool {
	return storage != nil && (storage.Type != proto.StorageLocal || storage.Enabled)
}

// NodeNeedsOwnDirectory reports whether a node cannot use a local or
// nfs_cache storage's directory and needs one of its own (a per-node path):
// the agent may not create or write it, or the node has none set. A
// connected node's report is what this reads; an offline one says nothing.
func NodeNeedsOwnDirectory(storageType string, state *StorageNodeState) bool {
	if state == nil || (storageType != proto.StorageLocal && storageType != proto.StorageNFSCache) {
		return false
	}
	if state.State == proto.StorageStateReady {
		return storageType == proto.StorageLocal && !state.Writable
	}
	return state.Reason == proto.StorageReasonPermissionDenied || state.Reason == proto.StorageReasonPathMissing
}
