package store

import (
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// InstanceModelFile is one model file an instance reads, as it was when the
// instance launched: the row (a node's local file, a node's cache entry, or a
// shared storage's single row) and the host path the container mounts. "Is
// this model file in use" is answered from these rows, never from a
// deployment's storage: a deployment reads whichever copy each node has.
type InstanceModelFile struct {
	ID          uint   `gorm:"primaryKey"`
	InstanceID  uint   `gorm:"index"`
	ModelID     uint   `gorm:"index"`
	ModelFileID uint   `gorm:"index"`
	HostPath    string `gorm:"size:1024"`
}

// endedInstanceStates are the states in which an instance no longer holds
// its model files open.
var endedInstanceStates = []string{proto.InstStopped, proto.InstFailed}

// InstanceModelFiles lists what each of the instances reads, by instance id.
func (s *Store) InstanceModelFiles(instanceIDs []uint) (map[uint][]InstanceModelFile, error) {
	out := map[uint][]InstanceModelFile{}
	if len(instanceIDs) == 0 {
		return out, nil
	}
	var rows []InstanceModelFile
	if err := s.reader().Where("instance_id IN ?", instanceIDs).Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.InstanceID] = append(out[row.InstanceID], row)
	}
	return out, nil
}

// liveReads selects the rows of instances that still hold their files, joined
// with the model file each reads.
func liveReads(db *gorm.DB) *gorm.DB {
	return db.Table("instance_model_files").
		Joins("JOIN instances ON instances.id = instance_model_files.instance_id").
		Joins("JOIN model_files ON model_files.id = instance_model_files.model_file_id").
		Where("instances.state NOT IN ?", endedInstanceStates)
}

// liveReadersOfStorage lists the live instances that read a storage's files
// and their deployments; nodeID 0 means on any node.
func liveReadersOfStorage(db *gorm.DB, storageID, nodeID uint) (deploymentIDs []uint, instanceUUIDs []string, err error) {
	q := liveReads(db).Where("model_files.storage_id = ?", storageID)
	if nodeID != 0 {
		q = q.Where("instances.node_id = ?", nodeID)
	}
	var rows []struct {
		DeploymentID uint
		UUID         string
	}
	if err := q.Distinct("instances.deployment_id", "instances.uuid").Order("instances.deployment_id asc, instances.uuid asc").Scan(&rows).Error; err != nil {
		return nil, nil, err
	}
	seen := map[uint]bool{}
	for _, row := range rows {
		if !seen[row.DeploymentID] {
			seen[row.DeploymentID] = true
			deploymentIDs = append(deploymentIDs, row.DeploymentID)
		}
		instanceUUIDs = append(instanceUUIDs, row.UUID)
	}
	return deploymentIDs, instanceUUIDs, nil
}

// DeploymentsReadingFiles lists, for each of a storage's model file rows that
// a live instance reads, the deployments of those instances.
func (s *Store) DeploymentsReadingFiles(storageID uint) (map[uint][]uint, error) {
	var rows []struct {
		ModelFileID  uint
		DeploymentID uint
	}
	if err := liveReads(s.reader()).Where("model_files.storage_id = ?", storageID).
		Distinct("instance_model_files.model_file_id", "instances.deployment_id").
		Order("instances.deployment_id asc").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[uint][]uint{}
	for _, row := range rows {
		out[row.ModelFileID] = append(out[row.ModelFileID], row.DeploymentID)
	}
	return out, nil
}

// touchInstanceModelFiles records that an instance started or stopped on its
// model files, which is what "least recently used" orders node caches by.
func (s *Store) touchInstanceModelFiles(inst *Instance) {
	var ids []uint
	if err := s.db.Model(&InstanceModelFile{}).Where("instance_id = ?", inst.ID).Pluck("model_file_id", &ids).Error; err != nil {
		return
	}
	now := time.Now().UTC()
	for _, id := range ids {
		_ = s.TouchModelFile(id, now)
	}
}
