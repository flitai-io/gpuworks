package inferencecatalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

// PackageSchemaVersion is the package format this server writes and the
// newest it reads. 2 added spec display names (display_name_zh_cn /
// display_name_en): a server that only knows 1 parses model.yaml strictly
// and would fail on them, so it must see the newer version and treat the
// package as incompatible rather than download it. Packages of 1 still read
// here; their retired display_profile keys are ignored with a warning.
const PackageSchemaVersion = 2

// oldestPackageSchemaVersion is the oldest package format this server reads.
const oldestPackageSchemaVersion = 1

// PackageManifest describes one executable inference catalog package. Counts
// are a checked summary, not a second model index: model identity comes only
// from models/**/model.yaml.
type PackageManifest struct {
	SchemaVersion int    `yaml:"schema_version" json:"schema_version"`
	PublishedAt   string `yaml:"published_at" json:"published_at"`
	Models        int    `yaml:"models" json:"models"`
	Specs         int    `yaml:"specs" json:"specs"`
}

// Package is the validated catalog plus its distribution metadata.
type Package struct {
	Manifest PackageManifest
	Catalog  FlatCatalog
}

type Archive struct {
	Package Package
	FS      fs.FS
}

// LoadPackage reads a catalog rooted at fsys. It rejects files outside the
// model-owned layout so an embedded, downloaded, and offline-imported package
// all have the same authority boundary.
func LoadPackage(fsys fs.FS) (Package, error) {
	manifestData, err := fs.ReadFile(fsys, "manifest.yaml")
	if err != nil {
		return Package{}, fmt.Errorf("read inference catalog package manifest: %w", err)
	}
	manifest, err := parsePackageManifest(manifestData)
	if err != nil {
		return Package{}, err
	}

	var modelFiles []string
	err = fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("inference catalog package contains non-regular file %s", name)
		}
		if err := validatePackageFileName(name); err != nil {
			return err
		}
		if path.Base(name) == "model.yaml" {
			modelFiles = append(modelFiles, name)
		}
		return nil
	})
	if err != nil {
		return Package{}, fmt.Errorf("walk inference catalog package: %w", err)
	}
	if len(modelFiles) == 0 {
		return Package{}, errors.New("inference catalog package requires at least one models/**/model.yaml")
	}
	sort.Strings(modelFiles)

	catalog := FlatCatalog{Models: make([]InferenceModel, 0, len(modelFiles))}
	for _, name := range modelFiles {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return Package{}, fmt.Errorf("read inference catalog model %s: %w", name, err)
		}
		model, err := parseModelYAML(data)
		if err != nil {
			return Package{}, fmt.Errorf("parse inference catalog model %s: %w", name, err)
		}
		modelDirectory := path.Dir(name)
		if _, err := fs.Stat(fsys, modelDirectory+"/README.zh-CN.md"); err == nil {
			model.ReadmeZHCN = "README.zh-CN.md"
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Package{}, fmt.Errorf("inspect model %s Chinese README: %w", model.ModelID, err)
		}
		if _, err := fs.Stat(fsys, modelDirectory+"/README.en.md"); err == nil {
			model.ReadmeEN = "README.en.md"
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Package{}, fmt.Errorf("inspect model %s English README: %w", model.ModelID, err)
		}
		catalog.Models = append(catalog.Models, model)
	}
	if err := ValidateFlatCatalog(&catalog); err != nil {
		return Package{}, err
	}
	for i, name := range modelFiles {
		expected, err := modelFileName(catalog.Models[i].ModelID)
		if err != nil {
			return Package{}, err
		}
		if name != expected {
			return Package{}, fmt.Errorf("model %s must be stored at %s, found %s", catalog.Models[i].ModelID, expected, name)
		}
	}
	if err := validatePackageAssets(fsys, &catalog); err != nil {
		return Package{}, err
	}
	// Filesystem ordering includes the trailing "/model.yaml", so it is not
	// equivalent to ordering bare model IDs (for example "foo-2" sorts before
	// "foo/model.yaml"). Keep the public catalog order independent of layout.
	sort.Slice(catalog.Models, func(i, j int) bool {
		return catalog.Models[i].ModelID < catalog.Models[j].ModelID
	})

	specs := countSpecs(catalog)
	if manifest.Models != len(catalog.Models) || manifest.Specs != specs {
		return Package{}, fmt.Errorf("inference catalog manifest says %d models/%d specs, package contains %d/%d", manifest.Models, manifest.Specs, len(catalog.Models), specs)
	}
	return Package{Manifest: manifest, Catalog: catalog}, nil
}

func LoadPackageDirectory(root string) (Package, error) {
	return LoadPackage(os.DirFS(root))
}

// ReadPackageArchive decodes the distribution form without touching disk.
// Link-like entries, ambiguous paths, duplicate names, and oversized streams
// are rejected before the regular package validator sees the files.
func ReadPackageArchive(reader io.Reader) (Archive, error) {
	limited := &countingReader{reader: io.LimitReader(reader, catalogasset.MaxPackageBytes+1)}
	gz, err := gzip.NewReader(limited)
	if err != nil {
		return Archive{}, fmt.Errorf("open inference catalog package gzip: %w", err)
	}
	gz.Multistream(false)
	tarReader := tar.NewReader(gz)
	files := fstest.MapFS{}
	var expanded int64
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Archive{}, fmt.Errorf("read inference catalog package tar: %w", err)
		}
		name := path.Clean(strings.ReplaceAll(header.Name, "\\", "/"))
		if name == "." || name != header.Name || path.IsAbs(header.Name) || !fs.ValidPath(name) || strings.HasPrefix(name, "../") {
			return Archive{}, fmt.Errorf("unsafe inference catalog package path %q", header.Name)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return Archive{}, fmt.Errorf("inference catalog package entry %s is not a regular file", name)
		}
		if _, duplicate := files[name]; duplicate {
			return Archive{}, fmt.Errorf("duplicate inference catalog package path %q", name)
		}
		if header.Size < 0 {
			return Archive{}, fmt.Errorf("inference catalog package entry %s has a negative size", name)
		}
		expanded += header.Size
		if expanded > 64<<20 {
			return Archive{}, errors.New("inference catalog package expands beyond 64 MiB")
		}
		data, err := io.ReadAll(io.LimitReader(tarReader, header.Size+1))
		if err != nil {
			return Archive{}, fmt.Errorf("read inference catalog package entry %s: %w", name, err)
		}
		if int64(len(data)) != header.Size {
			return Archive{}, fmt.Errorf("short inference catalog package entry %s", name)
		}
		files[name] = &fstest.MapFile{Data: data, Mode: 0o644}
	}
	if err := gz.Close(); err != nil {
		return Archive{}, fmt.Errorf("close inference catalog package gzip: %w", err)
	}
	if limited.bytes > catalogasset.MaxPackageBytes {
		return Archive{}, fmt.Errorf("compressed inference catalog package exceeds %d bytes", catalogasset.MaxPackageBytes)
	}
	pkg, err := LoadPackage(files)
	if err != nil {
		return Archive{}, err
	}
	return Archive{Package: pkg, FS: files}, nil
}

// PackageDigest identifies the exact deterministic distribution archive.
// CatalogVersion names a catalog package for display: its publication date
// and the start of its digest, since the date alone may stay the same across
// two different packages. The distribution index and the update view both use
// it (ADR 0040).
func CatalogVersion(publishedAt, digest string) string {
	date := strings.TrimSpace(publishedAt)
	if t, err := time.Parse(time.RFC3339, date); err == nil {
		date = t.UTC().Format("2006-01-02")
	}
	short := digest
	if len(short) > 8 {
		short = short[:8]
	}
	if date == "" {
		return short
	}
	return date + "." + short
}

func PackageDigest(fsys fs.FS) (string, error) {
	hash := sha256.New()
	if err := PackPackage(hash, fsys); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// MarshalModelYAML emits the canonical per-model document used inside a
// package. Validation still runs at the whole-catalog boundary after loading,
// where duplicate model identities can be detected.
func MarshalModelYAML(model InferenceModel) ([]byte, error) {
	catalog := FlatCatalog{Models: []InferenceModel{model}}
	if err := validateFlatCatalogStructure(&catalog); err != nil {
		return nil, err
	}
	return yaml.Marshal(catalog.Models[0])
}

// WritePackageDirectory creates a new canonical package directory. Refusing
// to overwrite an existing target keeps release tooling from partially
// replacing the checked-in authority after a failed write.
func WritePackageDirectory(root, publishedAt string, catalog FlatCatalog) error {
	return WritePackageDirectoryWithFiles(root, publishedAt, catalog, nil)
}

// WritePackageDirectoryWithFiles publishes model YAML plus referenced
// model-local assets/readmes from source. The flat candidate remains the
// structured authority; source only supplies bytes whose names and hashes are
// already declared by that candidate.
func WritePackageDirectoryWithFiles(root, publishedAt string, catalog FlatCatalog, source fs.FS) error {
	if err := ValidateFlatCatalog(&catalog); err != nil {
		return err
	}
	if _, err := os.Stat(root); err == nil {
		return fmt.Errorf("inference catalog package output already exists: %s", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect inference catalog package output: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(publishedAt)); err != nil {
		return fmt.Errorf("inference catalog package published_at must be RFC3339: %w", err)
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].ModelID < catalog.Models[j].ModelID })
	manifest := PackageManifest{SchemaVersion: PackageSchemaVersion, PublishedAt: publishedAt, Models: len(catalog.Models), Specs: countSpecs(catalog)}

	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create inference catalog package parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".catalog-package-*")
	if err != nil {
		return fmt.Errorf("create inference catalog package staging directory: %w", err)
	}
	keepTemporary := false
	defer func() {
		if !keepTemporary {
			_ = os.RemoveAll(temporary)
		}
	}()

	manifestData, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal inference catalog package manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "manifest.yaml"), manifestData, 0o644); err != nil {
		return fmt.Errorf("write inference catalog package manifest: %w", err)
	}
	for _, model := range catalog.Models {
		name, err := modelFileName(model.ModelID)
		if err != nil {
			return err
		}
		destination := filepath.Join(temporary, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return fmt.Errorf("create inference catalog model directory: %w", err)
		}
		data, err := MarshalModelYAML(model)
		if err != nil {
			return fmt.Errorf("marshal inference catalog model %s: %w", model.ModelID, err)
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			return fmt.Errorf("write inference catalog model %s: %w", model.ModelID, err)
		}
	}
	if err := copyPackageModelFiles(temporary, catalog, source); err != nil {
		return err
	}
	if _, err := LoadPackageDirectory(temporary); err != nil {
		return fmt.Errorf("verify written inference catalog package: %w", err)
	}
	if err := os.Rename(temporary, root); err != nil {
		return fmt.Errorf("publish inference catalog package: %w", err)
	}
	keepTemporary = true
	return nil
}

// PackPackage writes a deterministic tar.gz representation of a validated
// package. It intentionally carries no host timestamps, ownership, or modes.
func PackPackage(writer io.Writer, fsys fs.FS) error {
	if _, err := LoadPackage(fsys); err != nil {
		return err
	}
	var files []string
	if err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("inference catalog package contains non-regular file %s", name)
		}
		files = append(files, name)
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(files)

	counted := &countingWriter{writer: writer}
	gz := gzip.NewWriter(counted)
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tarWriter := tar.NewWriter(gz)
	for _, name := range files {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read package file %s: %w", name, err)
		}
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("write package header %s: %w", name, err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			return fmt.Errorf("write package file %s: %w", name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("close inference catalog tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close inference catalog gzip: %w", err)
	}
	if counted.bytes > catalogasset.MaxPackageBytes {
		return fmt.Errorf("compressed inference catalog package is %d bytes, limit is %d", counted.bytes, catalogasset.MaxPackageBytes)
	}
	return nil
}

type countingWriter struct {
	writer io.Writer
	bytes  int64
}

type countingReader struct {
	reader io.Reader
	bytes  int64
}

func (reader *countingReader) Read(data []byte) (int, error) {
	read, err := reader.reader.Read(data)
	reader.bytes += int64(read)
	return read, err
}

func (writer *countingWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	writer.bytes += int64(written)
	return written, err
}

func parsePackageManifest(data []byte) (PackageManifest, error) {
	var manifest PackageManifest
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return PackageManifest{}, fmt.Errorf("parse inference catalog package manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return PackageManifest{}, errors.New("inference catalog package manifest must contain one document")
		}
		return PackageManifest{}, fmt.Errorf("parse inference catalog package manifest: %w", err)
	}
	if manifest.SchemaVersion < oldestPackageSchemaVersion || manifest.SchemaVersion > PackageSchemaVersion {
		return PackageManifest{}, fmt.Errorf("unsupported inference catalog package schema_version %d", manifest.SchemaVersion)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(manifest.PublishedAt)); err != nil {
		return PackageManifest{}, fmt.Errorf("inference catalog package published_at must be RFC3339: %w", err)
	}
	if manifest.Models <= 0 || manifest.Specs <= 0 {
		return PackageManifest{}, errors.New("inference catalog package manifest requires positive models and specs")
	}
	return manifest, nil
}

func parseModelYAML(data []byte) (InferenceModel, error) {
	var model InferenceModel
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&model); err != nil {
		return InferenceModel{}, requestDeclarationDecodeError(data, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return InferenceModel{}, errors.New("model.yaml must contain one document")
		}
		return InferenceModel{}, err
	}
	return model, nil
}

func modelFileName(modelID string) (string, error) {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if !validModelIDPath(modelID) {
		return "", fmt.Errorf("model_id %q cannot be represented as a package path", modelID)
	}
	return "models/" + modelID + "/model.yaml", nil
}

func validModelIDPath(modelID string) bool {
	if modelID == "" || !fs.ValidPath(modelID) || strings.Contains(modelID, "\\") {
		return false
	}
	for _, segment := range strings.Split(modelID, "/") {
		if segment == "." || segment == ".." || segment == "" {
			return false
		}
	}
	return true
}

func validatePackageFileName(name string) error {
	if name == "manifest.yaml" {
		return nil
	}
	if !strings.HasPrefix(name, "models/") || !fs.ValidPath(name) {
		return fmt.Errorf("inference catalog package contains unexpected file %s", name)
	}
	base := path.Base(name)
	if base == "model.yaml" || base == "README.zh-CN.md" || base == "README.en.md" {
		return nil
	}
	if !strings.Contains(name, "/assets/") {
		return fmt.Errorf("inference catalog package contains unexpected model file %s", name)
	}
	for _, suffix := range []string{".py", ".sh", ".patch", ".diff", ".json", ".yaml", ".yml", ".toml", ".txt", ".html", ".md", ".tar.gz"} {
		if strings.HasSuffix(base, suffix) {
			return nil
		}
	}
	return fmt.Errorf("inference catalog package asset type is not allowed: %s", name)
}

func validatePackageAssets(fsys fs.FS, catalog *FlatCatalog) error {
	referenced := map[string]struct{}{}
	sizes := map[string]int64{}
	for modelIndex := range catalog.Models {
		model := &catalog.Models[modelIndex]
		var modelBytes int64
		for specIndex := range model.Specs {
			for assetIndex := range model.Specs[specIndex].Assets {
				asset := &model.Specs[specIndex].Assets[assetIndex]
				name := "models/" + model.ModelID + "/" + asset.File
				if _, duplicate := referenced[name]; duplicate {
					asset.SizeBytes = sizes[name]
					continue
				}
				referenced[name] = struct{}{}
				data, err := fs.ReadFile(fsys, name)
				if err != nil {
					return fmt.Errorf("read model %s asset %s: %w", model.ModelID, asset.File, err)
				}
				actualSize := int64(len(data))
				if actualSize > catalogasset.MaxFileBytes {
					return fmt.Errorf("model %s asset %s exceeds %d bytes", model.ModelID, asset.File, catalogasset.MaxFileBytes)
				}
				if asset.SizeBytes != 0 && asset.SizeBytes != actualSize {
					return fmt.Errorf("model %s asset %s size_bytes is %d, actual size is %d", model.ModelID, asset.File, asset.SizeBytes, actualSize)
				}
				asset.SizeBytes = actualSize
				sizes[name] = actualSize
				digest := sha256.Sum256(data)
				if fmt.Sprintf("%x", digest) != asset.SHA256 {
					return fmt.Errorf("model %s asset %s SHA-256 mismatch", model.ModelID, asset.File)
				}
				expanded := int64(len(data))
				if strings.HasSuffix(strings.ToLower(asset.File), ".tar.gz") {
					expanded, err = catalogasset.ValidateTarGzip(bytes.NewReader(data))
					if err != nil {
						return fmt.Errorf("model %s asset %s: %w", model.ModelID, asset.File, err)
					}
				}
				modelBytes += expanded
				if modelBytes > catalogasset.MaxModelExpandedBytes {
					return fmt.Errorf("model %s assets exceed %d expanded bytes", model.ModelID, catalogasset.MaxModelExpandedBytes)
				}
			}
		}
	}
	return fs.WalkDir(fsys, "models", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.Contains(name, "/assets/") {
			return walkErr
		}
		if _, ok := referenced[name]; !ok {
			return fmt.Errorf("inference catalog package contains unreferenced asset %s", name)
		}
		return nil
	})
}

func countSpecs(catalog FlatCatalog) int {
	total := 0
	for _, model := range catalog.Models {
		total += len(model.Specs)
	}
	return total
}
