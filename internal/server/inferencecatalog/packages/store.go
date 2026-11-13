// Package packages owns content-addressed inference catalog packages under
// the server data directory. The database stores only their digest/projection.
package packages

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

type Store struct {
	root string
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PutArchive validates and stores the canonical distribution format. The
// server persists the package embedded in its own binary with PutLoaded, so an
// older deployment can still resolve its exact assets after a future binary
// upgrade.
func (store *Store) PutArchive(reader io.Reader) (digest string, created bool, err error) {
	archive, err := inferencecatalog.ReadPackageArchive(reader)
	if err != nil {
		return "", false, err
	}
	return store.Put(archive.FS)
}

func New(root string) (*Store, error) {
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) {
		return nil, errors.New("catalog package store requires a scoped root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create catalog package store: %w", err)
	}
	return &Store{root: root}, nil
}

// Put validates and atomically persists a package. created tells the caller
// whether a failed database transaction may safely remove this exact digest.
func (store *Store) Put(fsys fs.FS) (digest string, created bool, err error) {
	if store == nil {
		return "", false, errors.New("catalog package store is unavailable")
	}
	if _, err := inferencecatalog.LoadPackage(fsys); err != nil {
		return "", false, err
	}
	digest, err = inferencecatalog.PackageDigest(fsys)
	if err != nil {
		return "", false, err
	}
	created, err = store.publish(fsys, digest)
	return digest, created, err
}

// PutLoaded persists a package the caller has already loaded with
// inferencecatalog.LoadPackage and whose digest it computed with
// inferencecatalog.PackageDigest: the package embedded in the binary, which a
// process validates once however many servers it starts. Anything read from
// outside goes through Put or PutArchive.
func (store *Store) PutLoaded(fsys fs.FS, digest string) (created bool, err error) {
	if store == nil {
		return false, errors.New("catalog package store is unavailable")
	}
	if !digestPattern.MatchString(digest) {
		return false, fmt.Errorf("invalid catalog package digest %q", digest)
	}
	return store.publish(fsys, digest)
}

// publish writes a validated package under its digest, unless that digest is
// already stored.
func (store *Store) publish(fsys fs.FS, digest string) (created bool, err error) {
	destination := filepath.Join(store.root, digest)
	if info, statErr := os.Stat(destination); statErr == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("catalog package digest destination is not a directory: %s", digest)
		}
		if _, loadErr := inferencecatalog.LoadPackageDirectory(destination); loadErr != nil {
			return false, fmt.Errorf("catalog package digest cache is invalid: %w", loadErr)
		}
		return false, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}

	staging, err := os.MkdirTemp(store.root, ".package-*")
	if err != nil {
		return false, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || name == "." {
			return walkErr
		}
		target := filepath.Join(staging, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("catalog package contains non-regular file %s", name)
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		return false, fmt.Errorf("stage catalog package: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		return false, fmt.Errorf("publish catalog package: %w", err)
	}
	published = true
	return true, nil
}

func (store *Store) Remove(digest string) error {
	if store == nil || !digestPattern.MatchString(digest) {
		return errors.New("invalid catalog package digest")
	}
	return os.RemoveAll(filepath.Join(store.root, digest))
}

// Prune removes content-addressed packages that are no longer referenced by
// the database. Only immediate, regular digest directories below the scoped
// store root are eligible; staging entries, files, and symlinks are ignored.
func (store *Store) Prune(referenced []string) ([]string, error) {
	if store == nil {
		return nil, errors.New("catalog package store is unavailable")
	}
	keep := make(map[string]struct{}, len(referenced))
	for _, digest := range referenced {
		digest = strings.ToLower(strings.TrimSpace(digest))
		if digestPattern.MatchString(digest) {
			keep[digest] = struct{}{}
		}
	}
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return nil, fmt.Errorf("list catalog package store: %w", err)
	}
	removed := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !digestPattern.MatchString(name) {
			continue
		}
		if _, referenced := keep[name]; referenced {
			continue
		}
		if err := os.RemoveAll(filepath.Join(store.root, name)); err != nil {
			return removed, fmt.Errorf("remove unreferenced catalog package %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	sort.Strings(removed)
	return removed, nil
}

func (store *Store) ReadAsset(digest, modelID, file string) ([]byte, error) {
	if store == nil || !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid catalog package digest")
	}
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	file = path.Clean(strings.ReplaceAll(strings.TrimSpace(file), "\\", "/"))
	if !fs.ValidPath(modelID) || !fs.ValidPath(file) || !strings.HasPrefix(file, "assets/") {
		return nil, errors.New("invalid catalog package asset path")
	}
	name := filepath.Join(store.root, digest, "models", filepath.FromSlash(modelID), filepath.FromSlash(file))
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read catalog package asset: %w", err)
	}
	return data, nil
}

// ReadDocumentation returns one localized README from a content-addressed
// inference catalog package without allowing arbitrary package paths.
func (store *Store) ReadDocumentation(digest, modelID, file string) ([]byte, error) {
	if store == nil || !digestPattern.MatchString(digest) {
		return nil, errors.New("invalid catalog package digest")
	}
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	file = strings.TrimSpace(file)
	if !fs.ValidPath(modelID) || (file != "README.zh-CN.md" && file != "README.en.md") {
		return nil, errors.New("invalid catalog package documentation path")
	}
	name := filepath.Join(store.root, digest, "models", filepath.FromSlash(modelID), file)
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read catalog package documentation: %w", err)
	}
	return data, nil
}

// Pack exports one content-addressed inference catalog package in the canonical
// .gpuworks-catalog.tar.gz format. Recomputing the identity prevents a damaged
// on-disk cache directory from being served under a stale digest.
func (store *Store) Pack(digest string, writer io.Writer) error {
	if store == nil || !digestPattern.MatchString(digest) {
		return errors.New("invalid catalog package digest")
	}
	root := filepath.Join(store.root, digest)
	actual, err := inferencecatalog.PackageDigest(os.DirFS(root))
	if err != nil {
		return fmt.Errorf("open catalog package %s: %w", digest, err)
	}
	if actual != digest {
		return fmt.Errorf("catalog package %s content digest is %s", digest, actual)
	}
	return inferencecatalog.PackPackage(writer, os.DirFS(root))
}

// Load returns the validated package projection for an immutable digest.
func (store *Store) Load(digest string) (inferencecatalog.Package, error) {
	if store == nil || !digestPattern.MatchString(digest) {
		return inferencecatalog.Package{}, errors.New("invalid catalog package digest")
	}
	root := filepath.Join(store.root, digest)
	actual, err := inferencecatalog.PackageDigest(os.DirFS(root))
	if err != nil {
		return inferencecatalog.Package{}, fmt.Errorf("open catalog package %s: %w", digest, err)
	}
	if actual != digest {
		return inferencecatalog.Package{}, fmt.Errorf("catalog package %s content digest is %s", digest, actual)
	}
	return inferencecatalog.LoadPackageDirectory(root)
}
