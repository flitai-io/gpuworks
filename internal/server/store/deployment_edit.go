package store

import (
	"errors"
	"maps"
	"slices"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// DeploymentEditApply is how an edit of a deployment's configuration reaches
// its instances.
type DeploymentEditApply string

const (
	// EditApplyNone writes the configuration only: running instances keep
	// the one they started with, and the next launch takes the new one.
	EditApplyNone DeploymentEditApply = "none"
	// EditApplyRolling replaces the replicas one at a time (a rolling
	// restart, RequestRollingRestart).
	EditApplyRolling DeploymentEditApply = "rolling"
	// EditApplyAll retires every instance at once (RestartDeploymentNow).
	EditApplyAll DeploymentEditApply = "all"
)

// DeploymentEdit is one edit of a deployment's configuration: the row as it
// should be afterwards, written as one change with the way it is applied.
type DeploymentEdit struct {
	// Next is the deployment with the edit applied. Only the columns an edit
	// may change (editColumnValues) are written from it; the rest of the row -
	// its identity, model, template and shape - is not an edit's to change.
	Next *Deployment
	// Requirements replace the deployment's node requirements; nil keeps them.
	Requirements nodelabel.Requirements
	Apply        DeploymentEditApply
	// Start makes a stopped deployment active in the same write.
	Start bool
	// Wake withdraws a deployment-wide sleep order in the same write, so the
	// instances an EditApplyAll starts come up awake.
	Wake bool
}

// EditDeployment writes an edit of a deployment read as read (with the
// requirements readReqs) and applies it, in one transaction. The edit was
// judged against what was read, so it is written only while that still holds:
// ErrStateChanged when the deployment's configuration, requirements or
// desired state moved on since, or when the apply needs an active deployment
// and it is not one. ErrNotFound when it is gone. Events are published after
// the commit.
func (s *Store) EditDeployment(read *Deployment, readReqs nodelabel.Requirements, edit DeploymentEdit) error {
	if read == nil || edit.Next == nil || edit.Next.ID != read.ID {
		return errors.New("edit deployment: next must be the deployment read")
	}
	var normalized nodelabel.Requirements
	if edit.Requirements != nil {
		var err error
		if normalized, err = nodelabel.NormalizeRequirements(edit.Requirements); err != nil {
			return err
		}
	}
	id := read.ID
	var retired []Instance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var current Deployment
		if err := tx.First(&current, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if !sameEditBasis(&current, read) {
			return ErrStateChanged
		}
		currentReqs, err := requirementsTx(tx, id)
		if err != nil {
			return err
		}
		if !sameRequirements(currentReqs, readReqs) {
			return ErrStateChanged
		}
		updates := editColumnValues(edit.Next)
		if edit.Start && current.State == DeployStopped {
			updates["desired_state"] = string(DeployActive)
		}
		if edit.Wake {
			updates["desired_sleep"] = 0
		}
		if err := tx.Model(&Deployment{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		if edit.Requirements != nil {
			if err := tx.Where("deployment_id = ?", id).Delete(&DeploymentNodeRequirement{}).Error; err != nil {
				return err
			}
			if rows := requirementRows(id, normalized); len(rows) > 0 {
				if err := tx.Create(&rows).Error; err != nil {
					return err
				}
			}
		}
		if edit.Wake {
			var insts []Instance
			if err := tx.Where("deployment_id = ?", id).Find(&insts).Error; err != nil {
				return err
			}
			if err := writeSleepIntent(tx, insts, 0, "", false); err != nil {
				return err
			}
		}
		switch edit.Apply {
		case EditApplyRolling:
			retired, err = requestRollingRestartTx(tx, id, time.Now().UTC())
		case EditApplyAll:
			retired, err = restartDeploymentNowTx(tx, id)
		case EditApplyNone, "":
		default:
			err = errors.New("edit deployment: unknown apply " + string(edit.Apply))
		}
		return err
	})
	if err != nil {
		return err
	}
	s.announceTransition(retired, proto.InstRemoving, deploymentRestartMessage)
	s.Bus.PublishChange("deployment", uintKey(id), "edit")
	if edit.Requirements != nil {
		s.Bus.PublishChange("deployment", uintKey(id), DeploymentChangeRequirements)
	}
	return nil
}

// editColumnValues are next's values of the columns an edit writes, keyed as
// GORM updates them.
func editColumnValues(next *Deployment) map[string]any {
	return map[string]any{
		"quick_params": next.QuickParams, "extra_args": next.ExtraArgs, "container_overrides": next.ContainerOverrides,
		"image": next.Image, "template_snapshot": next.CatalogSelectionSnapshot, "template_deviated": next.TemplateDeviated,
		"sleepable": next.Sleepable, "VRAMLimitBytes": next.VRAMLimitBytes, "partition_backend": next.PartitionBackend,
		"placement": next.Placement, "node_id": next.NodeID,
		"replicas": next.Replicas, "routing_policy": next.RoutingPolicy,
		"health_check": next.HealthCheck, "health_check_threshold_sec": next.HealthCheckThresholdSec,
		"tokenize_disabled": next.TokenizeDisabled, "priority_scheduling": next.PriorityScheduling, "start_timeout_sec": next.StartTimeoutSec,
		"placement_strategy": next.PlacementStrategy,
	}
}

// sameEditBasis reports whether current still is the deployment an edit read:
// the same configuration (every column an edit writes, and the identity it
// was judged against) and the same desired state. Columns the controller
// keeps moving - a rolling restart's progress, the sleep level - are not part
// of it, so an edit is not refused for them.
func sameEditBasis(current, read *Deployment) bool {
	if !maps.Equal(editColumnValues(current), editColumnValues(read)) {
		return false
	}
	return current.State == read.State && current.ModelID == read.ModelID && current.TemplateID == read.TemplateID &&
		current.TemplateSpecSource == read.TemplateSpecSource && current.TemplateSpecName == read.TemplateSpecName &&
		current.Engine == read.Engine && current.Name == read.Name && current.ServedName == read.ServedName &&
		current.NNodes == read.NNodes && current.NodeIDs == read.NodeIDs && current.SaveToStorageID == read.SaveToStorageID &&
		current.MasterAddr == read.MasterAddr && current.MasterPort == read.MasterPort
}

func requirementsTx(tx *gorm.DB, id uint) (nodelabel.Requirements, error) {
	var rows []DeploymentNodeRequirement
	if err := tx.Where("deployment_id = ?", id).Order("key, value").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := nodelabel.Requirements{}
	for _, row := range rows {
		out[row.Key] = append(out[row.Key], row.Value)
	}
	return out, nil
}

// sameRequirements compares two requirement sets as sets: the order a key's
// values were read in does not matter.
func sameRequirements(a, b nodelabel.Requirements) bool {
	if len(a) != len(b) {
		return false
	}
	for key, values := range a {
		other, ok := b[key]
		if !ok || len(values) != len(other) {
			return false
		}
		left, right := slices.Clone(values), slices.Clone(other)
		slices.Sort(left)
		slices.Sort(right)
		if !slices.Equal(left, right) {
			return false
		}
	}
	return true
}
