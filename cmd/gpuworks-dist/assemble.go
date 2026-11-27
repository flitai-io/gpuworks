package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

// defaultArchivePrefix starts the release archive names
// scripts/package_release.sh writes (its PACKAGE_PREFIX):
// <prefix>-<release>-<os>-<arch>.tar.gz.
const defaultArchivePrefix = "gpuworks"

var archivePrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// archivePrefix is the archive name prefix a -prefix flag names; empty is
// defaultArchivePrefix.
func archivePrefix(flag string) (string, error) {
	if flag == "" {
		return defaultArchivePrefix, nil
	}
	if !archivePrefixPattern.MatchString(flag) {
		return "", fmt.Errorf("-prefix %q is not an archive name prefix", flag)
	}
	return flag, nil
}

// archivePattern matches the release archive names that start with prefix.
func archivePattern(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-([A-Za-z0-9._+-]+)-(linux|darwin)-(amd64|arm64)\.tar\.gz$`)
}

// assembleInput is everything a tree is made from. The catalogs default to the
// ones embedded in this build, so the published catalogs are exactly what a
// server built from the same commit carries.
type assembleInput struct {
	Adopted           bool
	DatabaseMigration int
	Previous          string
	Keep              int
	Channel           string
	// Edition is the edition the index is published for; empty is
	// community, which leaves the index exactly as before editions.
	Edition string
	// ArchivePrefix starts the archive names in DistDir; empty is
	// defaultArchivePrefix.
	ArchivePrefix string
	DistDir       string
	OutDir        string
	ReleaseID     string
	Commit        string
	BuiltAt       string
	PublishedAt   time.Time
	KeyID         string
	Key           ed25519.PrivateKey

	ImageCatalog    []byte
	TemplateArchive []byte
	TemplateDigest  string
	TemplateInfo    inferencecatalog.PackageManifest
}

func assembleCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("assemble", flag.ContinueOnError)
	in := assembleInput{}
	fs.IntVar(&in.DatabaseMigration, "database-migration", 0, "database migration supported by the packaged build")
	fs.BoolVar(&in.Adopted, "adopted", false, "archives were adopted from a prior publication")
	var keyEnv string
	fs.StringVar(&in.Previous, "previous-index", "", "prior signed channel index")
	fs.IntVar(&in.Keep, "keep", 30, "number of nightly builds to advertise")
	fs.StringVar(&in.Channel, "channel", "", "channel the index is for (nightly or release)")
	fs.StringVar(&in.Edition, "edition", "", "edition the index is for (default: community, written as no edition)")
	fs.StringVar(&in.ArchivePrefix, "prefix", defaultArchivePrefix, "release archive name prefix (<prefix>-<id>-<os>-<arch>.tar.gz)")
	fs.StringVar(&in.DistDir, "dist", "dist", "directory holding gpuworks-<id>-<os>-<arch>.tar.gz")
	fs.StringVar(&in.OutDir, "out", "", "directory to write the tree into (must not exist)")
	fs.StringVar(&in.ReleaseID, "release-id", "", "expected release id (default: the one the archive names carry)")
	fs.StringVar(&in.Commit, "commit", os.Getenv("GPUWORKS_BUILD_COMMIT"), "commit the release was built from")
	fs.StringVar(&in.BuiltAt, "built-at", os.Getenv("GPUWORKS_BUILT_AT"), "build time")
	fs.StringVar(&in.KeyID, "key-id", envOr("GPUWORKS_DIST_SIGNING_KEY_ID", "gpuworks-dist-1"), "signing key id")
	fs.StringVar(&keyEnv, "key-env", "GPUWORKS_DIST_SIGNING_KEY", "environment variable holding the base64 ed25519 seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := distribution.PrivateKeyFromSeed(os.Getenv(keyEnv))
	if err != nil {
		return fmt.Errorf("$%s: %w", keyEnv, err)
	}
	in.Key = key
	in.PublishedAt = time.Now().UTC()
	in.ImageCatalog = imagecatalog.SeedJSON()
	in.TemplateArchive, in.TemplateDigest, in.TemplateInfo, err = seed.Published()
	if err != nil {
		return err
	}
	index, err := assemble(in)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "assembled %s: %s release %s (%d archives), image catalog %s, template catalog %s\n",
		in.OutDir, index.Channel, index.Server.ReleaseID, len(index.Server.Files), index.ImageCatalog.Version, index.TemplateCatalog.Version)
	return nil
}

// assemble writes the channel tree and returns the index it signed.
func assemble(in assembleInput) (distribution.Index, error) {
	if !distribution.ValidChannel(in.Channel) {
		return distribution.Index{}, fmt.Errorf("-channel %q is not a channel name", in.Channel)
	}
	if strings.TrimSpace(in.OutDir) == "" {
		return distribution.Index{}, errors.New("-out is required")
	}
	edition := strings.TrimSpace(in.Edition)
	if edition == distribution.EditionCommunity {
		// Community indexes name no edition, as they did before editions.
		edition = ""
	}
	if !distribution.ValidEdition(edition) {
		return distribution.Index{}, fmt.Errorf("-edition %q is not an edition name", in.Edition)
	}
	if _, err := archivePrefix(in.ArchivePrefix); err != nil {
		return distribution.Index{}, err
	}
	if _, err := os.Stat(in.OutDir); err == nil {
		return distribution.Index{}, fmt.Errorf("%s already exists; assemble writes a fresh tree", in.OutDir)
	}
	archives, releaseID, err := findArchives(in.DistDir, in.ArchivePrefix, in.ReleaseID)
	if err != nil {
		return distribution.Index{}, err
	}
	docs, err := readReleaseDocs(in.DistDir, releaseID)
	if err != nil {
		return distribution.Index{}, err
	}
	index := distribution.Index{
		Schema: distribution.IndexSchema, Channel: in.Channel, Edition: edition, PublishedAt: in.PublishedAt.UTC(),
		Server: distribution.ServerRelease{ReleaseID: releaseID, Commit: in.Commit, BuiltAt: in.BuiltAt},
	}

	releaseDir := "releases/" + releaseID
	var sums strings.Builder
	for _, archive := range archives {
		data, err := os.ReadFile(filepath.Join(in.DistDir, archive.name))
		if err != nil {
			return distribution.Index{}, err
		}
		file, err := writeBlob(in.OutDir, releaseDir+"/"+archive.name, data)
		if err != nil {
			return distribution.Index{}, err
		}
		fmt.Fprintf(&sums, "%s  %s\n", file.SHA256, archive.name)
		index.Server.Files = append(index.Server.Files, distribution.ReleaseFile{OS: archive.os, Arch: archive.arch, File: file})
	}
	if _, err := writeBlob(in.OutDir, releaseDir+"/SHA256SUMS", []byte(sums.String())); err != nil {
		return distribution.Index{}, err
	}
	for name, data := range docs {
		if _, err := writeBlob(in.OutDir, releaseDir+"/docs/"+name, data); err != nil {
			return distribution.Index{}, err
		}
	}

	images, err := manifest.Parse(in.ImageCatalog)
	if err != nil {
		return distribution.Index{}, fmt.Errorf("image catalog: %w", err)
	}
	imageFile, err := writeBlob(in.OutDir, "catalog/images/"+sha256Hex(in.ImageCatalog)+".json", in.ImageCatalog)
	if err != nil {
		return distribution.Index{}, err
	}
	index.ImageCatalog = &distribution.CatalogFile{Version: images.CatalogVersion, File: imageFile}
	if images.SchemaVersion == manifest.RunnerSchema {
		index.ImageCatalogV4 = index.ImageCatalog
		legacy, err := manifest.LegacyCatalog(in.ImageCatalog)
		if err != nil {
			return distribution.Index{}, err
		}
		desc, err := manifest.Parse(legacy)
		if err != nil {
			return distribution.Index{}, err
		}
		file, err := writeBlob(in.OutDir, "catalog/images/"+sha256Hex(legacy)+".json", legacy)
		if err != nil {
			return distribution.Index{}, err
		}
		index.ImageCatalog = &distribution.CatalogFile{Version: desc.CatalogVersion, File: file}
		in.ImageCatalog = legacy
	}
	// Keep the fixed channel URL compatible with older offline servers.
	// New servers obtain the full catalog through the signed index.
	if _, err := writeBlob(in.OutDir, in.Channel+"/manifest.json", in.ImageCatalog); err != nil {
		return distribution.Index{}, err
	}

	templateFile, err := writeBlob(in.OutDir, "catalog/templates/"+sha256Hex(in.TemplateArchive)+".tar.gz", in.TemplateArchive)
	if err != nil {
		return distribution.Index{}, err
	}
	index.TemplateCatalog = &distribution.TemplateCatalog{
		CatalogFile:   distribution.CatalogFile{Version: inferencecatalog.CatalogVersion(in.TemplateInfo.PublishedAt, in.TemplateDigest), File: templateFile},
		PackageDigest: in.TemplateDigest,
		SchemaVersion: in.TemplateInfo.SchemaVersion,
	}

	if !in.Adopted {
		index.Server.DatabaseMigration = in.DatabaseMigration
	}
	if in.Previous != "" {
		raw, err := os.ReadFile(in.Previous)
		if err != nil {
			return distribution.Index{}, err
		}
		keys := distribution.TrustedKeys()
		keys[in.KeyID] = in.Key.Public().(ed25519.PublicKey)
		data, _, err := distribution.Open(raw, keys)
		if err != nil {
			return distribution.Index{}, err
		}
		// The history is the same edition's: another edition's index is
		// refused like another channel's.
		previous, err := distribution.ParseIndexFor(data, in.Channel, edition)
		if err != nil {
			return distribution.Index{}, err
		}
		if in.Adopted {
			if original, ok := previous.FindServer(releaseID); ok && slices.Equal(original.Files, index.Server.Files) {
				index.Server.DatabaseMigration = original.DatabaseMigration
				index.Server.Commit = original.Commit
			}
		}
		for _, release := range previous.Servers() {
			if in.Channel == distribution.ChannelNightly && len(index.PreviousServers)+1 >= in.Keep {
				break
			}
			if release.ReleaseID != releaseID {
				index.PreviousServers = append(index.PreviousServers, release)
			}
		}
	}
	if err := index.Validate(); err != nil {
		return distribution.Index{}, fmt.Errorf("assembled index is invalid: %w", err)
	}
	payload, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return distribution.Index{}, err
	}
	envelope, err := distribution.Seal(payload, in.KeyID, in.Key)
	if err != nil {
		return distribution.Index{}, err
	}
	if _, err := writeBlob(in.OutDir, in.Channel+"/index.json", envelope); err != nil {
		return distribution.Index{}, err
	}
	if _, err := writeBlob(in.OutDir, in.Channel+"/latest", []byte(releaseID+"\n")); err != nil {
		return distribution.Index{}, err
	}
	return index, nil
}

type archiveName struct {
	name, os, arch string
}

// findArchives lists the release archives in dir named
// <prefix>-<id>-<os>-<arch>.tar.gz (an empty prefix is
// defaultArchivePrefix). Every one must carry the same release id; the two
// CI build jobs' dist directories are merged here, so their separate
// release.json files are not trusted.
func findArchives(dir, prefix, want string) ([]archiveName, string, error) {
	prefix, err := archivePrefix(prefix)
	if err != nil {
		return nil, "", err
	}
	pattern := archivePattern(prefix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	var found []archiveName
	releaseID := strings.TrimSpace(want)
	for _, entry := range entries {
		match := pattern.FindStringSubmatch(entry.Name())
		if match == nil || entry.IsDir() {
			continue
		}
		if releaseID == "" {
			releaseID = match[1]
		}
		if match[1] != releaseID {
			return nil, "", fmt.Errorf("%s is release %s, not %s", entry.Name(), match[1], releaseID)
		}
		found = append(found, archiveName{name: entry.Name(), os: match[2], arch: match[3]})
	}
	if len(found) == 0 {
		return nil, "", fmt.Errorf("%s holds no %s-<id>-<os>-<arch>.tar.gz", dir, prefix)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	return found, releaseID, nil
}

// writeBlob writes data at a tree-relative path and describes it.
func writeBlob(root, rel string, data []byte) (distribution.File, error) {
	target := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return distribution.File{}, err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return distribution.File{}, err
	}
	return distribution.File{Path: rel, Size: int64(len(data)), SHA256: sha256Hex(data)}, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
