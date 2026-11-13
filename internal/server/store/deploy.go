package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// ---- models ----

func normalizeModelIdentity(m *Model) {
	m.Source = strings.ToLower(strings.TrimSpace(m.Source))
	m.SourceID = strings.TrimSpace(m.SourceID)
	m.Revision = strings.TrimSpace(m.Revision)
	if subfolder, err := modelrepo.NormalizeSubfolder(m.ArtifactSubfolder); err == nil {
		m.ArtifactSubfolder = subfolder
	}
}

func modelIdentityQuery(db *gorm.DB, m *Model) *gorm.DB {
	return db.Where("lower(source) = ? AND lower(source_id) = ? AND revision = ? AND artifact_subfolder = ?", strings.ToLower(m.Source), strings.ToLower(m.SourceID), m.Revision, m.ArtifactSubfolder)
}

// SetModelWeightBytes records the artifact size a catalog spec supplied for a
// model row that had none. The weight total is an acquisition fact the model
// row owns, and every later estimate reads it from there; the caller that knows
// both the row and the spec is the one that writes it, so the estimator itself
// stays a reader.
func (s *Store) SetModelWeightBytes(modelID uint, weightBytes int64) error {
	if modelID == 0 || weightBytes <= 0 {
		return nil
	}
	return s.db.Model(&Model{}).Where("id = ?", modelID).Update("weight_bytes", weightBytes).Error
}

// UpsertModel finds-or-creates a model by immutable artifact identity.
func (s *Store) UpsertModel(m *Model) (*Model, error) {
	normalizeModelIdentity(m)
	if _, err := modelrepo.NormalizeSubfolder(m.ArtifactSubfolder); err != nil {
		return nil, err
	}
	var existing Model
	err := modelIdentityQuery(s.db, m).First(&existing).Error
	if err == nil {
		// Update mutable metadata.
		if err := s.db.Model(&existing).Updates(map[string]any{
			"name": m.Name, "arch": m.Arch, "quant_label": m.QuantLabel,
			"weight_bytes": m.WeightBytes, "dtype": m.Dtype, "meta": m.Meta,
			"image_id": m.ImageID, "readme": m.README, "logo_path": m.LogoPath,
			"origin_created_at": m.OriginCreatedAt, "origin_updated_at": m.OriginUpdatedAt,
			"context_length": m.ContextLength, "precision": m.Precision,
		}).Error; err != nil {
			return nil, err
		}
		if err := s.db.First(&existing, existing.ID).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	// Another registration can insert the same identity after the lookup.
	// Keep its metadata and reuse the committed artifact instead of surfacing
	// a unique-index error or overwriting the winner's fields.
	return s.FindOrCreateModel(m)
}

// FindOrCreateModel registers an immutable artifact without changing metadata
// that an earlier registration already committed. Unlike UpsertModel, this
// remains read-only for the winning row under every concurrent lookup timing.
func (s *Store) FindOrCreateModel(m *Model) (*Model, error) {
	normalizeModelIdentity(m)
	if _, err := modelrepo.NormalizeSubfolder(m.ArtifactSubfolder); err != nil {
		return nil, err
	}
	result := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(m)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		var winner Model
		if err := modelIdentityQuery(s.db, m).First(&winner).Error; err != nil {
			return nil, err
		}
		return &winner, nil
	}
	return m, nil
}

// RegisterLocalModel creates or updates a local model and its registered file
// atomically. The filesystem path is never moved or copied.
func (s *Store) RegisterLocalModel(m *Model, ref FileRef, path string, sizeBytes int64) (*Model, *ModelFile, error) {
	normalizeModelIdentity(m)
	ref = s.resolveRef(ref)
	storageID, nodeID := ref.StorageID, ref.NodeID
	var out Model
	var mf ModelFile
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing Model
		err := modelIdentityQuery(tx, m).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(m).Error; err != nil {
				return err
			}
			out = *m
		} else if err != nil {
			return err
		} else {
			m.ID = existing.ID
			if err := tx.Model(&existing).Updates(map[string]any{
				"name": m.Name, "arch": m.Arch, "quant_label": m.QuantLabel,
				"weight_bytes": m.WeightBytes, "dtype": m.Dtype, "meta": m.Meta, "image_id": m.ImageID,
				"readme": m.README, "logo_path": m.LogoPath, "origin_created_at": m.OriginCreatedAt,
				"origin_updated_at": m.OriginUpdatedAt, "context_length": m.ContextLength, "precision": m.Precision,
			}).Error; err != nil {
				return err
			}
			out = existing
			if err := tx.First(&out, out.ID).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("model_id = ? AND storage_id = ? AND node_id = ?", out.ID, storageID, nodeID).First(&mf).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			mf = ModelFile{ModelID: out.ID, StorageID: storageID, NodeID: nodeID, Path: path, State: proto.MFRegistered, Acquisition: proto.ModelAcquireRegistered, SizeBytes: sizeBytes, Downloaded: sizeBytes}
			if err := tx.Create(&mf).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if err := tx.Model(&mf).Updates(map[string]any{"path": path, "state": proto.MFRegistered, "acquisition": proto.ModelAcquireRegistered, "size_bytes": sizeBytes, "downloaded": sizeBytes, "message": ""}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	s.Bus.PublishChange("model", uintKey(out.ID), "local_registered")
	s.publishModelFile(mf.ID, proto.MFRegistered)
	return &out, &mf, nil
}

// GetModel loads a model by id.
func (s *Store) GetModel(id uint) (*Model, error) {
	var m Model
	if err := s.db.First(&m, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return &m, nil
}

// SetModelInferenceConfig persists the user-editable inference contract. The
// API layer validates and normalizes the JSON before calling this method; the
// store deliberately treats both arrays as opaque JSON values.
func (s *Store) SetModelInferenceConfig(id uint, operations, inputModalities string) (*Model, error) {
	result := s.db.Model(&Model{}).Where("id = ?", id).Updates(map[string]any{
		"operations": operations, "input_modalities": inputModalities,
	})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	s.Bus.PublishChange("model", uintKey(id), "inference_config")
	return s.GetModel(id)
}

// ListModels returns models newest-first, optionally limited to repository IDs.
func (s *Store) ListModels(sourceIDs ...string) ([]Model, error) {
	query := s.db
	if len(sourceIDs) > 0 {
		ids := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			ids[i] = strings.ToLower(strings.TrimSpace(id))
		}
		query = query.Where("lower(source_id) IN ?", ids)
	}
	var ms []Model
	err := query.Order("id desc").Find(&ms).Error
	return ms, err
}

// FindModelBySourceID returns an already registered source for a model. When
// both registries contain the model, preferredSource wins deterministically.
func (s *Store) FindModelBySourceID(sourceID, preferredSource string) (*Model, error) {
	sourceID = strings.ToLower(strings.TrimSpace(sourceID))
	preferredSource = strings.ToLower(strings.TrimSpace(preferredSource))
	var model Model
	if preferredSource != "" {
		err := s.db.Where("lower(source_id) = ? AND lower(source) = ?", sourceID, preferredSource).Order("id asc").First(&model).Error
		if err == nil {
			return &model, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	err := s.db.Where("lower(source_id) = ?", sourceID).Order("id asc").First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &model, nil
}

// FindModelBySourceRevision resolves one immutable catalog weight source.
// Unlike FindModelBySourceID it never falls back to another provider or
// revision, because a deployment snapshot must preserve the selected source.
func (s *Store) FindModelBySourceRevision(sourceID, source, revision string) (*Model, error) {
	return s.FindModelArtifact(sourceID, source, revision, "")
}

// FindModelArtifact resolves one immutable repository artifact. Subfolder is
// part of identity because a complete repository and one sparsely materialized
// repository subtree have different bytes, cache contents and scheduling metadata.
func (s *Store) FindModelArtifact(sourceID, source, revision, subfolder string) (*Model, error) {
	identity := Model{Source: source, SourceID: sourceID, Revision: revision, ArtifactSubfolder: subfolder}
	normalizeModelIdentity(&identity)
	if _, err := modelrepo.NormalizeSubfolder(identity.ArtifactSubfolder); err != nil {
		return nil, err
	}
	var model Model
	err := modelIdentityQuery(s.db, &identity).First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &model, nil
}

// DeleteModel removes an unused model and its derived node/profile rows. A
// deployment keeps the model definition authoritative, even after it stops.
func (s *Store) DeleteModel(id uint) error {
	var model Model
	if err := s.db.First(&model, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var deployments int64
	if err := s.db.Model(&Deployment{}).Where("model_id = ?", id).Count(&deployments).Error; err != nil {
		return err
	}
	if deployments > 0 {
		return fmt.Errorf("%w: model is used by %d deployment(s)", ErrReferenced, deployments)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := checkNoCacheDeletion(tx.Where("model_id = ?", id)); err != nil {
			return err
		}
		if err := tx.Where("model_id = ?", id).Delete(&ModelFile{}).Error; err != nil {
			return err
		}
		if err := tx.Where("model_id = ?", id).Delete(&Profile{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&Model{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	}); err != nil {
		return err
	}
	s.Bus.PublishChange("model", uintKey(id), "deleted")
	return nil
}

// ---- deployments ----

// CreateDeployment persists a new deployment (desired state) that states no
// node requirements.
func (s *Store) CreateDeployment(d *Deployment) (*Deployment, error) {
	return s.CreateDeploymentWithRequirements(d, nil)
}

// GetDeployment loads a deployment by id.
func (s *Store) GetDeployment(id uint) (*Deployment, error) {
	var d Deployment
	if err := s.db.First(&d, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// ListDeployments returns all deployments newest-first.
func (s *Store) ListDeployments() ([]Deployment, error) {
	var ds []Deployment
	err := s.reader().Order("id desc").Find(&ds).Error
	return ds, err
}

// DeploymentByServedName finds the newest active deployment by its served
// model name. Callers that need to route traffic across overlapping rollout
// attempts must use DeploymentsByServedName and select a ready instance.
func (s *Store) DeploymentByServedName(name string) (*Deployment, error) {
	var d Deployment
	err := s.db.Where("served_name = ? AND desired_state = ?", name, "active").Order("id desc").First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &d, err
}

// DeploymentsByServedName returns active rollout candidates newest-first. A
// served name is intentionally not a database unique key: a replacement may
// be starting while the previous deployment is still serving traffic.
func (s *Store) DeploymentsByServedName(name string) ([]Deployment, error) {
	var deployments []Deployment
	err := s.db.Where("served_name = ? AND desired_state = ?", name, "active").Order("id desc").Find(&deployments).Error
	return deployments, err
}

func (s *Store) SetDeploymentRoutingPolicy(id uint, policy string) error {
	err := s.db.Model(&Deployment{}).Where("id = ?", id).Update("routing_policy", policy).Error
	if err == nil {
		s.Bus.PublishChange("deployment", uintKey(id), "routing")
	}
	return err
}

// A deployment's health check threshold: how long a check waits for its
// reply, in seconds.
const (
	HealthCheckThresholdSecLo      = 1
	HealthCheckThresholdSecHi      = 600
	DefaultHealthCheckThresholdSec = 60
)

// ValidHealthCheckThresholdSec reports whether seconds is a threshold a
// deployment may state.
func ValidHealthCheckThresholdSec(seconds int) bool {
	return seconds >= HealthCheckThresholdSecLo && seconds <= HealthCheckThresholdSecHi
}

// SetDeploymentHealthCheck turns a deployment's health check on or off and
// sets how long a check waits.
func (s *Store) SetDeploymentHealthCheck(id uint, enabled bool, thresholdSec int) error {
	err := s.db.Model(&Deployment{}).Where("id = ?", id).Updates(map[string]any{"health_check": enabled, "health_check_threshold_sec": thresholdSec}).Error
	if err == nil {
		s.Bus.PublishChange("deployment", uintKey(id), "health_check")
	}
	return err
}

// SetDeploymentTokenize opens or closes the gateway's tokenize routes for a
// deployment. The change reaches the gateway's route table through the
// published change; nothing restarts.
func (s *Store) SetDeploymentTokenize(id uint, enabled bool) error {
	err := s.db.Model(&Deployment{}).Where("id = ?", id).Update("tokenize_disabled", !enabled).Error
	if err == nil {
		s.Bus.PublishChange("deployment", uintKey(id), "tokenize")
	}
	return err
}

// HealthCheckedDeployments are the deployments with the health check on,
// with the columns a check needs.
func (s *Store) HealthCheckedDeployments() ([]Deployment, error) {
	var out []Deployment
	err := s.db.Select("id", "model_id", "served_name", "desired_state", "health_check", "health_check_threshold_sec").
		Where("health_check = ?", true).Order("id").Find(&out).Error
	return out, err
}

// SetDeploymentPlacement replaces a deployment's placement and node list,
// the two columns a PD deployment's per-role scaling changes (ADR 0028).
func (s *Store) SetDeploymentPlacement(id uint, placement, nodeIDs string, nodeID uint) error {
	err := s.db.Model(&Deployment{}).Where("id = ?", id).Updates(map[string]any{"placement": placement, "node_ids": nodeIDs, "node_id": nodeID}).Error
	if err == nil {
		s.Bus.PublishChange("deployment", uintKey(id), "placement")
	}
	return err
}

// UpdateInstanceDesiredState replaces the serialized desired state for an
// instance. The physical column keeps its historical name; callers only use
// the domain name and never treat it as another kind of Spec. The event tells
// the route table to refresh because desired state carries serving identity
// and eligibility fields used by the proxy candidate snapshot.
func (s *Store) UpdateInstanceDesiredState(uuid, desiredStateJSON string) error {
	err := s.db.Model(&Instance{}).Where("uuid = ?", uuid).Update("spec", desiredStateJSON).Error
	if err == nil {
		s.Bus.PublishChange("instance", uuid, "desired-state")
	}
	return err
}

// SetDeploymentState flips active/stopped/deleting. Taking the typed state
// keeps new string literals from reaching the column. Leaving active ends a
// rolling restart in progress: its instances are removed anyway. A deletion
// cannot be withdrawn by a later state write (ErrStateChanged).
func (s *Store) SetDeploymentState(id uint, state DeploymentState) error {
	updates := map[string]any{"desired_state": string(state)}
	if state != DeployActive {
		updates["restart_requested_at"] = nil
	}
	query := s.db.Model(&Deployment{}).Where("id = ?", id)
	if state != DeployDeleting {
		query = query.Where("desired_state <> ?", string(DeployDeleting))
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		if _, err := s.GetDeployment(id); err != nil {
			return err
		}
		return ErrStateChanged
	}
	s.Bus.PublishChange("deployment", uintKey(id), "state:"+string(state))
	return nil
}

// DeleteDeployment removes a deployment after its instances have been torn
// down by the controller. Instance events are deployment-owned history and are
// removed together with the deployment; its names stay behind in
// DeletedDeployment for the usage and statistics it leaves.
func (s *Store) DeleteDeployment(id uint) error {
	var deletedInstances, scopedKeys []string
	var removals []InstanceEvent
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var dep Deployment
		if err := tx.Select("ID", "Name", "ServedName").First(&dep, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&DeletedDeployment{
			ID: dep.ID, Name: dep.Name, ServedName: dep.ServedName, DeletedAt: time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		var insts []Instance
		if err := tx.Where("deployment_id = ?", id).Find(&insts).Error; err != nil {
			return err
		}
		for _, inst := range insts {
			deletedInstances = append(deletedInstances, inst.UUID)
			removed, err := deleteInstanceRecords(tx, &inst)
			if err != nil {
				return err
			}
			removals = append(removals, removed...)
		}
		if err := tx.Where("deployment_id = ?", id).Delete(&DeploymentNodeRequirement{}).Error; err != nil {
			return err
		}
		if err := tx.Where("deployment_id = ?", id).Delete(&LifecycleSchedule{}).Error; err != nil {
			return err
		}
		var err error
		if scopedKeys, err = dropDeploymentGrants(tx, id); err != nil {
			return err
		}
		result := tx.Delete(&Deployment{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
	for _, prefix := range scopedKeys {
		s.apiKeys.forget(prefix)
	}
	if err == nil {
		for _, ev := range removals {
			s.observe(ev, nil)
		}
		for _, uuid := range deletedInstances {
			s.Bus.PublishChange("instance", uuid, "deleted")
		}
		s.Bus.PublishChange("deployment", uintKey(id), "deleted")
	}
	return err
}

// UpdateDeploymentLaunchConfig rewrites the three columns that together are a
// deployment's launch configuration: the complete typed configuration, the
// user's residual arguments, and the catalog selection snapshot whose own
// arguments are the template's residual. They are written in one transaction
// because a row holding a new configuration next to an old snapshot would
// describe a launch nothing produced.
//
// The values are computed by the deploy package; store deliberately knows only
// that these three columns move together.
func (s *Store) UpdateDeploymentLaunchConfig(id uint, quickParamsJSON, extraArgs, snapshotJSON string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Deployment{}).Where("id = ?", id).Updates(map[string]any{
			"quick_params": quickParamsJSON,
			"extra_args":   extraArgs,
			// The snapshot's physical column keeps its historical name.
			"template_snapshot": snapshotJSON,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err == nil {
		s.Bus.PublishChange("deployment", uintKey(id), "launch_config")
	}
	return err
}

// ---- instances ----

const (
	instanceEventCreated       = "INSTANCE_CREATED"
	instanceEventStateChanged  = "INSTANCE_STATE_CHANGED"
	instanceEventStatusUpdated = "INSTANCE_STATUS_UPDATED"
	instanceEventSleepResult   = "INSTANCE_SLEEP_RESULT"
	instanceEventRemoved       = "INSTANCE_REMOVED"
)

// Event kinds carried in InstanceEvent.Kind. The schema reserved `schedule`
// from the start; it had no writer until scheduling outcomes were recorded, so
// a deployment that could never be placed left no trace the API could reach.
const (
	EventKindSchedule = "schedule"
)

// Scheduling event codes. Stable and locale-independent: the human-readable
// text is the client's job (internal/server/messages).
const (
	// EventDeploymentUnschedulable: the scheduler ran and produced no placement.
	// Params carry the scheduler's own blocked reason, verbatim.
	EventDeploymentUnschedulable = "DEPLOYMENT_UNSCHEDULABLE"
	// EventUnitRetryExhausted: a unit spent its retry budget and the
	// controller stopped rebuilding it (ADR 0029). Recorded once, on the
	// unit's newest failed attempt. Params: attempts, reason.
	EventUnitRetryExhausted = "UNIT_RETRY_EXHAUSTED"
	// EventUnitRetried: an operator gave a unit its retry budget back.
	// Params: role, unit, attempts.
	EventUnitRetried = "UNIT_RETRIED"
	// EventReplicaRebuilt: the controller started a replica again after its
	// previous attempt failed, within the unit's retry budget (ADR 0029).
	// Recorded once per rebuild, so at most the budget per unit. Params:
	// role (PD only), replica (1-based), attempt, reason (the failed attempt's
	// message, shortened), node (the leader's node name).
	EventReplicaRebuilt = "REPLICA_REBUILT"
)

// CreateInstance persists a scheduled instance with its desired spec.
func (s *Store) CreateInstance(i *Instance) (*Instance, error) {
	var created InstanceEvent
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = s.createInstance(tx, i)
		return err
	}); err != nil {
		return nil, err
	}
	s.observe(created, nil)
	s.Bus.PublishChange("instance", i.UUID, "created")
	return i, nil
}

// CreateInstanceInTx persists a scheduled instance and its timeline row using
// the caller's transaction. It deliberately does not publish a bus event;
// admission dispatches that event only after the surrounding transaction
// commits.
func (s *Store) CreateInstanceInTx(tx *gorm.DB, i *Instance) error {
	if tx == nil {
		return errors.New("nil transaction")
	}
	_, err := s.createInstance(tx, i)
	return err
}

// createInstance writes the instance and its INSTANCE_CREATED event, which it
// returns for the caller to observe once the transaction commits.
func (s *Store) createInstance(tx *gorm.DB, i *Instance) (InstanceEvent, error) {
	if i == nil {
		return InstanceEvent{}, errors.New("nil instance")
	}
	if i.State == "" {
		i.State = proto.InstPending
	}
	if err := stampDeploymentSleep(tx, i); err != nil {
		return InstanceEvent{}, err
	}
	if err := tx.Create(i).Error; err != nil {
		return InstanceEvent{}, err
	}
	ev := InstanceEvent{InstanceID: i.ID, InstanceUUID: i.UUID, DeploymentID: i.DeploymentID}
	if err := addEventTo(tx, &ev, "state", instanceEventCreated, map[string]any{"state": i.State}); err != nil {
		return InstanceEvent{}, err
	}
	return ev, nil
}

// InstanceByUUID loads an instance. Reads through the read pool (see
// Store.reader): most callers are pure lookups (API detail views, the
// metrics-ingest deployment-label resolver), and the pre-write reads inside
// this package's own instance-mutating methods are safe against it too —
// under WAL a fresh read always observes every write already committed on
// the writer connection, so routing this off the single writer connection
// only removes read/write queuing, never a correctness guarantee.
func (s *Store) InstanceByUUID(uuid string) (*Instance, error) {
	var i Instance
	err := s.reader().Where("uuid = ?", uuid).First(&i).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &i, err
}

// InstancesForNode lists a node's non-stopped instances.
func (s *Store) InstancesForNode(nodeID uint) ([]Instance, error) {
	var is []Instance
	err := s.db.Where("node_id = ?", nodeID).Find(&is).Error
	return is, err
}

// InstancesForDeployment lists a deployment's instances.
func (s *Store) InstancesForDeployment(depID uint) ([]Instance, error) {
	var is []Instance
	err := s.db.Where("deployment_id = ?", depID).Order("id desc").Find(&is).Error
	return is, err
}

// ListInstances returns all instances.
func (s *Store) ListInstances() ([]Instance, error) {
	var is []Instance
	err := s.reader().Order("id desc").Find(&is).Error
	return is, err
}

// RunningInstances returns instances currently running or sleeping (for the
// per-GPU allocation ledger, derived not stored).
func (s *Store) RunningInstances() ([]Instance, error) {
	var is []Instance
	err := s.db.Where("state IN ?", []string{proto.InstRunning, proto.InstStarting}).Find(&is).Error
	return is, err
}

// UpdateInstanceStatus applies an agent status.instance report.
func (s *Store) UpdateInstanceStatus(st proto.InstanceStatus) error {
	inst, err := s.InstanceByUUID(st.UUID)
	if err != nil {
		return err
	}
	return s.updateInstanceStatus(inst, st)
}

// UpdateInstanceStatusFromNode applies a report only when its authenticated
// session node owns the instance. The hub separately keeps the current-session
// check atomic with this call.
func (s *Store) UpdateInstanceStatusFromNode(nodeID uint, st proto.InstanceStatus) error {
	inst, err := s.InstanceByUUID(st.UUID)
	if err != nil {
		return err
	}
	if nodeID == 0 || inst.NodeID != nodeID {
		return ErrNotFound
	}
	return s.updateInstanceStatus(inst, st)
}

func (s *Store) updateInstanceStatus(inst *Instance, st proto.InstanceStatus) error {
	updates := map[string]any{}
	if st.State != "" {
		updates["state"] = st.State
		updates["container_id"] = st.ContainerID
		updates["message"] = st.Message
		updates["runtime_failure"] = ""
		if st.RuntimeFailure != nil {
			updates["runtime_failure"] = string(mustJSON(st.RuntimeFailure))
		}
		updates["phase"] = st.Phase
		updates["progress"] = st.Progress
		updates["bytes_done"] = st.BytesDone
		updates["bytes_total"] = st.BytesTotal
		updates["rate_bps"] = st.RateBPS
		updates["direct_addr"] = st.DirectAddr
		// A running report is the agent's authoritative health result, including
		// the probed sleep level after a reconnect. Progress reports carry no
		// trustworthy level and must leave the observed value alone.
		if st.State == proto.InstRunning || st.Phase == proto.PhaseSleep {
			updates["sleep_level"] = st.SleepLevel
		}
		// A sleep the agent measured replaces the last measurement; a report
		// without one (a later probe, an older agent) keeps it.
		if st.SleepLevel > 0 && st.SleepResidueBytes > 0 {
			updates["sleep_residue_bytes"] = st.SleepResidueBytes
		}
		// A new process holds no host buffer yet; once an L1 sleep pinned one,
		// it stays until the process goes. An adopting agent (after its own
		// restart) does not know and reports none, which must not clear it:
		// only a report of a process starting does.
		switch {
		case st.State == proto.InstPending || st.State == proto.InstPulling || st.State == proto.InstStarting:
			updates["l1_host_buffer_held"] = false
		case st.L1HostBufferHeld && (st.State == proto.InstRunning || st.Phase == proto.PhaseSleep):
			updates["l1_host_buffer_held"] = true
		}
	}
	if len(st.Ports) > 0 {
		updates["ports"] = jsonStr(st.Ports)
	}
	if len(st.Measured) > 0 {
		updates["measured"] = measuredAfterReport(inst, st)
	}
	if st.OpenAPIPaths != nil {
		served := false
		for _, path := range *st.OpenAPIPaths {
			if path == "/v1/audio/transcriptions" {
				served = true
				break
			}
		}
		updates["transcription_served"] = served
	}
	if st.RuntimeObservation != nil {
		observation, err := json.Marshal(st.RuntimeObservation)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		updates["runtime_observation"] = string(observation)
		updates["runtime_observation_received_at"] = now
		// The image the container was created from is what the replica runs,
		// whichever registry it came from and whether or not the catalog
		// knows it: comparing replicas reads this. Only a content digest
		// identifies it: nerdctl reports the image's name in that field, which
		// every replica of a deployment shares whatever it runs.
		if imageID := strings.TrimSpace(st.RuntimeObservation.Container.ImageID); strings.HasPrefix(imageID, "sha256:") {
			updates["image_digest_id"] = imageID
		}
	}
	if len(updates) == 0 {
		return nil
	}
	// A failed sleep transition keeps its full GPU claim until the agent
	// confirms the engine is gone (the ledger cannot tell how much it still
	// holds). The failed row itself stays - it is a spent attempt of its unit -
	// so the confirmation is recorded by ending its sleep phase, which is what
	// releases the claim.
	if inst.State == proto.InstFailed && inst.Phase == proto.PhaseSleep && st.State == proto.InstStopped {
		res := s.db.Model(&Instance{}).Where("id = ? AND state = ? AND phase = ?", inst.ID, proto.InstFailed, proto.PhaseSleep).Update("phase", "")
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			s.Bus.PublishChange("instance", inst.UUID, "status")
			s.Bus.PublishChange("deployment", uintKey(inst.DeploymentID), "status")
		}
		return nil
	}
	prev, prevPhase, prevSleep := inst.State, inst.Phase, inst.SleepLevel
	sleepLevelReported := st.State == proto.InstRunning || st.Phase == proto.PhaseSleep
	sleepLevelChanged := sleepLevelReported && st.State == proto.InstRunning && inst.SleepLevel != st.SleepLevel
	if inst.State == proto.InstRemoving && st.State != proto.InstRemoving && st.State != proto.InstStopped {
		return nil
	}
	// A draining instance stays draining whatever its engine reports until it
	// is gone: a crash while it finishes its requests must not read as a failed
	// attempt of the replica that already has a replacement.
	if inst.State == proto.InstDraining && st.State != proto.InstStopped {
		return nil
	}
	// Agent reports can be in flight while the controller retires an instance.
	// Once the store contains a terminal/server-owned state, a delayed pulling,
	// running, or teardown-stopped report must not resurrect or downgrade that
	// generation. The state predicate is deliberately part of the UPDATE (not
	// just the InstanceByUUID read above), so the controller wins even when its
	// SetInstanceState races between this method's read and write.
	blockedStates := proto.InstanceFinishedStates()
	if st.State != proto.InstStopped && st.State != proto.InstRemoving {
		blockedStates = append(blockedStates, proto.InstRemoving)
	}
	if st.State != proto.InstStopped {
		blockedStates = append(blockedStates, proto.InstDraining)
	}
	var rowsAffected int64
	rolledBack := false
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		updateQuery := tx.Model(&Instance{}).Where("id = ? AND state NOT IN ?", inst.ID, blockedStates)
		if st.State == proto.InstStopped && (inst.State == proto.InstFailed || inst.State == proto.InstPendingRemoval) {
			var dep Deployment
			if depErr := tx.First(&dep, inst.DeploymentID).Error; depErr == nil && (dep.State == DeployStopped || dep.State == DeployDeleting) {
				updateQuery = tx.Model(&Instance{}).Where("id = ? AND state IN ?", inst.ID, []string{inst.State, proto.InstRemoving})
			}
		}
		result := updateQuery.Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		rowsAffected = result.RowsAffected
		if rowsAffected == 0 {
			return nil
		}
		var err error
		rolledBack, err = rollbackSleepIntent(tx, inst, st)
		if err != nil {
			return err
		}
		if rolledBack {
			return addEventTo(tx, &InstanceEvent{InstanceID: inst.ID, InstanceUUID: inst.UUID, DeploymentID: inst.DeploymentID}, "sleep", instanceEventSleepResult, map[string]any{"result": "failed_rollback", "target": *st.SleepTarget, "actual": st.SleepLevel, "detail": st.Message})
		}
		return nil
	}); err != nil {
		return err
	}
	if rowsAffected == 0 {
		return nil
	}
	if sleepLevelChanged {
		s.AddInstanceEventCode(inst, "sleep", instanceEventSleepResult, map[string]any{"result": "applied", "level": st.SleepLevel})
	}
	if rolledBack {
		s.Bus.PublishChange("deployment", uintKey(inst.DeploymentID), "sleep-rollback")
	}
	// An event is a milestone, not a sample. A pull emits thousands of reports
	// whose only difference is the message text and the byte counters; those
	// belong in the columns just updated above, which are overwritable. Only a
	// change of state or phase appends a row, and that row carries the summary
	// of the phase it ends, so the timeline says more with twenty rows than the
	// old one said with seventy thousand.
	if st.State != "" && (prev != st.State || prevPhase != st.Phase) {
		params := map[string]any{
			"state": st.State, "phase": st.Phase, "detail": st.Message,
			"progress": st.Progress, "bytes_done": st.BytesDone, "bytes_total": st.BytesTotal, "rate_bps": st.RateBPS,
		}
		s.addPhaseSummary(params, inst, prev, prevPhase)
		s.AddInstanceEventCode(inst, "state", instanceEventStatusUpdated, params)
		if prev != st.State && (st.State == proto.InstRunning || st.State == proto.InstStopped) {
			s.touchInstanceModelFiles(inst)
		}
		s.Bus.PublishChange("instance", st.UUID, st.State)
		return nil
	}
	if st.State != "" && st.SleepLevel != prevSleep {
		s.Bus.PublishChange("instance", st.UUID, "sleep")
		return nil
	}
	s.Bus.PublishChange("instance", st.UUID, bus.ReasonProgress)
	return nil
}

// addPhaseSummary folds the phase that just ended into the row that ends it.
//
// inst holds the instance as it was before this report was applied, so its
// counters are the last values the finished phase reached. The duration is
// measured from the event that opened that phase, which is the previous
// milestone row - there is no per-phase timestamp column and none is needed.
func (s *Store) addPhaseSummary(params map[string]any, inst *Instance, prevState, prevPhase string) {
	if inst == nil || (prevState == "" && prevPhase == "") {
		return
	}
	params["prev_state"], params["prev_phase"] = prevState, prevPhase
	params["prev_bytes_done"], params["prev_bytes_total"] = inst.BytesDone, inst.BytesTotal
	var last InstanceEvent
	err := s.db.Where("instance_id = ?", inst.ID).Order("id desc").First(&last).Error
	if err != nil || last.CreatedAt.IsZero() {
		return
	}
	elapsed := time.Since(last.CreatedAt.UTC())
	if elapsed < 0 {
		return
	}
	params["prev_duration_ms"] = elapsed.Milliseconds()
	if seconds := elapsed.Seconds(); seconds > 0 && inst.BytesDone > 0 {
		params["prev_rate_bps"] = int64(float64(inst.BytesDone) / seconds)
	}
}

// SetInstanceState updates just the state (server-driven transitions).
func (s *Store) SetInstanceState(uuid, state, message string) error {
	inst, err := s.InstanceByUUID(uuid)
	if err != nil {
		return err
	}
	if err := s.db.Model(&Instance{}).Where("id = ?", inst.ID).
		Updates(map[string]any{"state": state, "message": message, "runtime_failure": ""}).Error; err != nil {
		return err
	}
	s.AddInstanceEventCode(inst, "state", instanceEventStateChanged, map[string]any{"state": state, "detail": message})
	s.Bus.PublishChange("instance", uuid, state)
	return nil
}

// DeleteInstance removes an instance (after the agent confirms teardown).
func (s *Store) DeleteInstance(uuid string) error {
	inst, err := s.InstanceByUUID(uuid)
	if err != nil {
		return err
	}
	var removals []InstanceEvent
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		removals, err = deleteInstanceRecords(tx, inst)
		return err
	}); err != nil {
		return err
	}
	for _, ev := range removals {
		s.observe(ev, nil)
	}
	s.Bus.PublishChange("instance", uuid, "deleted")
	return nil
}

// deleteInstanceRecords removes an instance row and its per-instance payloads.
//
// Instance events are deliberately NOT deleted: they are append-only history,
// not current state. Deleting a deployment says something about the live
// configuration, not about whether the failure it recorded ever happened - and
// the log store already keeps its copy regardless, so erasing the events left
// the logs of a failure without the events that explain it. Retention bounds
// the table instead (see RetentionSweep).
//
// It returns the INSTANCE_REMOVED event it wrote, if any, for the caller to
// observe once the transaction commits.
func deleteInstanceRecords(tx *gorm.DB, inst *Instance) ([]InstanceEvent, error) {
	if err := tx.Where("instance_id = ?", inst.ID).Delete(&InstanceModelFile{}).Error; err != nil {
		return nil, err
	}
	removed, err := recordInstanceRemoved(tx, inst)
	if err != nil {
		return nil, err
	}
	return removed, tx.Delete(&Instance{}, inst.ID).Error
}

// AddInstanceEvent appends a timeline entry.
func (s *Store) AddInstanceEvent(instanceID uint, kind, message, data string) {
	_ = s.db.Create(&InstanceEvent{
		InstanceID: instanceID, Kind: kind, Message: message, Data: data,
		CreatedAt: time.Now().UTC(),
	}).Error
}

// AddInstanceEventCode appends a locale-independent timeline event. Params are
// stored as JSON in Data so old rows and old clients retain their Message/Data
// contract without a bulk migration.
//
// The instance's UUID and deployment are copied onto the row. The row outlives
// the instance, so it has to be readable without joining back to a table that
// may no longer have the answer.
func (s *Store) AddInstanceEventCode(inst *Instance, kind, code string, params map[string]any) {
	if inst == nil {
		return
	}
	s.addEvent(&InstanceEvent{
		InstanceID: inst.ID, InstanceUUID: inst.UUID, DeploymentID: inst.DeploymentID,
	}, kind, code, params)
}

// AddDeploymentEventCode appends a timeline entry for a deployment that has no
// instance yet. Scheduling outcomes are decided before any instance exists,
// which is exactly the case this exists for - without it, a deployment that
// could never be placed left no trace anywhere the API could reach.
func (s *Store) AddDeploymentEventCode(deploymentID uint, kind, code string, params map[string]any) {
	if deploymentID == 0 {
		return
	}
	s.addEvent(&InstanceEvent{DeploymentID: deploymentID}, kind, code, params)
}

func (s *Store) addEvent(ev *InstanceEvent, kind, code string, params map[string]any) {
	params, cards := s.schedulingPlacement(code, ev.InstanceUUID, params)
	if addEventTo(s.db, ev, kind, code, params) == nil {
		s.observe(*ev, cards)
	}
}

func addEventTo(tx *gorm.DB, ev *InstanceEvent, kind, code string, params map[string]any) error {
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ev.Kind = kind
	ev.Code = strings.TrimSpace(code)
	ev.Data = string(data)
	ev.CreatedAt = time.Now().UTC()
	return tx.Create(ev).Error
}

// InstanceEvents returns an instance's timeline (oldest first).
func (s *Store) InstanceEvents(instanceID uint, limit int) ([]InstanceEvent, error) {
	var evs []InstanceEvent
	q := s.db.Where("instance_id = ?", instanceID).Order("id asc")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&evs).Error
	return evs, err
}

// InstanceEventsByUUID returns an instance's raw timeline, oldest first,
// addressed by the same UUID the API uses. Unlike InstanceEvents it does not
// need the instance row to still exist, which is the point: the history of a
// deleted deployment stays readable.
//
// The API reads InstanceTimeline instead: unfolded, oldest-first rows are what
// made a busy instance's timeline unreadable. This stays for callers that need
// every row exactly as written.
func (s *Store) InstanceEventsByUUID(uuid string, limit int) ([]InstanceEvent, error) {
	if uuid == "" {
		return nil, nil
	}
	var evs []InstanceEvent
	q := s.db.Where("instance_uuid = ?", uuid).Order("id asc")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&evs).Error
	return evs, err
}

// DeploymentEvents returns a deployment's whole timeline, including entries
// recorded before any instance existed (oldest first).
func (s *Store) DeploymentEvents(deploymentID uint, limit int) ([]InstanceEvent, error) {
	if deploymentID == 0 {
		return nil, nil
	}
	var evs []InstanceEvent
	q := s.db.Where("deployment_id = ?", deploymentID).Order("id asc")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&evs).Error
	return evs, err
}

// LatestScheduleEvent returns the most recent scheduling entry for a deployment.
// `found` is false when the deployment has never been reported unschedulable.
// Callers use it to pace re-recording; for "how long has this been blocked",
// use FirstScheduleEvent - a rolling window of entries never ages past one
// window, so the latest entry cannot answer that.
func (s *Store) LatestScheduleEvent(deploymentID uint) (InstanceEvent, bool, error) {
	return s.scheduleEvent(deploymentID, "id desc")
}

// FirstScheduleEvent returns the earliest scheduling entry for a deployment -
// the moment it was first reported unschedulable.
func (s *Store) FirstScheduleEvent(deploymentID uint) (InstanceEvent, bool, error) {
	return s.scheduleEvent(deploymentID, "id asc")
}

func (s *Store) scheduleEvent(deploymentID uint, order string) (InstanceEvent, bool, error) {
	if deploymentID == 0 {
		return InstanceEvent{}, false, nil
	}
	var ev InstanceEvent
	err := s.db.Where("deployment_id = ? AND kind = ?", deploymentID, EventKindSchedule).
		Order(order).First(&ev).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return InstanceEvent{}, false, nil
	}
	return ev, err == nil, err
}

// DeleteEventsBefore trims the event table. Time and byte thresholds both apply
// and whichever trips first wins: a burst of failures can outgrow a byte budget
// long before it is old, and an old backlog can outlive its usefulness long
// before it is large.
func (s *Store) DeleteEventsBefore(cutoff time.Time, maxBytes int64) (int64, error) {
	var deleted int64
	if !cutoff.IsZero() {
		res := s.db.Where("created_at < ?", cutoff.UTC()).Delete(&InstanceEvent{})
		if res.Error != nil {
			return deleted, res.Error
		}
		deleted += res.RowsAffected
	}
	if maxBytes <= 0 {
		return deleted, nil
	}
	// SQLite has no cheap per-table byte counter, so the byte budget is enforced
	// by trimming oldest-first until the estimated size fits. SUM over the text
	// columns is the estimate; it ignores row overhead, which is irrelevant at
	// the granularity this budget is set at (tens of gigabytes).
	var total int64
	if err := s.db.Model(&InstanceEvent{}).
		Select("COALESCE(SUM(LENGTH(message) + LENGTH(data) + LENGTH(code)), 0)").
		Scan(&total).Error; err != nil {
		return deleted, err
	}
	for total > maxBytes {
		var batch []InstanceEvent
		if err := s.db.Order("id asc").Limit(500).Find(&batch).Error; err != nil {
			return deleted, err
		}
		if len(batch) == 0 {
			break
		}
		ids := make([]uint, 0, len(batch))
		var freed int64
		for i := range batch {
			ids = append(ids, batch[i].ID)
			freed += int64(len(batch[i].Message) + len(batch[i].Data) + len(batch[i].Code))
		}
		res := s.db.Where("id IN ?", ids).Delete(&InstanceEvent{})
		if res.Error != nil {
			return deleted, res.Error
		}
		deleted += res.RowsAffected
		total -= freed
	}
	return deleted, nil
}

// ---- desired-state derivation (per node) ----

// DesiredStateForNode builds the full desired state pushed to a node's agent.
// Derived from instances (spec JSON) and model files — no separate desired-state
// table to drift.
func (s *Store) DesiredStateForNode(nodeID uint) (proto.DesiredState, error) {
	var ds proto.DesiredState
	ds.Storages = []proto.StorageTask{}
	if node, err := s.NodeByID(nodeID); err == nil {
		ds.ModelsRoot = s.LegacyModelsRoot(node)
		ds.RuntimePreference = node.RuntimePreference
		ds.AcceleratorPreference = node.AcceleratorPreference
		storages, err := s.StorageTasksForNode(node)
		if err != nil {
			return ds, err
		}
		ds.Storages = storages
	}
	insts, err := s.InstancesForNode(nodeID)
	if err != nil {
		return ds, err
	}
	for _, in := range insts {
		if in.State == proto.InstRemoving {
			// A failed or partially created instance may have no serialized launch
			// spec. The UUID alone is enough for the agent to remove its container.
			spec := proto.InstanceDesiredState{UUID: in.UUID, DesiredState: "stopped"}
			if in.DesiredStateJSON != "" {
				_ = json.Unmarshal([]byte(in.DesiredStateJSON), &spec)
				spec.UUID = in.UUID
				spec.DesiredState = "stopped"
			}
			ds.Instances = append(ds.Instances, spec)
			continue
		}
		if in.DesiredStateJSON == "" {
			continue
		}
		// Failed/stopped instances are not desired: excluding them prevents an
		// agent reconnect from reviving crashed containers (the controller owns
		// retry decisions, not the desired-state replay).
		if in.State == proto.InstFailed || in.State == proto.InstStopped || in.State == proto.InstPendingRemoval {
			continue
		}
		var spec proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(in.DesiredStateJSON), &spec); err != nil {
			continue
		}
		// Where the node pulls the image from is planned by the hub from the
		// pull source rules as they are when the state is sent
		// (hub.planReplayImages); the instance records only what names the
		// image.
		ds.Instances = append(ds.Instances, spec)
	}
	tasks, err := s.ModelFileTasksForNode(nodeID)
	if err != nil {
		return ds, err
	}
	ds.ModelFiles = tasks
	var runs []BenchmarkRun
	nodeUUID := ""
	if n, e := s.NodeByID(nodeID); e == nil {
		nodeUUID = n.UUID
	}
	if nodeUUID != "" {
		_ = s.db.Where("load_origin = ? AND state IN ?", nodeUUID, []string{"pending", "running"}).Find(&runs).Error
		for _, run := range runs {
			apiKey, _ := DecryptAIKey(s.secretbox, run.APIKey)
			ds.Benchmarks = append(ds.Benchmarks, proto.BenchmarkSpec{ID: run.ID, DesiredState: "running", TargetURL: run.BaseURL, APIKey: apiKey, Model: run.Model, Mode: run.Mode, Dataset: json.RawMessage(run.Dataset), Search: json.RawMessage(run.Search), TokenizerRef: run.TokenizerRef, KeepDetail: run.KeepDetail, DetailCapMB: run.DetailCapMB})
		}
	}
	// The node's proxy is planned by the hub from the pull settings as they
	// are when the state is sent (planning.ProxyFor).
	auths, err := s.SourceAuths()
	if err != nil {
		return ds, err
	}
	ds.RegistryAuth = auths
	// A missing or malformed row keeps the sweep disabled; only an explicit
	// true opts the node's agents into removing foreign gpuworks-named
	// containers.
	_ = json.Unmarshal([]byte(strings.TrimSpace(s.GetSetting(SettingOrphanCleanup))), &ds.OrphanCleanup)
	if images, err := s.ListEnabledImages(); err == nil {
		for _, image := range images {
			ds.ImageRefs = append(ds.ImageRefs, image.ImageRef)
		}
	}
	return ds, nil
}

// ---- helpers ----

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func uintKey(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}
