package store

import (
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// InstanceProgressOnly reports whether st carries nothing but the progress of
// the state and phase it names: no ports, measurements, sleep facts, runtime
// observation, served paths, assets or failure. Such a report, against a row
// already in that state and phase, is what UpdateInstanceStatusFromNode
// writes as plain column updates with a progress notification and no event,
// so a newer one for the same instance fully replaces it.
func InstanceProgressOnly(st proto.InstanceStatus) bool {
	switch st.State {
	case "", proto.InstRunning, proto.InstRemoving, proto.InstDraining:
		return false
	}
	for _, finished := range proto.InstanceFinishedStates() {
		if st.State == finished {
			return false
		}
	}
	return st.Phase != proto.PhaseSleep && st.RuntimeFailure == nil && len(st.Ports) == 0 &&
		len(st.Measured) == 0 && st.SleepLevel == 0 && st.SleepTarget == nil &&
		st.SleepResidueBytes == 0 && !st.L1HostBufferHeld && len(st.RuntimeAssetDigests) == 0 &&
		st.RuntimeObservation == nil && st.OpenAPIPaths == nil
}

// ApplyInstanceProgress writes progress-only reports (InstanceProgressOnly)
// from nodeID in one transaction. A report applies only to the reporting
// node's row that is still in the report's state and phase, awake: exactly
// when UpdateInstanceStatusFromNode would write the same columns and publish
// nothing but progress. Every other report is returned unapplied, in order,
// for that full path, which owns transitions, events and their guards. Once
// one report of an instance is returned, so is every later one of it: the
// full path applies them after this batch, so applying a later one here would
// let the earlier one land last.
func (s *Store) ApplyInstanceProgress(nodeID uint, reports []proto.InstanceStatus) (unapplied []proto.InstanceStatus, err error) {
	var applied []string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		applied, unapplied = applied[:0], unapplied[:0]
		deferred := map[string]bool{}
		for _, st := range reports {
			if nodeID == 0 || deferred[st.UUID] || !InstanceProgressOnly(st) {
				deferred[st.UUID] = true
				unapplied = append(unapplied, st)
				continue
			}
			updates := map[string]any{
				"state": st.State, "container_id": st.ContainerID, "message": st.Message, "runtime_failure": "",
				"phase": st.Phase, "progress": st.Progress, "bytes_done": st.BytesDone, "bytes_total": st.BytesTotal,
				"rate_bps": st.RateBPS, "direct_addr": st.DirectAddr,
			}
			if st.State == proto.InstPending || st.State == proto.InstPulling || st.State == proto.InstStarting {
				updates["l1_host_buffer_held"] = false
			}
			result := tx.Model(&Instance{}).
				Where("uuid = ? AND node_id = ? AND state = ? AND phase = ? AND sleep_level = 0", st.UUID, nodeID, st.State, st.Phase).
				Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				deferred[st.UUID] = true
				unapplied = append(unapplied, st)
				continue
			}
			applied = append(applied, st.UUID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, uuid := range applied {
		s.Bus.PublishChange("instance", uuid, bus.ReasonProgress)
	}
	return unapplied, nil
}
