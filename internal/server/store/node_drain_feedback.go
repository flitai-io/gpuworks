package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// LatestDrainFailure reads a bounded newest attempt, not every retained
// generation. IDs decide order even when multiple rows have the same timestamp.
// A collective's last NNodes rows include its newest generation's ranks, so a
// failed follower is visible even when its leader was inserted last. The
// returned row carries only the failure's id, message and runtime failure;
// nil when there is none.
func (s *Store) LatestDrainFailure(deploymentID, sourceNodeID, sourceInstanceID uint) (*Instance, error) {
	var latest Instance
	err := s.reader().Select("id", "node_id", "state", "message", "runtime_failure", "dist_group", "dist_attempt", "n_nodes").Where("deployment_id = ?", deploymentID).Order("id desc").First(&latest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if latest.ID <= sourceInstanceID {
		return nil, nil
	}
	if latest.NNodes <= 1 || latest.DistGroup == "" || latest.DistAttempt == 0 {
		if latest.NodeID != sourceNodeID && latest.State == proto.InstFailed {
			return &latest, nil
		}
		return nil, nil
	}
	recent := s.reader().Model(&Instance{}).Select("id").Where("deployment_id = ?", deploymentID).Order("id desc").Limit(latest.NNodes)
	var failed Instance
	err = s.reader().Select("id", "message", "runtime_failure").Where("id IN (?) AND node_id <> ? AND dist_group = ? AND dist_attempt = ? AND state = ?", recent, sourceNodeID, latest.DistGroup, latest.DistAttempt, proto.InstFailed).Order("id desc").First(&failed).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &failed, nil
}
