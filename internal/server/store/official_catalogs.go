package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// OfficialCatalogState is what the database knows about the two official
// catalogs beyond their rows (setting official_catalogs, read and written
// whole). Each catalog is last-import-wins (ADR 0040): the embedded catalogs
// are applied again only when the server build changes, so an online update
// or an upload survives a restart.
type OfficialCatalogState struct {
	// SeededBuild is the server build (version.String()) that last applied the
	// catalogs embedded in its binary.
	SeededBuild string `json:"seeded_build,omitempty"`
	// ImageCatalogVersion is the catalog_version of the system image catalog
	// last imported from any entry point: the embedded seed, the channel or an
	// upload. Nothing in the image rows records it.
	ImageCatalogVersion string `json:"image_catalog_version,omitempty"`
}

// officialCatalogsMu serializes the read-modify-write of the state row.
var officialCatalogsMu sync.Mutex

// OfficialCatalogState returns the recorded state; an absent row is the zero
// state, which makes the first start apply the embedded catalogs.
func (s *Store) OfficialCatalogState() (OfficialCatalogState, error) {
	officialCatalogsMu.Lock()
	defer officialCatalogsMu.Unlock()
	return s.readOfficialCatalogState()
}

func (s *Store) readOfficialCatalogState() (OfficialCatalogState, error) {
	var state OfficialCatalogState
	raw := strings.TrimSpace(s.GetSetting(SettingOfficialCatalogs))
	if raw == "" {
		return state, nil
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return OfficialCatalogState{}, fmt.Errorf("decode %s setting: %w", SettingOfficialCatalogs, err)
	}
	return state, nil
}

func (s *Store) updateOfficialCatalogState(change func(*OfficialCatalogState)) error {
	officialCatalogsMu.Lock()
	defer officialCatalogsMu.Unlock()
	state, err := s.readOfficialCatalogState()
	if err != nil {
		return err
	}
	change(&state)
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingOfficialCatalogs, string(encoded))
}

// RecordImageCatalogImport notes the catalog_version of an image catalog that
// was just imported.
func (s *Store) RecordImageCatalogImport(catalogVersion string) error {
	return s.updateOfficialCatalogState(func(state *OfficialCatalogState) {
		state.ImageCatalogVersion = strings.TrimSpace(catalogVersion)
	})
}

// RecordEmbeddedCatalogsApplied notes that build applied its embedded catalogs.
func (s *Store) RecordEmbeddedCatalogsApplied(build string) error {
	return s.updateOfficialCatalogState(func(state *OfficialCatalogState) {
		state.SeededBuild = build
	})
}

// UpstreamInferencePackageDigest is the package the upstream inference specs
// were last reconciled from, which is the inference template catalog
// currently in force; empty when there are none. A reconcile stamps its
// digest on every spec it writes, and one that could read its catalog only
// in part leaves the rows it skipped on the digest they had
// (ReconcileInferenceCatalogKeeping), so the newest write names the catalog.
func (s *Store) UpstreamInferencePackageDigest() (string, error) {
	var digests []string
	if err := s.reader().Model(&InferenceSpecRow{}).
		Where("source = ? AND package_digest <> ''", "upstream").
		Group("package_digest").Order("MAX(updated_at) DESC, COUNT(*) DESC").
		Limit(1).Pluck("package_digest", &digests).Error; err != nil {
		return "", err
	}
	if len(digests) == 0 {
		return "", nil
	}
	return digests[0], nil
}
