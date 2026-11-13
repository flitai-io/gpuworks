package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ErrStateChanged reports a conditional write that found its rows no longer
// in the state the caller read them in: another writer (the controller, a
// concurrent request) got there first, and nothing was written.
var ErrStateChanged = errors.New("state changed since it was read")

// ReplicaFillingStates are the instance states that fill a replica
// (proto.InstanceFillsReplica), for conditional writes.
func ReplicaFillingStates() []string {
	var out []string
	for _, state := range proto.InstanceStates() {
		if proto.InstanceFillsReplica(state) {
			out = append(out, state)
		}
	}
	return out
}

// transitionInstances moves the named instances of a deployment from one of
// the from states to to, inside tx, all or none: when any of them is in
// another state it writes nothing and returns ErrStateChanged. It returns the
// rows as they were before the write.
func transitionInstances(tx *gorm.DB, depID uint, uuids []string, from []string, to, message string) ([]Instance, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	var rows []Instance
	if err := tx.Where("deployment_id = ? AND uuid IN ? AND state IN ?", depID, uuids, from).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) != len(uuids) {
		return nil, ErrStateChanged
	}
	result := tx.Model(&Instance{}).Where("deployment_id = ? AND uuid IN ? AND state IN ?", depID, uuids, from).
		Updates(map[string]any{"state": to, "message": message, "runtime_failure": ""})
	if result.Error != nil {
		return nil, result.Error
	}
	if int(result.RowsAffected) != len(uuids) {
		return nil, ErrStateChanged
	}
	return rows, nil
}

// announceTransition records and publishes a committed transition.
func (s *Store) announceTransition(rows []Instance, to, message string) {
	for i := range rows {
		s.AddInstanceEventCode(&rows[i], "state", instanceEventStateChanged, map[string]any{"state": to, "detail": message})
		s.Bus.PublishChange("instance", rows[i].UUID, to)
	}
}

// TransitionInstances moves the named instances of a deployment from one of
// the from states to to, all or none (ErrStateChanged when any has moved on),
// and records the change on each instance's timeline.
func (s *Store) TransitionInstances(depID uint, uuids []string, from []string, to, message string) error {
	var rows []Instance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		rows, err = transitionInstances(tx, depID, uuids, from, to, message)
		return err
	})
	if err != nil {
		return err
	}
	s.announceTransition(rows, to, message)
	return nil
}

// StartDeployment makes a stopped deployment active again and returns the
// state it is in afterwards: active when it started or already ran, the
// unchanged state otherwise (deleting). The condition is part of the write,
// so a deletion that lands first is never undone.
func (s *Store) StartDeployment(id uint) (DeploymentState, error) {
	result := s.db.Model(&Deployment{}).Where("id = ? AND desired_state = ?", id, string(DeployStopped)).Update("desired_state", string(DeployActive))
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected > 0 {
		s.Bus.PublishChange("deployment", uintKey(id), "state:"+string(DeployActive))
		return DeployActive, nil
	}
	dep, err := s.GetDeployment(id)
	if err != nil {
		return "", err
	}
	return dep.State, nil
}

// deploymentRestartMessage is what an instance a deployment restart retires
// says.
const deploymentRestartMessage = "deployment restart"

// RestartDeploymentNow retires every instance of an active deployment that
// has not already stopped or begun leaving, failed attempts included (a
// restart is the operator trying again), and ends a rolling restart in
// progress, in one transaction. It returns how many instances it retired;
// ErrStateChanged when the deployment is no longer active.
func (s *Store) RestartDeploymentNow(id uint) (int, error) {
	var rows []Instance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		rows, err = restartDeploymentNowTx(tx, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.announceTransition(rows, proto.InstRemoving, deploymentRestartMessage)
	s.Bus.PublishChange("deployment", uintKey(id), "restart")
	return len(rows), nil
}

// restartDeploymentNowTx is RestartDeploymentNow inside tx, for the writes
// that restart a deployment as part of a larger change (an edit).
func restartDeploymentNowTx(tx *gorm.DB, id uint) ([]Instance, error) {
	result := tx.Model(&Deployment{}).Where("id = ? AND desired_state = ?", id, string(DeployActive)).Update("restart_requested_at", nil)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrStateChanged
	}
	var uuids []string
	if err := tx.Model(&Instance{}).Where("deployment_id = ? AND state NOT IN ?", id, []string{proto.InstStopped, proto.InstRemoving, proto.InstPendingRemoval}).Pluck("uuid", &uuids).Error; err != nil {
		return nil, err
	}
	from := append(ReplicaFillingStates(), proto.InstFailed, proto.InstDraining)
	return transitionInstances(tx, id, uuids, from, proto.InstRemoving, deploymentRestartMessage)
}

// RequestRollingRestart records a rolling restart of an active deployment
// asked for at at and retires the replicas' failed attempts in the same
// transaction, so every replica is replaced with its whole retry budget. A
// newer request replaces an older one: the replicas already replaced since
// the older one are replaced again. ErrStateChanged when the deployment is no
// longer active.
func (s *Store) RequestRollingRestart(id uint, at time.Time) error {
	var rows []Instance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		rows, err = requestRollingRestartTx(tx, id, at)
		return err
	})
	if err != nil {
		return err
	}
	s.announceTransition(rows, proto.InstRemoving, deploymentRestartMessage)
	s.Bus.PublishChange("deployment", uintKey(id), "restart")
	return nil
}

// requestRollingRestartTx is RequestRollingRestart inside tx.
func requestRollingRestartTx(tx *gorm.DB, id uint, at time.Time) ([]Instance, error) {
	result := tx.Model(&Deployment{}).Where("id = ? AND desired_state = ?", id, string(DeployActive)).Update("restart_requested_at", at)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrStateChanged
	}
	var failed []string
	if err := tx.Model(&Instance{}).Where("deployment_id = ? AND state = ?", id, proto.InstFailed).Pluck("uuid", &failed).Error; err != nil {
		return nil, err
	}
	return transitionInstances(tx, id, failed, []string{proto.InstFailed}, proto.InstRemoving, deploymentRestartMessage)
}
