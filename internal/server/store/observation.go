package store

import (
	"errors"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The queries behind an instance's observation (ADR 0039): the api contract
// its template's spec declares now, and the in-place rewrite of the
// observation part of running instances' desired state.

// TemplateSpecAPIContract returns the stored api contract (JSON
// *inferenceapi.Contract) of the catalog spec a deployment was created from,
// identified by its template row and spec identity. ErrNotFound when the spec
// no longer exists; the caller then falls back to the deployment's snapshot.
// One indexed row read (idx_inference_spec_identity).
func (s *Store) TemplateSpecAPIContract(templateID uint, source, name string) (string, error) {
	var row InferenceSpecRow
	err := s.db.Select("api_contract").
		Where("model_id = ? AND source = ? AND name = ?", templateID, source, name).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrNotFound
	}
	return row.APIContract, err
}

// ObservedDeployment is a deployment whose instances' observation may need
// refreshing, with those instances.
type ObservedDeployment struct {
	Deployment Deployment
	Instances  []Instance
}

// DeploymentsForObservation lists the deployments created from the given
// template rows (every deployment when templateIDs is nil) that have
// instances, with their instances. It is read when the catalog changes, not
// per reconcile: two indexed queries (deployments.template_id,
// instances.deployment_id), never a per-row lookup.
func (s *Store) DeploymentsForObservation(templateIDs []uint) ([]ObservedDeployment, error) {
	if templateIDs != nil && len(templateIDs) == 0 {
		return nil, nil
	}
	query := s.db.Model(&Deployment{}).Where("desired_state <> ?", DeployDeleting)
	if templateIDs != nil {
		query = query.Where("template_id IN ?", templateIDs)
	}
	var deployments []Deployment
	if err := query.Find(&deployments).Error; err != nil {
		return nil, err
	}
	if len(deployments) == 0 {
		return nil, nil
	}
	ids := make([]uint, len(deployments))
	byID := make(map[uint]int, len(deployments))
	for index, deployment := range deployments {
		ids[index] = deployment.ID
		byID[deployment.ID] = index
	}
	var instances []Instance
	if err := s.db.Where("deployment_id IN ?", ids).Find(&instances).Error; err != nil {
		return nil, err
	}
	grouped := make([][]Instance, len(deployments))
	for _, instance := range instances {
		if index, ok := byID[instance.DeploymentID]; ok {
			grouped[index] = append(grouped[index], instance)
		}
	}
	out := make([]ObservedDeployment, 0, len(deployments))
	for index, deployment := range deployments {
		if len(grouped[index]) > 0 {
			out = append(out, ObservedDeployment{Deployment: deployment, Instances: grouped[index]})
		}
	}
	return out, nil
}

// RewriteObservations applies change to each instance's desired state in one
// transaction, storing only the instances change reports as changed, and
// returns those instances with their new desired state. change must touch the
// observation fields alone: nothing else in a running instance's desired
// state may change in place. No event is
// published: nothing the controller or the gateway decides depends on it, and
// the caller pushes the changed states to their agents itself. The instance
// generation still moves (Bus.Advance) when a row changed: the whole desired
// state is written back, and the gateway trusts its route snapshot only while
// no instance row was written since.
func (s *Store) RewriteObservations(instances []Instance, change func(*Instance, *proto.InstanceDesiredState) bool) ([]Instance, error) {
	var changed []Instance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		changed = changed[:0]
		for index := range instances {
			instance := instances[index]
			before := instance.DesiredStateJSON
			if err := rewriteDesiredState(tx, &instance, func(spec *proto.InstanceDesiredState) bool {
				return change(&instance, spec)
			}); err != nil {
				return err
			}
			if instance.DesiredStateJSON != before {
				changed = append(changed, instance)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(changed) > 0 && s.Bus != nil {
		s.Bus.Advance("instance")
	}
	return changed, nil
}

// TemplateSpecKey names one template spec row: the template (model) it
// belongs to and its source and name.
type TemplateSpecKey struct {
	TemplateID uint
	Source     string
	Name       string
}

// TemplateSpecAPIContracts is TemplateSpecAPIContract for many specs in one
// query (by template id, then filtered), for callers that resolve every
// routed deployment at once. Absent specs are absent from the map.
func (s *Store) TemplateSpecAPIContracts(keys []TemplateSpecKey) (map[TemplateSpecKey]string, error) {
	out := map[TemplateSpecKey]string{}
	if len(keys) == 0 {
		return out, nil
	}
	wanted := make(map[TemplateSpecKey]bool, len(keys))
	ids := make([]uint, 0, len(keys))
	for _, key := range keys {
		if !wanted[key] {
			wanted[key] = true
			ids = append(ids, key.TemplateID)
		}
	}
	var rows []InferenceSpecRow
	if err := s.db.Select("model_id", "source", "name", "api_contract").Where("model_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		key := TemplateSpecKey{TemplateID: row.ModelID, Source: row.Source, Name: row.Name}
		if wanted[key] {
			out[key] = row.APIContract
		}
	}
	return out, nil
}
