package imagecatalog

import (
	_ "embed"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// seedJSON is the system image manifest generated from the gpuworks-runner
// build matrix (cmd/gpuworks-image-manifest). Every release publishes the same
// file as the distribution channel's image catalog (ADR 0040), so a fresh
// install and "check for updates" import identical content.
//
//go:embed catalog-seed.json
var seedJSON []byte

// ErrManifestInvalid marks a manifest that does not parse or validate.
var ErrManifestInvalid = errors.New("image manifest is invalid")

// ErrConflict marks an import refused because a community or custom row owns
// a catalog series name or image ref.
var ErrConflict = manifest.ErrConflict

// Seed returns the embedded system catalog.
func Seed() (manifest.Manifest, error) { return manifest.Parse(seedJSON) }

// SeedJSON returns the embedded catalog file byte for byte, which is what the
// distribution channel publishes (ADR 0040).
func SeedJSON() []byte { return append([]byte(nil), seedJSON...) }

// SyncSeed imports the embedded system catalog. It runs on every server start
// and is idempotent: rows are upserted by image_ref, operator switches and
// archive URLs survive, and system rows the seed no longer lists are deleted
// or, while something references them, deprecated. An entry that collides
// with a community or custom row is skipped and reported, never fatal.
func SyncSeed(st *store.Store) (manifest.Result, error) {
	m, err := Seed()
	if err != nil {
		// The seed ships inside the binary; failing to parse it is a build
		// defect, not something a deployment can repair.
		return manifest.Result{}, fmt.Errorf("embedded image catalog: %w", err)
	}
	return manifest.ImportOfficial(m, manifestSink{store: st}, manifest.ImportOptions{SkipConflicts: true})
}

// ImportManifest parses a system catalog manifest and applies it. This is the
// operator path ("check for updates", an uploaded manifest): a conflict with a
// community or custom row fails the whole import before anything is written.
func ImportManifest(st *store.Store, body []byte) (manifest.Result, error) {
	m, err := manifest.Parse(body)
	if err != nil {
		return manifest.Result{}, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	return manifest.ImportOfficial(m, manifestSink{store: st}, manifest.ImportOptions{})
}
