package store

import (
	"errors"

	"gorm.io/gorm"
)

// LatestDeploymentEvent is the deployment's newest event with one of the
// codes, found false when it has none (or they were trimmed). It walks the
// (deployment_id, id) index back from the newest event of the deployment.
func (s *Store) LatestDeploymentEvent(deploymentID uint, codes []string) (InstanceEvent, bool, error) {
	if deploymentID == 0 || len(codes) == 0 {
		return InstanceEvent{}, false, nil
	}
	var ev InstanceEvent
	err := s.reader().Where("deployment_id = ? AND code IN ?", deploymentID, codes).
		Order("id DESC").First(&ev).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return InstanceEvent{}, false, nil
	}
	return ev, err == nil, err
}

// DeploymentEventsByCode are the deployment's newest events with one of the
// codes, newest first, at most limit of them. Like LatestDeploymentEvent it
// walks the (deployment_id, id) index back from the newest.
func (s *Store) DeploymentEventsByCode(deploymentID uint, codes []string, limit int) ([]InstanceEvent, error) {
	if deploymentID == 0 || len(codes) == 0 || limit <= 0 {
		return nil, nil
	}
	var evs []InstanceEvent
	err := s.reader().Where("deployment_id = ? AND code IN ?", deploymentID, codes).
		Order("id DESC").Limit(limit).Find(&evs).Error
	return evs, err
}
