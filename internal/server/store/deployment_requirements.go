package store

import (
	"fmt"
	"sort"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// PlacementStrategy is how a deployment's replicas are spread over its
// candidate nodes. It is the user's preference and nothing else records it;
// the persisted column stays a plain string.
type PlacementStrategy string

const (
	// PlacementSpread prefers the candidate running the fewest of the
	// deployment's replicas. It is the default.
	PlacementSpread PlacementStrategy = "spread"
	// PlacementBinpack prefers the candidate with the least room left that
	// still fits a replica.
	PlacementBinpack PlacementStrategy = "binpack"
)

// PlacementStrategies returns every strategy value.
func PlacementStrategies() []PlacementStrategy {
	return []PlacementStrategy{PlacementSpread, PlacementBinpack}
}

// DeploymentNodeRequirement is one allowed value of one key of a deployment's
// node requirements (nodelabel.Requirements): the values of a key are
// alternatives, and every key must hold. One row per value makes "which
// deployments name this node or this network" a query on (key, value) - what
// deleting a network or draining a node has to ask - rather than a scan of
// every deployment's placement document.
type DeploymentNodeRequirement struct {
	DeploymentID uint   `gorm:"primaryKey;autoIncrement:false"`
	Key          string `gorm:"primaryKey;size:128;index:idx_deployment_node_requirements_key_value,priority:1"`
	Value        string `gorm:"primaryKey;size:256;index:idx_deployment_node_requirements_key_value,priority:2"`
}

// requirementRows are the rows of one deployment's requirements, in key then
// value order.
func requirementRows(deploymentID uint, reqs nodelabel.Requirements) []DeploymentNodeRequirement {
	keys := make([]string, 0, len(reqs))
	for key := range reqs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var rows []DeploymentNodeRequirement
	for _, key := range keys {
		for _, value := range reqs[key] {
			rows = append(rows, DeploymentNodeRequirement{DeploymentID: deploymentID, Key: key, Value: value})
		}
	}
	return rows
}

// templateInferenceConfig is the source declaration a template create may
// initialize on its model. It is not a new persisted fact: the two strings go
// to the existing model columns under the same conditions as a template init.
type templateInferenceConfig struct {
	modelID         uint
	operations      string
	inputModalities string
}

// CreateDeploymentWithRequirements persists a new deployment and its node
// requirements in one transaction, so no reader ever sees the deployment
// without the requirements that bound where it may run. The requirements are
// validated and normalized here (nodelabel.NormalizeRequirements); rules that
// need more than the requirements themselves are the caller's.
func (s *Store) CreateDeploymentWithRequirements(d *Deployment, reqs nodelabel.Requirements) (*Deployment, error) {
	return s.createDeploymentWithRequirements(d, reqs, nil)
}

// CreateDeploymentWithTemplateInferenceConfig also initializes the model's
// template contract in the deployment transaction. A served-name conflict or a
// contract write failure therefore leaves neither a partial deployment nor a
// contract changed by a create that did not happen.
func (s *Store) CreateDeploymentWithTemplateInferenceConfig(d *Deployment, reqs nodelabel.Requirements, modelID uint, operations, inputModalities string) (*Deployment, error) {
	return s.createDeploymentWithRequirements(d, reqs, &templateInferenceConfig{
		modelID: modelID, operations: operations, inputModalities: inputModalities,
	})
}

func (s *Store) createDeploymentWithRequirements(d *Deployment, reqs nodelabel.Requirements, config *templateInferenceConfig) (*Deployment, error) {
	normalized, err := nodelabel.NormalizeRequirements(reqs)
	if err != nil {
		return nil, err
	}
	if d.State == "" {
		d.State = DeployActive
	}
	if d.PlacementStrategy == "" {
		d.PlacementStrategy = PlacementSpread
	}
	modelChanged := false
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := servedNameFree(tx, d.ServedName); err != nil {
			return err
		}
		if err := tx.Create(d).Error; err != nil {
			return err
		}
		rows := requirementRows(d.ID, normalized)
		if len(rows) > 0 {
			if err := tx.Create(&rows).Error; err != nil {
				return err
			}
		}
		if config == nil {
			return nil
		}
		var err error
		modelChanged, err = updateModelInferenceConfig(tx, config.modelID, config.operations, config.inputModalities, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	if modelChanged {
		s.Bus.PublishChange("model", uintKey(config.modelID), "inference_config")
	}
	s.Bus.PublishChange("deployment", uintKey(d.ID), "created")
	return d, nil
}

// ServedNameInUseError refuses a new deployment whose served name another
// deployment already answers under. The gateway routes by served name, so two
// deployments sharing one would silently split or take over each other's
// traffic. Rows that already share a name stay as they are; only a create is
// refused.
type ServedNameInUseError struct {
	ServedName string
	HolderID   uint
	HolderName string
}

func (e *ServedNameInUseError) Error() string {
	return fmt.Sprintf("served name %q is already used by deployment %s (id %d)", e.ServedName, e.HolderName, e.HolderID)
}

// servedNameFree reports whether no deployment holds name. A stopped
// deployment keeps its name, since starting it again routes it under that
// name; one being deleted gives its name up, so a deployment can be
// recreated under the name of the one it replaces.
func servedNameFree(tx *gorm.DB, name string) error {
	if name == "" {
		return nil
	}
	var holder Deployment
	err := tx.Select("id", "name").Where("served_name = ? AND desired_state <> ?", name, string(DeployDeleting)).Order("id").Limit(1).Find(&holder).Error
	if err != nil {
		return err
	}
	if holder.ID != 0 {
		return &ServedNameInUseError{ServedName: name, HolderID: holder.ID, HolderName: holder.Name}
	}
	return nil
}

// SetDeploymentRequirements replaces a deployment's node requirements and its
// placement strategy in one transaction. They bound only replicas launched
// from now on: one already running where the new requirements no longer
// admit stays where it is. The requirements are validated and normalized
// here, as on create; rules that need more than the requirements themselves
// (the vendor the image is for, whether any node could ever fit) are the
// caller's.
func (s *Store) SetDeploymentRequirements(id uint, reqs nodelabel.Requirements, strategy PlacementStrategy) error {
	normalized, err := nodelabel.NormalizeRequirements(reqs)
	if err != nil {
		return err
	}
	if strategy == "" {
		strategy = PlacementSpread
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Deployment{}).Where("id = ?", id).Update("placement_strategy", strategy)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Where("deployment_id = ?", id).Delete(&DeploymentNodeRequirement{}).Error; err != nil {
			return err
		}
		rows := requirementRows(id, normalized)
		if len(rows) == 0 {
			return nil
		}
		return tx.Create(&rows).Error
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("deployment", uintKey(id), DeploymentChangeRequirements)
	return nil
}

// DeploymentChangeRequirements is the change reason published when a
// deployment's node requirements or strategy are replaced.
const DeploymentChangeRequirements = "requirements"

// DeploymentRequirements is a deployment's node requirements as stored. Rows
// are returned as they are, not revalidated: a value that names a deleted node
// or network still constrains the deployment (it matches nothing), and
// dropping it would widen where the deployment may run.
func (s *Store) DeploymentRequirements(id uint) (nodelabel.Requirements, error) {
	all, err := s.DeploymentRequirementsFor([]uint{id})
	if err != nil {
		return nil, err
	}
	return all[id], nil
}

// DeploymentRequirementsFor loads the requirements of several deployments in
// one query, keyed by deployment id. A deployment with none is absent.
func (s *Store) DeploymentRequirementsFor(ids []uint) (map[uint]nodelabel.Requirements, error) {
	out := map[uint]nodelabel.Requirements{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []DeploymentNodeRequirement
	if err := s.reader().Where("deployment_id IN ?", ids).
		Order("deployment_id, key, value").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if out[row.DeploymentID] == nil {
			out[row.DeploymentID] = nodelabel.Requirements{}
		}
		out[row.DeploymentID][row.Key] = append(out[row.DeploymentID][row.Key], row.Value)
	}
	return out, nil
}

// DeploymentsRequiring are the ids of the deployments whose requirements name
// value for key, in id order.
func (s *Store) DeploymentsRequiring(key, value string) ([]uint, error) {
	var ids []uint
	err := s.reader().Model(&DeploymentNodeRequirement{}).
		Where("key = ? AND value = ?", key, value).
		Distinct().Order("deployment_id").Pluck("deployment_id", &ids).Error
	return ids, err
}

// insertRequirementRows adds rows that are not there yet. It is the
// migration's writer: a row already present is the same fact, not a conflict.
func insertRequirementRows(tx *gorm.DB, rows []DeploymentNodeRequirement) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}
