package store

import (
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// likeEscaper escapes the SQL LIKE wildcards (and the escape character) in a
// literal searched for with ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ResolveImageRef preserves exact catalog identity precedence, then resolves
// a declared registry source. Lowest ID breaks ambiguous alias ties.
func (s *Store) ResolveImageRef(ref string) (*Image, error) {
	ref = strings.TrimSpace(ref)
	row, err := s.ImageByRef(ref)
	if err != ErrNotFound {
		return row, err
	}
	if ref == "" {
		return nil, ErrNotFound
	}
	// Only a row whose registry_sources document contains the reference can
	// declare it; the database narrows to those before any is decoded. A
	// reference the catalog does not know (a template's own image) is asked
	// about on every launch and replay, and must not cost a decode of the
	// whole catalog each time.
	pattern := "%" + likeEscaper.Replace(ref) + "%"
	var rows []Image
	if err := s.reader().Where(`registry_sources LIKE ? ESCAPE '\'`, pattern).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		var sources []string
		if json.Unmarshal([]byte(row.RegistrySources), &sources) != nil {
			continue
		}
		for _, source := range sources {
			source = strings.TrimSpace(source)
			// registry_sources may also hold an archive URL. It addresses a file
			// to download, never an image reference, so it can never resolve one.
			if source == "" || pullref.IsArchiveURL(source) {
				continue
			}
			if source == ref {
				return &row, nil
			}
		}
	}
	return nil, ErrNotFound
}

// FindModelArtifactAnyRevision resolves an unspecified revision once, preferring
// a complete target-node artifact, then the oldest identity deterministically.
func (s *Store) FindModelArtifactAnyRevision(sourceID, source, subfolder string, nodeID uint) (*Model, error) {
	subfolder, err := modelrepo.NormalizeSubfolder(subfolder)
	if err != nil {
		return nil, err
	}
	query := s.db.Where("lower(source) = ? AND lower(source_id) = ? AND artifact_subfolder = ?", strings.ToLower(strings.TrimSpace(source)), strings.ToLower(strings.TrimSpace(sourceID)), subfolder)
	if nodeID != 0 {
		query = query.Clauses(clause.OrderBy{Expression: clause.Expr{SQL: "CASE WHEN EXISTS (SELECT 1 FROM model_files WHERE model_files.model_id = models.id AND model_files.node_id = ? AND model_files.state IN ('present', 'registered')) THEN 0 ELSE 1 END, models.id ASC", Vars: []any{nodeID}}})
	} else {
		query = query.Order("models.id ASC")
	}
	var model Model
	err = query.Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &model, err
}

// ImageCopies lists the references that name the same image as ref, in the
// order they are preferred: the further sources the deployment's template
// declared (ADR 0038), the registry sources the image catalog records for it,
// then the image's own reference (pullref.Copies). An image the catalog does
// not know has its template's sources and itself.
func (s *Store) ImageCopies(ref string, templateSources []string) []string {
	ref = strings.TrimSpace(ref)
	canonical := ref
	var registrySources []string
	if row, err := s.ResolveImageRef(ref); err == nil {
		_ = json.Unmarshal([]byte(row.RegistrySources), &registrySources)
		// A deployment that names one of the catalog row's copies still runs
		// that copy: it stays the image's own reference, and the row's
		// canonical joins the other copies.
		if !pullref.SameRef(row.ImageRef, ref) {
			registrySources = append(registrySources, row.ImageRef)
		}
	}
	return pullref.Copies(canonical, templateSources, registrySources)
}
