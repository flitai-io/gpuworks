package inferencecatalog

import (
	"fmt"
	"io/fs"
	"time"

	"gopkg.in/yaml.v3"
)

// BuildSeedManifest translates the source descriptor into the complete
// distribution manifest. It validates the models and assets without
// reserializing any model-owned files. A zero date denotes an unpublished
// development build; use the reproducible archive epoch for those builds.
func BuildSeedManifest(source fs.FS, publishedAt time.Time) ([]byte, error) {
	data, err := fs.ReadFile(source, "manifest.yaml")
	if err != nil {
		return nil, fmt.Errorf("read seed descriptor: %w", err)
	}
	var descriptor struct {
		SchemaVersion int `yaml:"schema_version"`
	}
	if err := decodeManifest(data, &descriptor, true, "seed catalog descriptor"); err != nil {
		return nil, err
	}
	if !supportedPackageSchema(descriptor.SchemaVersion) {
		return nil, fmt.Errorf("unsupported seed catalog descriptor schema_version %d", descriptor.SchemaVersion)
	}
	if publishedAt.IsZero() {
		publishedAt = time.Unix(0, 0)
	}
	pkg, _, err := loadPackageContents(source, false, PackageManifest{
		SchemaVersion: descriptor.SchemaVersion,
		PublishedAt:   publishedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	if err := validatePackageManifest(pkg.Manifest); err != nil {
		return nil, err
	}
	return yaml.Marshal(pkg.Manifest)
}
