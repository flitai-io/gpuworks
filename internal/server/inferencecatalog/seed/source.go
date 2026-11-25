package seed

import (
	"fmt"
	"io/fs"
	"sync"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// Builds made by the release tooling embed the finished manifest. A plain
// go test / go build sees the source descriptor instead. Use the Unix epoch
// for that unpublished package so its digest never depends on the local clock.
var embeddedPackageFS = sync.OnceValues(func() (fs.FS, error) {
	source, err := fs.Sub(embeddedCatalog, "catalog")
	if err != nil {
		return nil, err
	}
	data, err := fs.ReadFile(source, "manifest.yaml")
	if err != nil {
		return nil, err
	}
	var manifest inferencecatalog.PackageManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	if manifest.PublishedAt != "" {
		return source, nil
	}
	data, err = inferencecatalog.BuildSeedManifest(source, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("prepare local seed manifest: %w", err)
	}
	// BuildSeedManifest validated the layout. Supply only the new root entry;
	// every model, README and asset stays in the immutable embedded tree.
	return localSeedFS{source, fstest.MapFS{
		"manifest.yaml": {Data: data, Mode: 0o444},
		"models":        {Mode: fs.ModeDir | 0o555},
	}}, nil
})

type localSeedFS struct {
	source fs.FS
	root   fstest.MapFS
}

func (s localSeedFS) Open(name string) (fs.File, error) {
	if name == "." || name == "manifest.yaml" {
		return s.root.Open(name)
	}
	return s.source.Open(name)
}
