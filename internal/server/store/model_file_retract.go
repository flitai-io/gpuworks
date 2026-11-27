package store

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// retractModelFile takes back a ready row of a node's own disk after that
// node looked and did not find the files. A registered path stays as the
// user's assertion, marked absent; any other row goes, so the next
// deployment that needs the model on that node acquires it as if it had never
// been there.
//
// Only a node-local copy is one node's to take back. A shared storage's row
// is not: the node may simply have lost its mount, and the storage rescan
// decides that (MFMissing). Neither is a copy of a storage the node reports
// as anything but ready: a mount that is gone, or did not answer the last
// check, shows an empty directory where the model was, which reads as the
// directory being absent although the files are fine.
func (s *Store) retractModelFile(row *ModelFile) error {
	if row.NodeID == 0 || !row.Ready() || !s.nodeLocalModelFiles(row) {
		return nil
	}
	q := s.db.Model(&ModelFile{}).Where("id = ? AND state = ?", row.ID, row.State)
	if row.Acquisition == proto.ModelAcquireRegistered {
		res := q.Updates(map[string]any{"state": proto.MFAbsent, "message": ""})
		if res.Error == nil && res.RowsAffected > 0 {
			s.publishModelFile(row.ID, proto.MFAbsent)
		}
		return res.Error
	}
	res := q.Delete(&ModelFile{})
	if res.Error == nil && res.RowsAffected > 0 {
		s.publishModelFile(row.ID, "deleted")
	}
	return res.Error
}

// retractMissingModelFiles applies a launch that failed with
// proto.ErrorModelPathMissing: the rows on the node's own disk that the
// instance was launched to read at path are retracted.
func (s *Store) retractMissingModelFiles(inst *Instance, path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	var rows []ModelFile
	if err := s.db.Where("node_id = ? AND id IN (?)", inst.NodeID,
		s.db.Model(&InstanceModelFile{}).Select("model_file_id").Where("instance_id = ? AND host_path = ?", inst.ID, path)).Find(&rows).Error; err != nil {
		return
	}
	for i := range rows {
		_ = s.retractModelFile(&rows[i])
	}
}

// nodeLocalModelFiles reports whether row is a copy on the reporting node's
// own disk whose storage the node reports as ready (or has not reported on
// yet, which older agents do). A shared storage keeps one row for every
// node, and a node's cache of a shared storage is its own only while the
// storage answers there.
func (s *Store) nodeLocalModelFiles(row *ModelFile) bool {
	storage, err := s.StorageByID(row.StorageID)
	if err != nil {
		return false
	}
	switch storage.Type {
	case proto.StorageLocal, proto.StorageNFSCache:
	default:
		return false
	}
	state, err := s.StorageNodeStateFor(row.StorageID, row.NodeID)
	if err != nil {
		return true // the node has not reported on it (an older agent)
	}
	return state.State == proto.StorageStateReady
}
