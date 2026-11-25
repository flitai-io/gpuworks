package seed

import (
	_ "embed"
	"fmt"
	"io/fs"
	"sync"
	"testing/fstest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// The release build overlays this empty file. Keep it outside catalog so the
// generator always sees the source descriptor and the compiler's model tree.
//
//go:embed release-manifest.yaml
var releaseManifest []byte

var seedSource = sync.OnceValues(func() (fs.FS, error) {
	return fs.Sub(embeddedCatalog, "catalog")
})

// BuildManifest counts exactly the models Go embeds in both release binaries.
func BuildManifest(publishedAt time.Time) ([]byte, error) {
	source, err := seedSource()
	if err != nil {
		return nil, err
	}
	return inferencecatalog.BuildSeedManifest(source, publishedAt)
}

var embeddedPackageFS = sync.OnceValues(func() (fs.FS, error) {
	source, err := seedSource()
	if err != nil {
		return nil, err
	}
	data := releaseManifest
	if len(data) == 0 {
		data, err = BuildManifest(time.Time{})
		if err != nil {
			return nil, fmt.Errorf("prepare local seed manifest: %w", err)
		}
	}
	return seedFS{source, fstest.MapFS{
		"manifest.yaml": {Data: data, Mode: 0o444},
		"models":        {Mode: fs.ModeDir | 0o555},
	}}, nil
})

type seedFS struct {
	source fs.FS
	root   fstest.MapFS
}

func (s seedFS) Open(name string) (fs.File, error) {
	if name == "." || name == "manifest.yaml" {
		return s.root.Open(name)
	}
	return s.source.Open(name)
}
