package native

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ImageConfig is the portable subset of an image config blob.
type ImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	Volumes    []string
	WorkingDir string
}

// LoadImageConfig reads the config blob of a registered image.
func (l Layout) LoadImageConfig(ref Ref) (ImageConfig, error) {
	manifest, err := l.LoadManifest(ref)
	if err != nil {
		return ImageConfig{}, err
	}
	data, err := os.ReadFile(filepath.Join(l.LayersDir(), manifest.Config.Digest))
	if err != nil {
		return ImageConfig{}, fmt.Errorf("read image config: %w", err)
	}
	var blob struct {
		Config struct {
			Entrypoint []string            `json:"Entrypoint"`
			Cmd        []string            `json:"Cmd"`
			Env        []string            `json:"Env"`
			Volumes    map[string]struct{} `json:"Volumes"`
			WorkingDir string              `json:"WorkingDir"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &blob); err != nil {
		return ImageConfig{}, fmt.Errorf("decode image config: %w", err)
	}
	config := ImageConfig{Entrypoint: blob.Config.Entrypoint, Cmd: blob.Config.Cmd, Env: blob.Config.Env, WorkingDir: blob.Config.WorkingDir}
	for volume := range blob.Config.Volumes {
		config.Volumes = append(config.Volumes, volume)
	}
	return config, nil
}

// TagImage registers the manifest of source under target as well, the native
// equivalent of `udocker tag`: both names then share the same blobs.
func (l Layout) TagImage(source, target Ref) error {
	if !l.HasImage(source) {
		return fmt.Errorf("image %s:%s is not registered", source.Local, source.Tag)
	}
	data, err := os.ReadFile(filepath.Join(l.TagDir(source), "manifest"))
	if err != nil {
		return err
	}
	manifest, err := l.LoadManifest(source)
	if err != nil {
		return err
	}
	return l.RegisterImage(&Image{Ref: target, Manifest: manifest, ManifestJSON: data})
}

// RemoveImage deletes a registered tag, the blobs no other registered image
// still references, the sidecar tars of those blobs, and the cached base
// rootfs once no remaining tag has the same manifest. Existing containers are
// full copies and keep working, exactly as after `udocker rmi`.
//
// Only the removed manifest's own blobs are candidates, and a blob is kept if
// any other tag directory -- native or written by the udocker CLI, v1 or v2 --
// links to it or lists it, so an unrelated or legacy image is never damaged.
func (l Layout) RemoveImage(ref Ref) error {
	tagDir := l.TagDir(ref)
	if _, err := os.Stat(filepath.Join(tagDir, "TAG")); err != nil {
		return fmt.Errorf("image %s:%s is not registered: %w", ref.Local, ref.Tag, err)
	}
	manifest, _ := l.LoadManifest(ref)
	manifestID, _ := l.ImageID(ref)
	if err := os.RemoveAll(tagDir); err != nil {
		return err
	}
	// Prune now-empty repository directories up to repos/.
	for dir := filepath.Dir(tagDir); dir != l.ReposDir() && strings.HasPrefix(dir, l.ReposDir()); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	inUse, stillTagged, err := l.references()
	if err != nil {
		return fmt.Errorf("scan remaining images: %w", err)
	}
	for _, desc := range append([]Descriptor{manifest.Config}, manifest.Layers...) {
		if desc.Digest == "" || inUse[desc.Digest] {
			continue
		}
		if err := os.Remove(filepath.Join(l.LayersDir(), desc.Digest)); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(filepath.Join(l.Dir, "tarcache", strings.ReplaceAll(desc.Digest, ":", "-")+".tar"))
	}
	if manifestID != "" && !stillTagged[manifestID] {
		// A background build in flight notices the image is gone and discards
		// its own .building directory; removing it here too covers a build a
		// previous agent process left behind.
		for _, dir := range []string{
			l.baseDir(manifestID), l.baseDir(manifestID) + ".building",
			l.preparedDir(manifestID), l.preparedDir(manifestID) + ".building",
		} {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// references walks repos/ and returns every blob name any tag directory links
// to or lists in its manifest, and the IDs of the manifests still registered.
func (l Layout) references() (map[string]bool, map[string]bool, error) {
	blobs, manifests := map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(l.ReposDir(), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err == nil {
				blobs[filepath.Base(target)] = true
			}
			return nil
		}
		if entry.Name() != "manifest" || entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var manifest Manifest
		if json.Unmarshal(data, &manifest) == nil {
			for _, desc := range append([]Descriptor{manifest.Config}, manifest.Layers...) {
				blobs[desc.Digest] = true
			}
		}
		manifests[digestOf(data)] = true
		return nil
	})
	return blobs, manifests, err
}

// Images lists every tag registered under repos/, sorted by reference. A tag
// directory is one holding a TAG marker; its repository is the path between
// repos/ and the tag, which may itself contain slashes.
func (l Layout) Images() ([]Ref, error) {
	root := l.ReposDir()
	var refs []Ref
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return filepath.SkipDir
			}
			return err
		}
		if entry.IsDir() || entry.Name() != "TAG" {
			return nil
		}
		tagDir := filepath.Dir(path)
		repo, err := filepath.Rel(root, filepath.Dir(tagDir))
		if err != nil || repo == "." {
			return nil //nolint:nilerr // A TAG outside a valid repository subdirectory is not an image candidate.
		}
		tag := filepath.Base(tagDir)
		sep := ":"
		if strings.HasPrefix(tag, "sha256:") {
			sep = "@"
		}
		refs = append(refs, ParseRef(filepath.ToSlash(repo)+sep+tag))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	return refs, nil
}

// String is the reference as written: repo:tag or repo@digest.
func (r Ref) String() string {
	if r.IsDigest() {
		return r.Local + "@" + r.Tag
	}
	return r.Local + ":" + r.Tag
}
