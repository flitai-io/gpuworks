package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInferenceCatalogConflict = errors.New("inference catalog user spec already exists")

// InferenceTemplateTopic is published after a catalog write commits, keyed by
// the template (inference model) row ID, or InferenceTemplatesAll after a
// whole-catalog reconcile. The controller re-resolves the observation of the
// instances created from those templates (ADR 0039).
const (
	InferenceTemplateTopic = "inference_template"
	InferenceTemplatesAll  = "*"
)

var (
	// ErrInferenceSpecNotFound: no user spec with that name on that model.
	ErrInferenceSpecNotFound = errors.New("inference catalog user spec not found")
	// ErrInferenceSpecInUse: a deployment outside "deleting" still pins the
	// spec, so retiring it would break the next launch resolution.
	ErrInferenceSpecInUse = errors.New("inference catalog user spec is still referenced by deployments")
	// ErrInferenceModelReferenced: dropping the model row would leave another
	// model's component reference dangling.
	ErrInferenceModelReferenced = errors.New("inference model is still referenced as a component")
)

// ReconcileInferenceCatalog upserts a complete flat catalog in one
// transaction. The caller supplies each model's specs through Specs; no
// legacy template graph or evidence side tables are consulted.
func (s *Store) ReconcileInferenceCatalog(models []InferenceModel) error {
	return s.ReconcileInferenceCatalogKeeping(models, nil)
}

// InferenceCatalogKeep names an upstream model, or one upstream spec of it
// when Spec is set, that a reconcile leaves as it is stored.
type InferenceCatalogKeep struct {
	ModelID string
	Spec    string
}

// ReconcileInferenceCatalogKeeping is ReconcileInferenceCatalog for a catalog
// the caller could read only in part: the upstream rows named in keep are
// neither rewritten nor pruned, so a spec this server already has stays
// usable when a newer catalog states it in a form the server cannot read.
// Rows that do not exist are not created.
func (s *Store) ReconcileInferenceCatalogKeeping(models []InferenceModel, keep []InferenceCatalogKeep) error {
	if len(models) == 0 {
		return errors.New("inference catalog is empty")
	}
	kept := catalogKeep{}
	for _, item := range keep {
		modelID := strings.ToLower(strings.TrimSpace(item.ModelID))
		if modelID == "" {
			continue
		}
		if kept[modelID] == nil {
			kept[modelID] = map[string]struct{}{}
		}
		if spec := strings.TrimSpace(item.Spec); spec != "" {
			kept[modelID][spec] = struct{}{}
		}
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		return reconcileInferenceCatalog(tx, models, true, kept)
	})
	if err == nil {
		s.Bus.PublishChange(InferenceTemplateTopic, InferenceTemplatesAll, "")
	}
	return err
}

// ImportUserInferenceCatalog merges user-authored specs without pruning
// upstream rows. The editing boundary is the stable spec identity
// (original model, user source, spec name): an import may add another user
// spec for the same engine, while replacing an equally named user spec still
// requires overwrite=true.
func (s *Store) ImportUserInferenceCatalog(models []InferenceModel, overwrite bool) error {
	if len(models) == 0 {
		return errors.New("inference catalog is empty")
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for i := range models {
			models[i].ModelID = strings.ToLower(strings.TrimSpace(models[i].ModelID))
			models[i].Source = "user"
			for specIndex := range models[i].Specs {
				models[i].Specs[specIndex].Source = "user"
				if models[i].Specs[specIndex].PackageDigest == "" {
					models[i].Specs[specIndex].PackageDigest = models[i].PackageDigest
				}
			}
			var existing InferenceModel
			err := tx.Preload("Specs").Where("model_id = ?", models[i].ModelID).First(&existing).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			incomingNames := make(map[string]struct{})
			for _, spec := range models[i].Specs {
				incomingNames[strings.TrimSpace(spec.Name)] = struct{}{}
			}
			for _, spec := range existing.Specs {
				if spec.Source == "user" {
					_, collision := incomingNames[spec.Name]
					if collision && !overwrite {
						return fmt.Errorf("%w: %s/%s", ErrInferenceCatalogConflict, existing.ModelID, spec.Name)
					}
				}
			}
			preserved := make([]InferenceSpecRow, 0, len(existing.Specs)+len(models[i].Specs))
			for _, spec := range existing.Specs {
				_, replacedUserSpec := incomingNames[spec.Name]
				if spec.Source != "user" || !replacedUserSpec {
					spec.ID = 0
					preserved = append(preserved, spec)
				}
			}
			models[i].Specs = append(preserved, models[i].Specs...)
			if existing.Source == "upstream" {
				// A user package may extend an existing original model, but does not
				// take ownership of its canonical metadata or documentation.
				models[i].DisplayTitle = existing.DisplayTitle
				models[i].ParameterCount = existing.ParameterCount
				models[i].Architecture = existing.Architecture
				models[i].ReadmeZHCN = existing.ReadmeZHCN
				models[i].ReadmeEN = existing.ReadmeEN
				models[i].PackageDigest = existing.PackageDigest
				models[i].ContextLength = existing.ContextLength
				models[i].ReleaseDate = existing.ReleaseDate
				models[i].Unsupported = existing.Unsupported
				// The editor writes the whole model back; its estimation facts
				// stay the catalog's too, not whatever a saved copy carried.
				models[i].InputModalities = existing.InputModalities
				models[i].ModelConfig = existing.ModelConfig
				models[i].KVBytesPerToken = existing.KVBytesPerToken
				models[i].Source = "upstream"
			}
		}
		return reconcileInferenceCatalog(tx, models, false, nil)
	})
	if err == nil {
		for _, model := range models {
			s.Bus.PublishChange(InferenceTemplateTopic, strconv.FormatUint(uint64(model.ID), 10), "")
		}
	}
	return err
}

// DeleteUserInferenceSpec retires one user-authored spec. Imports are additive
// (specs absent from an import are preserved), so an explicit delete is the
// only way to remove a user spec. Deleting the last remaining spec of a user
// model drops the model row; upstream rows always keep their canonical specs
// and never reach that branch. Deletion refuses while a deployment outside
// "deleting" still pins the spec, or while another model mounts a
// to-be-dropped model as a component.
func (s *Store) DeleteUserInferenceSpec(modelID, specName string) (modelDeleted bool, err error) {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	specName = strings.TrimSpace(specName)
	if modelID == "" || specName == "" {
		return false, errors.New("model_id and spec are required")
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var model InferenceModel
		if err := tx.Preload("Specs").Where("model_id = ?", modelID).First(&model).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		target := -1
		for i := range model.Specs {
			if model.Specs[i].Source == "user" && model.Specs[i].Name == specName {
				target = i
				break
			}
		}
		if target < 0 {
			return ErrInferenceSpecNotFound
		}
		var pinnedBy []string
		if err := tx.Model(&Deployment{}).
			Where("template_id = ? AND template_spec_source = ? AND template_spec_name = ? AND desired_state <> ?",
				model.ID, "user", specName, string(DeployDeleting)).
			Pluck("name", &pinnedBy).Error; err != nil {
			return err
		}
		if len(pinnedBy) > 0 {
			return fmt.Errorf("%w: %s", ErrInferenceSpecInUse, strings.Join(pinnedBy, ", "))
		}
		if err := tx.Delete(&InferenceSpecRow{}, model.Specs[target].ID).Error; err != nil {
			return err
		}
		if len(model.Specs) > 1 {
			return nil
		}
		if model.Source != "user" {
			return fmt.Errorf("model %s would be left without specs", model.ModelID)
		}
		if err := refuseDanglingComponentReference(tx, model.ID, model.ModelID); err != nil {
			return err
		}
		modelDeleted = true
		return tx.Delete(&InferenceModel{}, model.ID).Error
	})
	return modelDeleted, err
}

// refuseDanglingComponentReference mirrors
// inferencecatalog.validateComponentReferences for the delete path: a model
// row may not be dropped while any other spec still mounts it as a component.
func refuseDanglingComponentReference(tx *gorm.DB, modelRowID uint, modelID string) error {
	var rows []InferenceSpecRow
	if err := tx.Where("components <> '' AND components <> '[]' AND components <> 'null'").Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	var owners []InferenceModel
	if err := tx.Find(&owners).Error; err != nil {
		return err
	}
	ownerIDs := make(map[uint]string, len(owners))
	for _, owner := range owners {
		ownerIDs[owner.ID] = owner.ModelID
	}
	type componentReference struct {
		ModelID string `json:"model_id"`
	}
	for _, row := range rows {
		if row.ModelID == modelRowID {
			continue
		}
		var components []componentReference
		if err := json.Unmarshal([]byte(row.Components), &components); err != nil || len(components) == 0 {
			continue
		}
		for _, component := range components {
			if strings.EqualFold(strings.TrimSpace(component.ModelID), modelID) {
				return fmt.Errorf("%w: %s spec %s mounts %s", ErrInferenceModelReferenced, ownerIDs[row.ModelID], row.Name, modelID)
			}
		}
	}
	return nil
}

// catalogKeep maps a model ID to the upstream spec names a reconcile leaves
// alone. A model the catalog does not carry at all is left alone whole.
type catalogKeep map[string]map[string]struct{}

func reconcileInferenceCatalog(tx *gorm.DB, models []InferenceModel, pruneUpstream bool, keep catalogKeep) error {
	seenModels := make(map[string]struct{}, len(models))
	for i := range models {
		model := &models[i]
		normalizeInferenceModel(model)
		specs := model.Specs
		// GORM otherwise auto-saves the association during Create/Save and
		// the explicit reconciliation below would insert it a second time.
		model.Specs = nil
		if model.ModelID == "" {
			return fmt.Errorf("catalog model %d requires model_id", i)
		}
		if _, duplicate := seenModels[model.ModelID]; duplicate {
			return fmt.Errorf("duplicate catalog model_id %q", model.ModelID)
		}
		seenModels[model.ModelID] = struct{}{}
		if model.Source == "" {
			model.Source = "upstream"
		}
		if model.Source != "upstream" && model.Source != "user" {
			return fmt.Errorf("catalog model %s has invalid source %q", model.ModelID, model.Source)
		}
		var existing InferenceModel
		err := tx.Where("model_id = ?", model.ModelID).First(&existing).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			model.ID = 0
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			model.ID, model.CreatedAt = existing.ID, existing.CreatedAt
			// An explicit column list, so a field added to the entity is not
			// written here until it is named. That is the point - a reconcile
			// must not clobber columns it does not own - but it also means a
			// new catalog field reaches fresh databases (which Create fills
			// wholesale) and silently skips every existing row.
			if err := tx.Model(&existing).Select("display_title", "parameter_count", "architecture", "input_modalities", "model_config", "kv_bytes_per_token", "readme_zh_cn", "readme_en", "package_digest", "context_length", "release_date", "source", "unsupported", "updated_at").Updates(model).Error; err != nil {
				return err
			}
		}
		seenSpecs := make(map[string]struct{}, len(specs))
		targetSource := "user"
		if pruneUpstream {
			targetSource = "upstream"
		}
		seenTargetSpecs := make(map[string]struct{})
		for j := range specs {
			spec := &specs[j]
			if strings.TrimSpace(spec.Source) == "" {
				spec.Source = model.Source
			}
			if strings.TrimSpace(spec.PackageDigest) == "" {
				spec.PackageDigest = model.PackageDigest
			}
			normalizeInferenceSpec(spec)
			spec.ModelID = model.ID
			if spec.Name == "" || spec.Engine == "" || spec.Image == "" || spec.Quantization == "" {
				return fmt.Errorf("model %s spec %d requires name, engine, image, and quantization", model.ModelID, j)
			}
			if spec.Source != "upstream" && spec.Source != "user" {
				return fmt.Errorf("model %s spec %s has invalid source %q", model.ModelID, spec.Name, spec.Source)
			}
			identity := spec.Source + "\x00" + spec.Name
			if _, duplicate := seenSpecs[identity]; duplicate {
				return fmt.Errorf("model %s has duplicate %s spec name %q", model.ModelID, spec.Source, spec.Name)
			}
			seenSpecs[identity] = struct{}{}
			if spec.Source == targetSource {
				seenTargetSpecs[spec.Name] = struct{}{}
			}
			if err := validateCatalogJSON(spec); err != nil {
				return fmt.Errorf("model %s spec %s: %w", model.ModelID, spec.Name, err)
			}
			var existingSpec InferenceSpecRow
			err := tx.Where("model_id = ? AND source = ? AND name = ?", model.ID, spec.Source, spec.Name).First(&existingSpec).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				spec.ID = 0
				if err := tx.Create(spec).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				spec.ID, spec.CreatedAt = existingSpec.ID, existingSpec.CreatedAt
				if err := tx.Save(spec).Error; err != nil {
					return err
				}
			}
		}
		// A catalog import is authoritative for this model's specs. Removing
		// omitted specs prevents old five-layer configurations resurfacing.
		if pruneUpstream {
			for name := range keep[model.ModelID] {
				seenTargetSpecs[name] = struct{}{}
			}
		}
		query := tx.Where("model_id = ? AND source = ?", model.ID, targetSource)
		if len(seenTargetSpecs) > 0 {
			query = query.Where("name NOT IN ?", mapKeys(seenTargetSpecs))
		}
		if err := query.Delete(&InferenceSpecRow{}).Error; err != nil {
			return err
		}
	}
	if pruneUpstream {
		// Upstream imports are authoritative, while user-owned rows are kept
		// for the editor. Remove upstream models omitted by the latest import
		// together with their specs so stale executable configs cannot resurface.
		for modelID := range keep {
			seenModels[modelID] = struct{}{}
		}
		modelKeys := mapKeys(seenModels)
		var stale []InferenceModel
		if err := tx.Where("source = ? AND model_id NOT IN ?", "upstream", modelKeys).Find(&stale).Error; err != nil {
			return err
		}
		for _, item := range stale {
			if err := tx.Where("model_id = ? AND source = ?", item.ID, "upstream").Delete(&InferenceSpecRow{}).Error; err != nil {
				return err
			}
			var remaining int64
			if err := tx.Model(&InferenceSpecRow{}).Where("model_id = ?", item.ID).Count(&remaining).Error; err != nil {
				return err
			}
			if remaining == 0 {
				if err := tx.Delete(&InferenceModel{}, item.ID).Error; err != nil {
					return err
				}
			} else if err := tx.Model(&InferenceModel{}).Where("id = ?", item.ID).Updates(map[string]any{"source": "user", "package_digest": "", "readme_zh_cn": "", "readme_en": ""}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeInferenceModel(model *InferenceModel) {
	model.ModelID = strings.ToLower(strings.TrimSpace(model.ModelID))
	model.DisplayTitle = strings.TrimSpace(model.DisplayTitle)
	model.ParameterCount = strings.TrimSpace(model.ParameterCount)
	model.Architecture = strings.TrimSpace(model.Architecture)
	model.ReadmeZHCN = strings.TrimSpace(model.ReadmeZHCN)
	model.ReadmeEN = strings.TrimSpace(model.ReadmeEN)
	model.PackageDigest = strings.ToLower(strings.TrimSpace(model.PackageDigest))
	model.ReleaseDate = strings.TrimSpace(model.ReleaseDate)
	model.Source = strings.ToLower(strings.TrimSpace(model.Source))
}

func normalizeInferenceSpec(spec *InferenceSpecRow) {
	spec.Source = strings.ToLower(strings.TrimSpace(spec.Source))
	spec.PackageDigest = strings.ToLower(strings.TrimSpace(spec.PackageDigest))
	spec.Name = strings.TrimSpace(spec.Name)
	spec.DisplayNameZHCN = strings.TrimSpace(spec.DisplayNameZHCN)
	spec.DisplayNameEN = strings.TrimSpace(spec.DisplayNameEN)
	spec.Scenario = strings.ToLower(strings.TrimSpace(spec.Scenario))
	spec.Engine = strings.ToLower(strings.TrimSpace(spec.Engine))
	spec.Image = strings.TrimSpace(spec.Image)
	spec.Quantization = strings.ToLower(strings.TrimSpace(spec.Quantization))
}

func validateCatalogJSON(spec *InferenceSpecRow) error {
	for name, value := range map[string]string{
		"weight_sources": spec.WeightSources, "gpus": spec.GPUs,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s JSON is required", name)
		}
		var decoded any
		if err := json.Unmarshal([]byte(value), &decoded); err != nil {
			return fmt.Errorf("%s is invalid JSON: %w", name, err)
		}
	}
	for name, value := range map[string]string{
		"assets": spec.Assets, "entrypoint": spec.Entrypoint, "placement": spec.Placement,
		"api_contract": spec.APIContract, "components": spec.Components, "image_sources": spec.ImageSources,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(value), &decoded); err != nil {
			return fmt.Errorf("%s is invalid JSON: %w", name, err)
		}
	}
	return nil
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func (s *Store) ListInferenceModels() ([]InferenceModel, error) {
	var models []InferenceModel
	if err := s.db.Preload("Specs").Order("model_id").Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// InferenceSpecWeightSources is one spec's weight_sources column and the row
// id of the catalog model the spec belongs to: all that the reverse lookup from
// a weight repository to its catalog model reads.
type InferenceSpecWeightSources struct {
	ModelID       uint
	WeightSources string
}

// ListInferenceSpecWeightSources reads those two columns of every spec and
// nothing else. The lookup runs per candidate node during a deployment preview,
// so it must not load the whole catalog.
func (s *Store) ListInferenceSpecWeightSources() ([]InferenceSpecWeightSources, error) {
	var rows []InferenceSpecWeightSources
	if err := s.db.Model(&InferenceSpecRow{}).Select("model_id", "weight_sources").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) InferenceModelByID(id uint) (*InferenceModel, error) {
	var model InferenceModel
	err := s.db.Preload("Specs").First(&model, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &model, err
}

// InferenceModelKVBytesPerTokenByID reads only the identity and measured KV
// metadata for one catalog model. Launch planning calls it per candidate, so it
// must not preload every spec or scan the catalog.
func (s *Store) InferenceModelKVBytesPerTokenByID(id uint) (modelID, kvBytesPerToken string, err error) {
	if id == 0 {
		return "", "", ErrNotFound
	}
	var row struct {
		ModelID         string
		KVBytesPerToken string
	}
	err = s.db.Model(&InferenceModel{}).
		Select("model_id", "kv_bytes_per_token").
		First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", "", ErrNotFound
	}
	return row.ModelID, row.KVBytesPerToken, err
}

func (s *Store) InferenceModelByModelID(modelID string) (*InferenceModel, error) {
	var model InferenceModel
	err := s.db.Preload("Specs").Where("model_id = ?", strings.ToLower(strings.TrimSpace(modelID))).First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &model, err
}

type DetectedGPU struct {
	Vendor string
	Name   string
	CC     string
	ArchID string
}

func (s *Store) ListDistinctDetectedGPUs() ([]DetectedGPU, error) {
	var rows []DetectedGPU
	err := s.db.Model(&GPU{}).
		Select("lower(trim(vendor)) AS vendor, trim(name) AS name, trim(compute_capability) AS cc, trim(arch_id) AS arch_id").
		Where("trim(name) <> ''").
		Group("lower(trim(vendor)), trim(name), trim(compute_capability), trim(arch_id)").
		Order("vendor, name, cc, arch_id").Scan(&rows).Error
	return rows, err
}

// UpsertInferenceSpec is useful to user-template editors that save one spec
// at a time. JSON columns are validated before the write.
func (s *Store) UpsertInferenceSpec(spec *InferenceSpecRow) error {
	if spec == nil {
		return errors.New("inference spec is nil")
	}
	normalizeInferenceSpec(spec)
	if spec.ModelID == 0 || spec.Name == "" || spec.Engine == "" || spec.Image == "" || spec.Quantization == "" {
		return errors.New("inference spec requires model_id, name, engine, image, and quantization")
	}
	if err := validateCatalogJSON(spec); err != nil {
		return err
	}
	if spec.Source == "" {
		spec.Source = "user"
	}
	if spec.Source != "user" {
		return errors.New("individual inference spec edits must use source=user")
	}
	if err := s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "model_id"}, {Name: "source"}, {Name: "name"}}, UpdateAll: true}).Create(spec).Error; err != nil {
		return err
	}
	s.Bus.PublishChange(InferenceTemplateTopic, strconv.FormatUint(uint64(spec.ModelID), 10), "")
	return nil
}
