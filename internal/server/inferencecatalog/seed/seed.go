package seed

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

//go:embed catalog
var embeddedCatalog embed.FS

// Catalog returns the validated catalog shipped by the server. The embedded
// multi-file package is the sole built-in authority; the database is only its
// queryable projection. Each call returns a catalog of its own: callers may
// normalize or edit it without touching another's.
func Catalog() (inferencecatalog.FlatCatalog, error) {
	cached, err := loadEmbedded()
	if err != nil {
		return inferencecatalog.FlatCatalog{}, err
	}
	return cached.catalogCopy()
}

// Archive returns the deterministic archive of the embedded catalog package.
// Callers persist it in the content-addressed package store at startup.
func Archive() ([]byte, error) {
	cached, err := loadEmbedded()
	if err != nil {
		return nil, err
	}
	return bytes.Clone(cached.archive), nil
}

// Package returns the embedded package's files and digest, for the server to
// persist in its content-addressed package store at startup
// (packages.Store.PutLoaded): the package was loaded and validated once for
// the process, and is not packed and unpacked again on each start.
func Package() (fs.FS, string, error) {
	cached, err := loadEmbedded()
	if err != nil {
		return nil, "", err
	}
	root, err := embeddedPackageFS()
	if err != nil {
		return nil, "", err
	}
	return root, cached.digest, nil
}

// Published returns the embedded package as the distribution channel
// publishes it: the deterministic archive, its package digest (what upstream
// specs record) and its manifest (ADR 0040).
func Published() ([]byte, string, inferencecatalog.PackageManifest, error) {
	cached, err := loadEmbedded()
	if err != nil {
		return nil, "", inferencecatalog.PackageManifest{}, err
	}
	return bytes.Clone(cached.archive), cached.digest, cached.manifest, nil
}

// CatalogWithDigest returns the validated catalog together with the digest
// of the exact embedded package it was loaded from.
func CatalogWithDigest() (inferencecatalog.FlatCatalog, string, error) {
	cached, err := loadEmbedded()
	if err != nil {
		return inferencecatalog.FlatCatalog{}, "", err
	}
	catalog, err := cached.catalogCopy()
	if err != nil {
		return inferencecatalog.FlatCatalog{}, "", err
	}
	return catalog, cached.digest, nil
}

// embeddedCache is the embedded package loaded, digested and packed. The
// package is part of the binary and never changes while it runs, so the work
// is done once per process instead of on every server start: a test binary
// starts hundreds of servers, each of which paid it again.
type embeddedCache struct {
	// catalog is the validated FlatCatalog as JSON, decoded afresh for each
	// caller so none shares another's slices and maps. JSON carries every
	// field the catalog types define (TestEmbeddedCatalogCopyIsExact).
	catalog  []byte
	digest   string
	manifest inferencecatalog.PackageManifest
	archive  []byte
}

var loadEmbedded = sync.OnceValues(func() (embeddedCache, error) {
	pkg, err := embeddedPackage()
	if err != nil {
		return embeddedCache{}, err
	}
	root, err := embeddedPackageFS()
	if err != nil {
		return embeddedCache{}, err
	}
	digest, err := inferencecatalog.PackageDigest(root)
	if err != nil {
		return embeddedCache{}, fmt.Errorf("digest embedded inference catalog: %w", err)
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, root); err != nil {
		return embeddedCache{}, err
	}
	catalog, err := json.Marshal(pkg.Catalog)
	if err != nil {
		return embeddedCache{}, fmt.Errorf("encode embedded inference catalog: %w", err)
	}
	return embeddedCache{catalog: catalog, digest: digest, manifest: pkg.Manifest, archive: archive.Bytes()}, nil
})

func (c embeddedCache) catalogCopy() (inferencecatalog.FlatCatalog, error) {
	var catalog inferencecatalog.FlatCatalog
	if err := json.Unmarshal(c.catalog, &catalog); err != nil {
		return inferencecatalog.FlatCatalog{}, fmt.Errorf("decode embedded inference catalog: %w", err)
	}
	return catalog, nil
}

func embeddedPackage() (inferencecatalog.Package, error) {
	root, err := embeddedPackageFS()
	if err != nil {
		return inferencecatalog.Package{}, fmt.Errorf("open embedded inference catalog package: %w", err)
	}
	pkg, err := inferencecatalog.LoadPackage(root)
	if err != nil {
		return inferencecatalog.Package{}, fmt.Errorf("load embedded inference catalog package: %w", err)
	}
	return pkg, nil
}

func embeddedPackageFS() (fs.FS, error) {
	root, err := fs.Sub(embeddedCatalog, "catalog")
	if err != nil {
		return nil, fmt.Errorf("open embedded inference catalog package: %w", err)
	}
	return root, nil
}
