// Package manifest contains the system image catalog schema and importer. It
// deliberately has no persistence or HTTP dependencies so the embedded seed,
// "check for updates", and an uploaded manifest all exercise the same
// validation and upsert semantics.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Manifest struct {
	SchemaVersion  int              `json:"schema_version"`
	CatalogVersion string           `json:"catalog_version"`
	GeneratedAt    string           `json:"generated_at,omitempty"`
	Version        string           `json:"version,omitempty"`
	PublishedAt    string           `json:"published_at,omitempty"`
	SHA256         string           `json:"sha256,omitempty"`
	Signature      string           `json:"signature,omitempty"`
	Series         []ManifestSeries `json:"series"`
}

//nolint:revive // Keep the exported manifest schema name stable for callers.
type ManifestSeries struct {
	Name       string            `json:"name"`
	Engine     string            `json:"engine"`
	Purpose    string            `json:"purpose"`
	Framework  string            `json:"framework"`
	Notes      string            `json:"notes,omitempty"`
	CommonArgs []CommonArg       `json:"common_args,omitempty"`
	CommonEnv  []CommonEnv       `json:"common_env,omitempty"`
	Versions   []ManifestVersion `json:"versions"`
}

//nolint:revive // Keep the exported manifest schema name stable for callers.
type ManifestVersion struct {
	// Name is the version's display name; when empty the series name is used.
	Name             string   `json:"name,omitempty"`
	ImageRef         string   `json:"image_ref"`
	EngineVersion    string   `json:"engine_version,omitempty"`
	FrameworkVersion string   `json:"framework_version,omitempty"`
	Variant          string   `json:"-"` // legacy compatibility; image names carry any variant identity
	Arch             string   `json:"arch,omitempty"`
	RegistrySources  []string `json:"registry_sources,omitempty"`
	BlobSHA256       string   `json:"blob_sha256,omitempty"`
	SizeBytes        int64    `json:"size_bytes,omitempty"`
	// BlobSHA256, SizeBytes and Blob are written by older catalogs (the
	// retired offline package). They are still parsed so such a manifest's
	// sha256 verifies; the generator no longer produces them.
	Blob            string `json:"blob,omitempty"`
	Deprecated      bool   `json:"deprecated,omitempty"`
	DeprecatedSince string `json:"deprecated_since,omitempty"`
	RollingTag      bool   `json:"rolling_tag,omitempty"`
	Notes           string `json:"notes,omitempty"`
}

type CommonArg struct {
	Flag        string `json:"flag"`
	Value       string `json:"value,omitempty"`
	Description string `json:"description,omitempty"`
}
type CommonEnv struct {
	Key         string `json:"key"`
	Value       string `json:"value,omitempty"`
	Description string `json:"description,omitempty"`
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 2 && m.SchemaVersion != 3 {
		return fmt.Errorf("unsupported manifest schema_version %d", m.SchemaVersion)
	}
	if strings.TrimSpace(m.CatalogVersion) == "" {
		return errors.New("catalog_version is required")
	}
	if len(m.Series) == 0 {
		return errors.New("series is required")
	}
	seen := map[string]bool{}
	for _, s := range m.Series {
		if strings.TrimSpace(s.Name) == "" {
			return errors.New("series name is required")
		}
		if len(s.Versions) == 0 {
			return fmt.Errorf("series %q must contain at least one version", s.Name)
		}
		for _, v := range s.Versions {
			if err := ValidateTagged(v.ImageRef); err != nil {
				return err
			}
			for _, source := range v.RegistrySources {
				if err := ValidateTagged(source); err != nil {
					return fmt.Errorf("manifest image %q has invalid registry source %q: %w", v.ImageRef, source, err)
				}
			}
			if seen[v.ImageRef] {
				return fmt.Errorf("duplicate image_ref %q", v.ImageRef)
			}
			seen[v.ImageRef] = true
		}
	}
	return nil
}

func ValidateTagged(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("image_ref is required")
	}
	if strings.Contains(ref, "@sha256:") {
		return errors.New("image_ref must include a tag, not a digest")
	}
	if !strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		return fmt.Errorf("image_ref %q must include an explicit tag", ref)
	}
	return nil
}

func Parse(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	if m.SHA256 != "" {
		want := strings.TrimPrefix(strings.ToLower(m.SHA256), "sha256:")
		if ComputeSHA256(m) != want {
			return m, errors.New("manifest sha256 mismatch")
		}
	}
	return m, nil
}

// ComputeSHA256 is the manifest's integrity hash: sha256 over its canonical
// JSON encoding with the sha256 field itself cleared.
func ComputeSHA256(m Manifest) string {
	m.SHA256 = ""
	canonical, _ := json.Marshal(m)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
