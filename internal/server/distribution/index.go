// Package distribution fetches what GPUWorks publishes — server releases, the
// system image catalog and the built-in inference template catalog — from a
// channel mirrored on several sources.
//
// Trust comes from the signed index, not from the source: every source is
// asked at once, the first index whose signature verifies wins, and every file
// is checked against the size and sha256 the index lists. A source is only
// transport, so a customer mirror or an offline copy is as good as the
// published ones (ADR 0040).
package distribution

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The channels GPUWorks publishes (ADR 0040): every master build goes to
// nightly, every v* tag to release.
const (
	ChannelNightly = "nightly"
	ChannelRelease = "release"
)

// Channels lists the published channels, nightly first.
var Channels = []string{ChannelNightly, ChannelRelease}

// KnownChannel reports whether name is one of the published channels.
func KnownChannel(name string) bool {
	for _, channel := range Channels {
		if name == channel {
			return true
		}
	}
	return false
}

// IndexSchema is the index layout this client understands. Fields added later
// are ignored; a change old clients must not misread bumps the schema.
const IndexSchema = 1

// Index is one channel's signed payload: what the channel currently offers.
type Index struct {
	Schema      int           `json:"schema"`
	Channel     string        `json:"channel"`
	PublishedAt time.Time     `json:"published_at"`
	Server      ServerRelease `json:"server"`
	// ImageCatalog and TemplateCatalog are absent when a channel publishes no
	// catalog of that kind.
	ImageCatalog    *CatalogFile     `json:"image_catalog,omitempty"`
	TemplateCatalog *TemplateCatalog `json:"template_catalog,omitempty"`
}

// ServerRelease is the server build a channel offers, one archive per platform.
type ServerRelease struct {
	ReleaseID string        `json:"release_id"`
	Commit    string        `json:"commit"`
	BuiltAt   string        `json:"built_at"`
	Files     []ReleaseFile `json:"files"`
}

// File is one published file, addressed relative to a source's base URL.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ReleaseFile is a server archive for one platform.
type ReleaseFile struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	File
}

// CatalogFile is a published catalog. Version is for display and equality: it
// is never ordered (ADR 0040).
type CatalogFile struct {
	Version string `json:"version"`
	File
}

// TemplateCatalog is the published inference template catalog package.
// PackageDigest is the package's content identity (what the database records
// for its upstream specs); SHA256 covers the archive bytes.
type TemplateCatalog struct {
	CatalogFile
	PackageDigest string `json:"package_digest"`
	SchemaVersion int    `json:"schema_version"`
}

var (
	channelPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	releaseIDPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	sha256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidChannel reports whether name can be a channel.
func ValidChannel(name string) bool { return channelPattern.MatchString(name) }

// ParseIndex decodes and validates a verified payload for the channel that was
// asked for. A payload naming another channel is refused: a source must not be
// able to answer a release request with a nightly build.
func ParseIndex(payload []byte, channel string) (Index, error) {
	var index Index
	if err := json.Unmarshal(payload, &index); err != nil {
		return Index{}, fmt.Errorf("decode index: %w", err)
	}
	if err := index.Validate(); err != nil {
		return Index{}, err
	}
	if index.Channel != channel {
		return Index{}, fmt.Errorf("index is for channel %q, not %q", index.Channel, channel)
	}
	return index, nil
}

// Validate checks the shape of an index: the schema, the channel, and that
// every file it names is a safe relative path with a size and a digest.
func (x Index) Validate() error {
	if x.Schema != IndexSchema {
		return fmt.Errorf("index schema %d is not supported (this build reads schema %d)", x.Schema, IndexSchema)
	}
	if !ValidChannel(x.Channel) {
		return fmt.Errorf("index channel %q is invalid", x.Channel)
	}
	if err := x.Server.validate(); err != nil {
		return err
	}
	if x.ImageCatalog != nil {
		if err := x.ImageCatalog.validate("image catalog"); err != nil {
			return err
		}
	}
	if t := x.TemplateCatalog; t != nil {
		if err := t.CatalogFile.validate("template catalog"); err != nil {
			return err
		}
		if !sha256Pattern.MatchString(t.PackageDigest) {
			return fmt.Errorf("template catalog package_digest %q is not a sha256", t.PackageDigest)
		}
		if t.SchemaVersion <= 0 {
			return fmt.Errorf("template catalog schema_version must be positive")
		}
	}
	return nil
}

func (r ServerRelease) validate() error {
	if !releaseIDPattern.MatchString(r.ReleaseID) {
		return fmt.Errorf("index release_id %q is invalid", r.ReleaseID)
	}
	seen := make(map[string]bool, len(r.Files))
	for _, f := range r.Files {
		if f.OS == "" || f.Arch == "" {
			return fmt.Errorf("release file %s has no os/arch", f.Path)
		}
		key := f.OS + "/" + f.Arch
		if seen[key] {
			return fmt.Errorf("release lists %s twice", key)
		}
		seen[key] = true
		if err := f.File.validate(); err != nil {
			return fmt.Errorf("release file %s: %w", key, err)
		}
	}
	return nil
}

func (c CatalogFile) validate(what string) error {
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("%s has no version", what)
	}
	if err := c.File.validate(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// validate refuses anything a client could be steered by: absolute paths,
// parent references, URLs or backslashes in the path, and a file without a
// size or a full digest.
func (f File) validate() error {
	p := f.Path
	switch {
	case p == "", strings.HasPrefix(p, "/"), strings.Contains(p, "\\"), strings.Contains(p, "://"),
		path.Clean(p) != p, p == "..", strings.HasPrefix(p, "../"):
		return fmt.Errorf("path %q is not a plain relative path", p)
	case f.Size <= 0:
		return fmt.Errorf("%s has no size", p)
	case !sha256Pattern.MatchString(f.SHA256):
		return fmt.Errorf("%s sha256 %q is not 64 lowercase hex digits", p, f.SHA256)
	}
	return nil
}

// NewerThan reports whether this release is an update for a server running
// releaseID built at builtAt: a different release that orders after it (see
// compareBuilds). Moving from a nightly build to the release channel must not
// offer the older release as an update. When the two cannot be ordered (a
// local build) any other release counts as newer.
func (r ServerRelease) NewerThan(releaseID, builtAt string) bool {
	if r.ReleaseID == releaseID {
		return false
	}
	order, ok := compareBuilds(r.ReleaseID, r.BuiltAt, releaseID, builtAt)
	return !ok || order > 0
}

// OlderThan reports whether this release orders before a server running
// releaseID built at builtAt (see compareBuilds). The catalogs a channel
// publishes come from its release's build, so a channel whose release is
// older than the running server offers catalogs no newer than the ones that
// server embeds. When the two cannot be ordered (a local build) nothing is
// known to be older.
func (r ServerRelease) OlderThan(releaseID, builtAt string) bool {
	order, ok := compareBuilds(r.ReleaseID, r.BuiltAt, releaseID, builtAt)
	return ok && order < 0
}

// VersionChange classifies a replacement using only the two reported IDs.
func VersionChange(current, target string) string {
	if current == target {
		return ""
	}
	order, known := compareBuilds(target, "", current, "")
	switch {
	case known && order > 0:
		return "upgrade"
	case known && order < 0:
		return "downgrade"
	default:
		return "different_build"
	}
}

// compareBuilds orders two builds. Two version tags (vYY.M.PATCH, see
// docs/dev/release.md#版本号) order by version, so a patch to an older
// release line built after a newer line is still older. Anything else, such
// as a nightly's short commit, orders by build time. ok is false when the
// times cannot be read.
func compareBuilds(theirID, theirBuilt, ourID, ourBuilt string) (order int, ok bool) {
	if theirs, ok := parseReleaseVersion(theirID); ok {
		if ours, ok := parseReleaseVersion(ourID); ok {
			return slices.Compare(theirs[:], ours[:]), true
		}
	}
	theirTime, err := time.Parse(time.RFC3339, strings.TrimSpace(theirBuilt))
	if err != nil {
		return 0, false
	}
	ourTime, err := time.Parse(time.RFC3339, strings.TrimSpace(ourBuilt))
	if err != nil {
		return 0, false
	}
	return theirTime.Compare(ourTime), true
}

var releaseVersionPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// IsReleaseVersion reports whether id is a release tag vYY.M.PATCH, as the
// release channel publishes, rather than a nightly build's short commit.
func IsReleaseVersion(id string) bool {
	_, ok := parseReleaseVersion(id)
	return ok
}

// parseReleaseVersion reads a release tag vYY.M.PATCH. A pre-release or a
// git describe suffix is not a release version.
func parseReleaseVersion(id string) ([3]int, bool) {
	m := releaseVersionPattern.FindStringSubmatch(id)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

// Asset picks the server archive for a platform.
func (r ServerRelease) Asset(goos, goarch string) (ReleaseFile, error) {
	for _, f := range r.Files {
		if f.OS == goos && f.Arch == goarch {
			return f, nil
		}
	}
	return ReleaseFile{}, fmt.Errorf("release %s has no %s/%s build", r.ReleaseID, goos, goarch)
}
