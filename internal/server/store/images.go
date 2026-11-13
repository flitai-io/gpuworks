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
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

const (
	ImageStateAbsent  = "absent"
	ImageStatePulling = "pulling"
	ImageStatePresent = "present"
	ImageStateSyncing = "syncing"
	ImageStateFailed  = "failed"
)

// migrateImageRegistryMirrors reads the image_registry_mirrors setting that
// preceded image_registry_rewrites, as the rewrite document it stood for;
// migration 153 turns that into pull source rules.
func migrateImageRegistryMirrors(raw string) string {
	var old struct {
		Default string `json:"default"`
		Sources map[string]struct {
			Prefix string `json:"prefix"`
		} `json:"sources"`
	}
	if json.Unmarshal([]byte(raw), &old) != nil || len(old.Sources) == 0 {
		return ""
	}
	mirrors := make([]string, 0, len(old.Sources))
	for _, key := range []string{"quay", "huaweicloud"} {
		if item, ok := old.Sources[key]; ok && strings.TrimSpace(item.Prefix) != "" {
			mirrors = append(mirrors, strings.TrimSuffix(strings.TrimSpace(item.Prefix), "/"))
		}
	}
	if len(mirrors) == 0 {
		return ""
	}
	b, _ := json.Marshal(map[string]any{"default": old.Default, "rules": []map[string]any{{"prefix": "gpustack/", "mirrors": mirrors}}})
	return string(b)
}

// emptyJSONList reports whether a catalog payload omitted a candidate list.
// Community/custom imports use omission to mean "keep the operator's current
// candidates"; an explicit PATCH is the supported way to clear them.
func emptyJSONList(raw string) bool {
	switch strings.TrimSpace(raw) {
	case "", "null", "[]":
		return true
	default:
		return false
	}
}

const (
	ImagePurposeInference  = "inference"
	ImagePurposeEvaluation = "evaluation"
)

// UpsertImageSeries upserts a series by its stable display name.
func (s *Store) UpsertImageSeries(series *ImageSeries) (*ImageSeries, error) {
	series.Name = strings.TrimSpace(series.Name)
	if series.Name == "" {
		return nil, errors.New("series name is required")
	}
	if series.Purpose == "" {
		series.Purpose = ImagePurposeInference
	}
	if series.Purpose != ImagePurposeInference && series.Purpose != ImagePurposeEvaluation {
		return nil, errors.New("purpose must be inference or evaluation")
	}
	if series.Source == "" {
		series.Source = "community"
	}
	if series.Source != "official" && series.Source != "community" && series.Source != "custom" {
		return nil, errors.New("source must be official, community, or custom")
	}
	var existing ImageSeries
	err := s.db.Where("name = ?", series.Name).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := s.db.Create(series).Error; err != nil {
			return nil, err
		}
		s.Bus.PublishChange("image", series.Name, "series_created")
		return series, nil
	}
	if err != nil {
		return nil, err
	}
	if existing.Source != "" && existing.Source != series.Source {
		return nil, fmt.Errorf("image series %q already belongs to source %q; cannot change it to %q", series.Name, existing.Source, series.Source)
	}
	if existing.Source != "official" {
		if emptyJSONList(series.CommonArgs) {
			series.CommonArgs = existing.CommonArgs
		}
		if emptyJSONList(series.CommonEnv) {
			series.CommonEnv = existing.CommonEnv
		}
	}
	series.ID = existing.ID
	columns := []any{"name", "engine", "purpose", "framework", "common_args", "common_env", "source", "notes", "enabled"}
	if existing.Source == "official" {
		// The catalog owns a system series' description; whether it is offered
		// is the operator's switch, which a catalog import never resets.
		columns = columns[:len(columns)-1]
	}
	if err := s.db.Model(&existing).Select(columns[0], columns[1:]...).Updates(series).Error; err != nil {
		return nil, err
	}
	if err := s.db.First(&existing, existing.ID).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("image", existing.Name, "series_updated")
	return &existing, nil
}

// UpsertImage updates catalog metadata by image_ref while preserving presence
// rows. It is safe to call repeatedly during manifest synchronization.
func (s *Store) UpsertImage(img *Image) (*Image, error) {
	img.ImageRef = strings.TrimSpace(img.ImageRef)
	if err := validateTaggedImageRef(img.ImageRef); err != nil {
		return nil, err
	}
	if img.Source == "" {
		img.Source = "community"
	}
	if img.Source != "official" && img.Source != "community" && img.Source != "custom" {
		return nil, errors.New("source must be official, community, or custom")
	}
	var existingSource Image
	if err := s.db.Select("source").Where("image_ref = ?", img.ImageRef).First(&existingSource).Error; err == nil && existingSource.Source != "" && existingSource.Source != img.Source {
		return nil, fmt.Errorf("image %q already belongs to source %q; cannot change it to %q", img.ImageRef, existingSource.Source, img.Source)
	}
	if img.SeriesID == 0 {
		name := strings.TrimSpace(img.Name)
		if img.SeriesName != "" {
			name = strings.TrimSpace(img.SeriesName)
		}
		if name == "" {
			name = deriveImageSeriesName(img.ImageRef)
		}
		if img.SeriesName == "" && (img.Source == "community" || img.Source == "custom") {
			var resolveErr error
			name, resolveErr = resolveImageSeriesName(s.db, name, img.Framework)
			if resolveErr != nil {
				return nil, resolveErr
			}
		}
		purpose := img.Purpose
		if purpose == "" {
			purpose = ImagePurposeInference
		}
		series, err := s.UpsertImageSeries(&ImageSeries{Name: name, Engine: img.Engine, Purpose: purpose, Framework: img.Framework, Source: img.Source, Enabled: true})
		if err != nil {
			return nil, err
		}
		img.SeriesID = series.ID
	}
	var existing Image
	err := s.db.Where("image_ref = ?", img.ImageRef).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := s.db.Create(img).Error; err != nil {
			return nil, err
		}
		s.Bus.PublishChange("image", img.ImageRef, "created")
		return img, nil
	}
	if err != nil {
		return nil, err
	}
	if existing.Source != "" && existing.Source != img.Source {
		return nil, fmt.Errorf("image %q already belongs to source %q; cannot change it to %q", img.ImageRef, existing.Source, img.Source)
	}
	img.ID = existing.ID
	columns := []any{"series_id", "name", "engine", "engine_version", "framework", "framework_version", "variant", "arch", "source", "deprecated", "deprecated_since", "size_bytes", "notes", "offline_blob_sha256", "registry_sources", "rolling_tag", "upstream_gone", "enabled"}
	if existing.Source == "official" {
		// The catalog owns a system row's description and registry
		// references. Two things on it belong to this cluster and survive every
		// catalog import: the operator's enabled switch, and the archive
		// addresses an operator added (the catalog cannot know about them, and
		// dropping one would silently break every node that still fetches the
		// image from there).
		columns = columns[:len(columns)-1]
		img.RegistrySources = preserveClusterArchiveSources(img.RegistrySources, existing.RegistrySources)
	}
	if err := s.db.Model(&existing).Select(columns[0], columns[1:]...).Updates(img).Error; err != nil {
		return nil, err
	}
	if err := s.db.First(&existing, existing.ID).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("image", existing.ImageRef, "updated")
	return &existing, nil
}

// deriveImageSeriesName groups manually entered images by repository rather
// than by engine. This keeps community/custom repositories such as
// lmsysorg/sglang-rocm separate from one another and lets the same engine have
// multiple independently editable image families.
func deriveImageSeriesName(ref string) string {
	name := strings.TrimSpace(ref)
	lastSlash := strings.LastIndex(name, "/")
	if colon := strings.LastIndex(name, ":"); colon > lastSlash {
		name = name[:colon]
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 {
		host := strings.ToLower(parts[0])
		if strings.Contains(host, ".") || strings.Contains(host, ":") || host == "localhost" {
			parts = parts[1:]
		}
	}
	name = strings.TrimSpace(strings.Join(parts, "/"))
	if name == "" {
		return "custom-image"
	}
	return name
}

// resolveImageSeriesName keeps automatically grouped community/custom rows
// distinct when one repository is used for more than one framework. Explicit
// SeriesName values remain operator-owned and are resolved by the API layer.
func resolveImageSeriesName(db *gorm.DB, base, framework string) (string, error) {
	base = strings.TrimSpace(base)
	framework = strings.ToLower(strings.TrimSpace(framework))
	if base == "" || framework == "" {
		return base, nil
	}
	var existing ImageSeries
	err := db.Where("name = ?", base).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return base, nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(existing.Framework) == "" || strings.EqualFold(existing.Framework, framework) {
		return base, nil
	}
	root := base + " · " + framework
	name := root
	for suffix := 2; ; suffix++ {
		var candidate ImageSeries
		lookupErr := db.Where("name = ?", name).First(&candidate).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return name, nil
		}
		if lookupErr != nil {
			return "", lookupErr
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Framework), framework) {
			return name, nil
		}
		name = fmt.Sprintf("%s %d", root, suffix)
	}
}

func validateTaggedImageRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("image_ref is required")
	}
	if strings.Contains(strings.ToLower(ref), "@sha256:") {
		return errors.New("digest image refs are not supported")
	}
	if name := ref[strings.LastIndex(ref, "/")+1:]; !strings.Contains(name, ":") {
		return errors.New("image_ref must include an explicit tag")
	}
	return nil
}

func (s *Store) ListImages() ([]Image, error) {
	var images []Image
	err := s.db.Order("deprecated asc, name asc, id asc").Find(&images).Error
	return images, err
}

// ListImageSeries returns enabled and disabled series in stable display order.
func (s *Store) ListImageSeries() ([]ImageSeries, error) {
	var rows []ImageSeries
	err := s.db.Order("enabled desc, name asc, id asc").Find(&rows).Error
	return rows, err
}

func (s *Store) GetImageSeries(id uint) (*ImageSeries, error) {
	var row ImageSeries
	err := s.db.First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

func (s *Store) ImageSeriesByName(name string) (*ImageSeries, error) {
	var row ImageSeries
	err := s.db.Where("name = ?", strings.TrimSpace(name)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

func (s *Store) VersionsForSeries(seriesID uint) ([]Image, error) {
	var rows []Image
	err := s.db.Where("series_id = ?", seriesID).Order("deprecated asc, framework_version desc, engine_version desc, id asc").Find(&rows).Error
	return rows, err
}

func (s *Store) ListEnabledImages() ([]Image, error) {
	var images []Image
	err := s.db.Table("image_versions").Joins("JOIN image_series ON image_series.id = image_versions.series_id").Where("image_versions.enabled = ? AND image_versions.deprecated = ? AND image_series.enabled = ?", true, false, true).Order("image_versions.name asc, image_versions.id asc").Find(&images).Error
	return images, err
}

func (s *Store) GetImage(id uint) (*Image, error) {
	var image Image
	err := s.db.First(&image, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &image, err
}

func deletableImageSource(source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	return source == "custom" || source == "community"
}

// DeleteImage deletes one custom/community version, its node distribution
// rows, and the series shell when this was its last version. A version that
// anything references (ImageReferenceCounts) is refused, by the same rule that
// keeps a referenced system image deprecated rather than deleted.
func (s *Store) DeleteImage(id uint) error {
	image, err := s.GetImage(id)
	if err != nil {
		return err
	}
	if !deletableImageSource(image.Source) {
		return errors.New("official image versions cannot be deleted")
	}
	counts, err := s.ImageReferenceCounts([]string{image.ImageRef})
	if err != nil {
		return err
	}
	if inUse := counts[image.ImageRef]; inUse > 0 {
		return fmt.Errorf("%w: image is referenced %d time(s) by deployments, instances, or models", ErrReferenced, inUse)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("image_version_id = ?", id).Delete(&ImageNode{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&Image{}, id).Error; err != nil {
			return err
		}
		var remaining int64
		if err := tx.Model(&Image{}).Where("series_id = ?", image.SeriesID).Count(&remaining).Error; err != nil {
			return err
		}
		if remaining == 0 {
			if err := tx.Delete(&ImageSeries{}, image.SeriesID).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	s.Bus.PublishChange("image", image.ImageRef, "deleted")
	return nil
}

// DeleteImageSeries deletes a custom/community series and all of its versions
// after proving that no version is referenced.
func (s *Store) DeleteImageSeries(id uint) error {
	series, err := s.GetImageSeries(id)
	if err != nil {
		return err
	}
	if !deletableImageSource(series.Source) {
		return errors.New("official image series cannot be deleted")
	}
	versions, err := s.VersionsForSeries(id)
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(versions))
	ids := make([]uint, 0, len(versions))
	for _, image := range versions {
		refs = append(refs, image.ImageRef)
		ids = append(ids, image.ID)
	}
	counts, err := s.ImageReferenceCounts(refs)
	if err != nil {
		return err
	}
	inUse := 0
	for _, n := range counts {
		inUse += n
	}
	if inUse > 0 {
		return fmt.Errorf("%w: image series is referenced %d time(s) by deployments, instances, or models", ErrReferenced, inUse)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if len(ids) > 0 {
			if err := tx.Where("image_version_id IN ?", ids).Delete(&ImageNode{}).Error; err != nil {
				return err
			}
			if err := tx.Where("series_id = ?", id).Delete(&Image{}).Error; err != nil {
				return err
			}
		}
		result := tx.Delete(&ImageSeries{}, id)
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
	s.Bus.PublishChange("image", series.Name, "series_deleted")
	return nil
}

func (s *Store) SetImageSeriesEnabled(id uint, enabled bool) (*ImageSeries, error) {
	result := s.db.Model(&ImageSeries{}).Where("id = ?", id).Update("enabled", enabled)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	row, err := s.GetImageSeries(id)
	if err == nil {
		s.Bus.PublishChange("image", row.Name, "series_enabled")
	}
	return row, err
}

// UpdateImageSeriesCommon updates the operator-owned candidate chips without
// changing series identity, source, or enabled state.
func (s *Store) UpdateImageSeriesCommon(id uint, commonArgs, commonEnv string) (*ImageSeries, error) {
	result := s.db.Model(&ImageSeries{}).Where("id = ?", id).Updates(map[string]any{
		"common_args": commonArgs,
		"common_env":  commonEnv,
	})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	row, err := s.GetImageSeries(id)
	if err == nil {
		s.Bus.PublishChange("image", row.Name, "series_common_updated")
	}
	return row, err
}

func (s *Store) SetImageEnabled(id uint, enabled bool) (*Image, error) {
	result := s.db.Model(&Image{}).Where("id = ?", id).Update("enabled", enabled)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	row, err := s.GetImage(id)
	if err == nil {
		s.Bus.PublishChange("image", row.ImageRef, "enabled")
	}
	return row, err
}

// UpdateImageRegistrySources replaces one version's acquisition addresses and
// nothing else. Every other field stays catalog-owned for an official image,
// but which addresses this cluster can fetch an image from is
// deployment data the catalog cannot know: an official version whose registry
// is unreachable here still has to be fetchable from an HTTP archive.
func (s *Store) UpdateImageRegistrySources(id uint, sources string) (*Image, error) {
	var image Image
	if err := s.db.First(&image, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if err := s.db.Model(&image).Update("registry_sources", sources).Error; err != nil {
		return nil, err
	}
	if err := s.db.First(&image, id).Error; err != nil {
		return nil, err
	}
	s.Bus.PublishChange("image", image.ImageRef, "runspec_updated")
	return &image, nil
}

// preserveClusterArchiveSources keeps the archive addresses an operator added
// when a catalog row is written again: the catalog's own entries stay as they
// are, and every archive address already on the row that the catalog does not
// mention is appended. Registry references the catalog dropped are dropped —
// they are catalog data, and the catalog is the authority for them.
func preserveClusterArchiveSources(catalog, existing string) string {
	var catalogEntries, existingEntries []string
	if json.Unmarshal([]byte(catalog), &catalogEntries) != nil {
		catalogEntries = nil
	}
	if json.Unmarshal([]byte(existing), &existingEntries) != nil {
		existingEntries = nil
	}
	seen := make(map[string]struct{}, len(catalogEntries)+len(existingEntries))
	merged := make([]string, 0, len(catalogEntries)+len(existingEntries))
	for _, entry := range catalogEntries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, trimmed)
	}
	for _, entry := range existingEntries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" || !pullref.IsArchiveURL(trimmed) {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, trimmed)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return catalog
	}
	return string(encoded)
}

func (s *Store) ImageByRef(ref string) (*Image, error) {
	var image Image
	err := s.db.Where("image_ref = ?", strings.TrimSpace(ref)).First(&image).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &image, err
}

func (s *Store) UpsertImageNode(row *ImageNode) (*ImageNode, error) {
	if row.ImageVersionID == 0 {
		row.ImageVersionID = row.ImageID
	}
	// The insert and the row-matching are one statement keyed on the unique
	// (image_version_id, node_id) index: a node status report creating the same
	// row concurrently must not surface as a constraint error to the caller.
	// A caller-supplied ID can be stale by the time this runs, so it is not
	// part of the conflict resolution.
	row.ID = 0
	err := s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "image_version_id"}, {Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"state", "size_bytes", "runtime_size_bytes", "transferred_bytes", "rate_bps",
			"transfer_id", "source_node_id", "source", "message", "rate_limit_kib", "transfer_error_code", "updated_at",
		}),
	}).Create(row).Error
	if err != nil {
		return nil, err
	}
	var existing ImageNode
	if err := s.db.Where("image_version_id = ? AND node_id = ?", row.ImageVersionID, row.NodeID).First(&existing).Error; err != nil {
		return nil, err
	}
	existing.ImageID = existing.ImageVersionID
	s.Bus.PublishChange("image", uintKey(existing.ImageVersionID), "node")
	return &existing, nil
}

func (s *Store) FailImageSyncIfActive(imageID, nodeID uint, transferID, message string) error {
	return s.FailImageSyncIfActiveCode(imageID, nodeID, transferID, message, "")
}

// FailImageNodeIfUnchanged fails a transfer that stopped reporting.
//
// A pull or a URL fetch runs on the node, so the server only knows it is alive
// through the statuses the agent sends. When those stop — the agent died, the
// node lost the network, or a runtime hung inside its own CLI — the row would
// otherwise stay in progress forever, with a progress bar frozen at whatever
// it last reported and no way for the operator to retry. The update is
// conditional on the row still being in progress and on the timestamp the
// caller saw, so a status that arrives between the decision and this write wins.
func (s *Store) FailImageNodeIfUnchanged(imageID, nodeID uint, transferID string, observed time.Time, message string) (bool, error) {
	res := s.db.Model(&ImageNode{}).
		Where("image_version_id = ? AND node_id = ? AND transfer_id = ? AND state IN ? AND updated_at <= ?", imageID, nodeID, transferID, []string{ImageStatePulling, ImageStateSyncing}, observed).
		Updates(map[string]any{"state": ImageStateFailed, "last_transfer_id": gorm.Expr("CASE WHEN COALESCE(transfer_id, '') <> '' THEN transfer_id ELSE last_transfer_id END"), "transfer_id": "", "rate_bps": int64(0), "last_rate_limit_kib": gorm.Expr("CASE WHEN rate_limit_kib > 0 THEN rate_limit_kib ELSE last_rate_limit_kib END"), "rate_limit_kib": int64(0), "message": message})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, nil
}

// MarkImageNodeAbsent records that a node no longer holds the image, clearing
// the progress a previous transfer left behind: a row that says "absent" must
// not still carry the bytes of the attempt that put it there.
func (s *Store) MarkImageNodeAbsent(imageID, nodeID uint, message string) error {
	res := s.db.Model(&ImageNode{}).
		Where("image_version_id = ? AND node_id = ?", imageID, nodeID).
		Updates(map[string]any{
			"state": ImageStateAbsent, "size_bytes": int64(0), "runtime_size_bytes": nil,
			"transferred_bytes": int64(0), "rate_bps": int64(0),
			"last_transfer_id": gorm.Expr("CASE WHEN COALESCE(transfer_id, '') <> '' THEN transfer_id ELSE last_transfer_id END"), "transfer_id": "", "message": message,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.Bus.PublishChange("image", uintKey(imageID), "node")
	return nil
}

// CancelImageNodeTransfer ends one node's image transfer on the server side.
//
// Unlike the watchdog this does not look at timestamps: an operator asking for
// a stop is not a guess about liveness, so the row becomes retryable whatever
// it last reported.
func (s *Store) CancelImageNodeTransfer(imageID, nodeID uint, message string) (bool, error) {
	res := s.db.Model(&ImageNode{}).
		Where("image_version_id = ? AND node_id = ? AND state IN ?", imageID, nodeID, []string{ImageStatePulling, ImageStateSyncing}).
		Updates(map[string]any{"state": ImageStateFailed, "last_transfer_id": gorm.Expr("CASE WHEN COALESCE(transfer_id, '') <> '' THEN transfer_id ELSE last_transfer_id END"), "transfer_id": "", "rate_bps": int64(0), "last_rate_limit_kib": gorm.Expr("CASE WHEN rate_limit_kib > 0 THEN rate_limit_kib ELSE last_rate_limit_kib END"), "rate_limit_kib": int64(0), "message": message})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		s.Bus.PublishChange("image", uintKey(imageID), "node")
	}
	return res.RowsAffected > 0, nil
}

func (s *Store) ImageNodesForImage(imageID uint) ([]ImageNode, error) {
	var rows []ImageNode
	err := s.db.Where("image_version_id = ?", imageID).Find(&rows).Error
	for i := range rows {
		rows[i].ImageID = rows[i].ImageVersionID
	}
	return rows, err
}

// ImageHolders lists, in id order, the nodes other than except that hold an
// image. It reads node ids only, on the read pool: an automatic copy asks it
// on every launch of a node that lacks the image, and a fleet that holds it
// would otherwise be read row by row each time.
func (s *Store) ImageHolders(imageID, except uint) ([]uint, error) {
	var ids []uint
	err := s.reader().Model(&ImageNode{}).
		Where("image_version_id = ? AND state = ? AND node_id <> ?", imageID, ImageStatePresent, except).
		Order("node_id").Pluck("node_id", &ids).Error
	return ids, err
}

// ImageNodesForImages returns the node rows of several versions in batched IN
// queries, so a catalog page reads its presence once rather than per version.
func (s *Store) ImageNodesForImages(imageIDs []uint) ([]ImageNode, error) {
	var out []ImageNode
	for start := 0; start < len(imageIDs); start += imageReferenceBatch {
		var rows []ImageNode
		batch := imageIDs[start:min(start+imageReferenceBatch, len(imageIDs))]
		if err := s.db.Where("image_version_id IN ?", batch).Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	for i := range out {
		out[i].ImageID = out[i].ImageVersionID
	}
	return out, nil
}

func (s *Store) ImageNodesForNode(nodeID uint) ([]ImageNode, error) {
	var rows []ImageNode
	err := s.db.Where("node_id = ?", nodeID).Find(&rows).Error
	for i := range rows {
		rows[i].ImageID = rows[i].ImageVersionID
	}
	return rows, err
}

func (s *Store) ImageNode(imageID, nodeID uint) (*ImageNode, error) {
	var row ImageNode
	err := s.db.Where("image_version_id = ? AND node_id = ?", imageID, nodeID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	row.ImageID = row.ImageVersionID
	return &row, err
}

func (s *Store) ApplyImageNodeStatus(imageID, nodeID uint, st proto.ImageNodeStatus) error {
	var current ImageNode
	err := s.db.Where("image_version_id = ? AND node_id = ?", imageID, nodeID).First(&current).Error
	if err != nil {
		return err
	}
	if st.TransferID != "" && current.TransferID != "" && st.TransferID != current.TransferID {
		return nil
	}
	if st.TransferID != "" && current.TransferID == "" && current.LastTransferID != "" && st.TransferID != current.LastTransferID {
		return nil
	}
	// A finished row has no active transfer id. A progress frame that still
	// names a transfer arrived after that transfer ended and must not reopen it.
	if st.TransferID != "" && current.TransferID == "" &&
		(current.State == ImageStatePresent || current.State == ImageStateFailed) &&
		st.State != ImageStatePresent && st.State != ImageStateFailed {
		return nil
	}
	if current.TransferErrorCode == "PEER_RELAY_BUSY" && st.State != ImageStatePresent {
		return nil
	}
	updates := map[string]any{"state": st.State, "message": st.Message, "runtime_size_bytes": nil}
	if st.ImageID != "" {
		updates["image_digest_id"] = st.ImageID
	}
	if st.SizeBytes > 0 {
		updates["size_bytes"] = st.SizeBytes
	}
	if st.TransferredBytes > 0 {
		updates["transferred_bytes"] = st.TransferredBytes
	}
	if st.RateBPS > 0 {
		updates["rate_bps"] = st.RateBPS
	}
	if st.Source != "" {
		updates["source"] = st.Source
	}
	if current.State == ImageStatePulling && st.State == ImageStatePresent {
		// Registry layer counters are partial, not the size of a saved archive.
		// Leave archive bytes unknown for later peer copies; runtime size is
		// measured independently by the node's inventory.
		updates["size_bytes"], updates["transferred_bytes"] = int64(0), int64(0)
	}
	if st.State == ImageStatePresent {
		updates["transfer_error_code"] = ""
	}
	if st.State == ImageStatePresent || st.State == ImageStateFailed {
		updates["transfer_id"] = ""
		completedID := st.TransferID
		if completedID == "" {
			completedID = current.TransferID
		}
		if completedID != "" {
			updates["last_transfer_id"] = completedID
		}
		updates["rate_bps"] = int64(0)
		if current.RateLimitKiB > 0 || (current.State == ImageStateSyncing && current.Source == "peer") {
			updates["last_rate_limit_kib"] = current.RateLimitKiB
		}
		updates["rate_limit_kib"] = int64(0)
	} else if st.TransferID != "" {
		updates["transfer_id"] = st.TransferID
	}
	res := s.db.Model(&ImageNode{}).Where("id = ? AND state = ? AND COALESCE(transfer_id, '') = ? AND transfer_error_code = ? AND last_transfer_id = ?", current.ID, current.State, current.TransferID, current.TransferErrorCode, current.LastTransferID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	// A rolling tag resolves to a concrete runtime image only after pull or
	// inspect. Fill snapshots created before that observation, without
	// rewriting an existing instance snapshot on later refreshes.
	if st.ImageID != "" {
		var image Image
		if err := s.db.First(&image, imageID).Error; err != nil {
			return err
		}
		// A finished attempt never ran this image; only instances still
		// on their way up or running take the observation.
		if err := s.db.Model(&Instance{}).
			Where("node_id = ? AND image = ? AND (image_digest_id IS NULL OR image_digest_id = '')", nodeID, image.ImageRef).
			Where("state NOT IN ?", append(proto.InstanceFinishedStates(), proto.InstRemoving)).
			Update("image_digest_id", st.ImageID).Error; err != nil {
			return err
		}
	}
	// An image transfer reports its byte counters once a second. While the
	// state has not moved, that is progress: it cannot make a deployment
	// runnable, so it must not re-plan every deployment in the cluster. The
	// existing "node" reason keeps meaning "presence on a node changed", which
	// is what suppresses the catalog-refresh feedback loop.
	reason := "node"
	if current.State == st.State && (st.State == ImageStatePulling || st.State == ImageStateSyncing) {
		reason = bus.ReasonProgress
	}
	s.Bus.PublishChange("image", uintKey(imageID), reason)
	return nil
}

func (s *Store) ImageNodesForImageWithNodes(imageID uint) ([]ImageNode, error) {
	return s.ImageNodesForImage(imageID)
}
