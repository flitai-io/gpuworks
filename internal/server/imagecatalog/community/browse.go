// Package community lists the tags of community image repositories (Docker
// Hub, quay.io, and any OCI distribution registry) for the image catalog's
// community tab. It is a leaf: it knows registry APIs and the built-in
// repository list, and nothing about persistence or HTTP serving.
package community

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Registry kinds. Each lists tags through a different API.
const (
	RegistryDockerHub = "docker-hub"
	RegistryQuay      = "quay"
	RegistryOCI       = "oci"
)

// cacheTTL is how long one repository's tag list is served from memory before
// the upstream registry is asked again.
const cacheTTL = time.Hour

// Repository paths use OCI name components, without an image tag or digest.
var repositoryPath = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

// Repo is one community repository: a card in the community tab, or the
// result of browsing one repository's tags.
type Repo struct {
	Repo string `json:"repo"`
	// Registry selects the tag-list API; empty means inferred from Repo.
	Registry string `json:"registry,omitempty"`
	// UpstreamAPI overrides the first-page URL of the tag list.
	UpstreamAPI string `json:"upstream_api,omitempty"`
	// Engine may be empty for repositories containing several engine families.
	Engine    string `json:"engine"`
	Framework string `json:"framework,omitempty"`
	// Siblings are further repositories shown on the same card. Each lists
	// its own tags and inherits this card's engine and framework.
	Siblings []string `json:"siblings,omitempty"`
	Tags     []Tag    `json:"tags,omitempty"`
}

type Tag struct {
	Tag              string `json:"tag"`
	Engine           string `json:"engine,omitempty"`
	EngineVersion    string `json:"engine_version,omitempty"`
	Framework        string `json:"framework,omitempty"`
	FrameworkVersion string `json:"framework_version,omitempty"`
	Arch             string `json:"arch,omitempty"`
	RollingTag       bool   `json:"rolling_tag,omitempty"`
	UpstreamGone     bool   `json:"upstream_gone,omitempty"`
	// LastUpdated is the registry's own timestamp for the tag, when its API
	// reports one (Docker Hub and quay do, the OCI tag list does not).
	LastUpdated string `json:"last_updated,omitempty"`
}

// Catalog is the shape of the built-in repository list and of the
// image_community_catalog setting that overlays it.
type Catalog struct {
	SchemaVersion  int    `json:"schema_version,omitempty"`
	CatalogVersion string `json:"catalog_version,omitempty"`
	Repos          []Repo `json:"repos"`
}

// IsRollingTag identifies tags whose content moves upstream without the tag
// changing. Numbered and release-candidate tags stay immutable-looking entries.
func IsRollingTag(tagOrRef string) bool {
	value := strings.TrimSpace(tagOrRef)
	if colon := strings.LastIndex(value, ":"); colon >= 0 && colon+1 < len(value) && !strings.Contains(value[colon+1:], "/") {
		value = value[colon+1:]
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "latest", "main", "master", "stable", "nightly", "dev", "edge":
		return true
	default:
		return false
	}
}

//go:embed catalog.json
var builtinCatalogJSON []byte

// HTTPClient is what a Browser sends registry requests through.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Browser lists community tags and keeps the last successful list per
// repository. The zero value is not usable; use NewBrowser.
type Browser struct {
	client HTTPClient

	// Credentials reads the current login for an exact registry host.
	// Configure it before using the browser; secrets never enter catalog rows.
	Credentials func(host string) (username, secret string, err error)

	mu      sync.Mutex
	entries map[string]cacheEntry
}

// cacheEntry is one repository's last successful list. fingerprint is the
// repository's resolved configuration: an administrator who changes where a
// repository is listed from must not be served the old registry's tags.
type cacheEntry struct {
	fingerprint string
	tags        []Tag
	refreshed   time.Time
}

// NewBrowser returns a Browser that sends requests through client, or the
// standard client when nil.
func NewBrowser(client HTTPClient) *Browser {
	if client == nil {
		client = http.DefaultClient
	}
	return &Browser{client: scopedRedirectClient(client), entries: map[string]cacheEntry{}}
}

// Repos returns the built-in community repositories with the
// administrator-configured ones overlaid by repo (a configured entry replaces
// the built-in one; new ones are appended). Tags are not filled in.
func Repos(configured []Repo) ([]Repo, error) {
	if err := ValidateRepos(configured); err != nil {
		return nil, err
	}
	var catalog Catalog
	if err := json.Unmarshal(builtinCatalogJSON, &catalog); err != nil {
		return nil, err
	}
	out := make([]Repo, 0, len(catalog.Repos)+len(configured))
	index := map[string]int{}
	for _, row := range append(catalog.Repos, configured...) {
		row.Repo = strings.TrimSpace(row.Repo)
		row.Registry = registryOf(row)
		row.Tags = nil
		if i, ok := index[row.Repo]; ok {
			out[i] = row
			continue
		}
		index[row.Repo] = len(out)
		out = append(out, row)
	}
	return out, nil
}

// lookup resolves the repository a browse names: a card, a sibling on a card
// (which inherits the card's engine and framework), or any other repository,
// whose registry is inferred from its host.
func lookup(repo string, repos []Repo) Repo {
	for _, row := range repos {
		if row.Repo == repo {
			return row
		}
	}
	for _, row := range repos {
		for _, sibling := range row.Siblings {
			if strings.TrimSpace(sibling) == repo {
				return Repo{Repo: repo, Registry: inferRegistry(repo), Engine: row.Engine, Framework: row.Framework}
			}
		}
	}
	return Repo{Repo: repo, Registry: inferRegistry(repo)}
}

func fingerprint(row Repo) string {
	encoded, _ := json.Marshal([]string{row.Repo, row.Registry, row.UpstreamAPI})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

// Browse lists every tag the upstream registry has for repo. No tag is
// filtered out: release tags, model-specific tags such as "qwen38", and rolling
// tags such as "nightly" are all offered, and the client searches them by name.
// A successful list is cached for an hour per repository and configuration.
func (b *Browser) Browse(ctx context.Context, repo string, configured []Repo) (Repo, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return Repo{}, fmt.Errorf("repo is required")
	}
	_, path := splitHost(repo)
	if strings.Contains(repo, "://") || strings.ContainsAny(repo, " \t\r\n?#@\\") || strings.HasSuffix(repo, "/") || !repositoryPath.MatchString(path) {
		return Repo{}, fmt.Errorf("invalid repository %q: expected a repository name without a tag or digest", repo)
	}
	repos, err := Repos(configured)
	if err != nil {
		return Repo{}, err
	}
	row := lookup(repo, repos)
	row.Tags = nil
	ctx, authKey, err := b.authContext(ctx, row)
	if err != nil {
		return row, err
	}
	key := fingerprint(row) + authKey

	b.mu.Lock()
	entry, cached := b.entries[repo]
	b.mu.Unlock()
	if cached && entry.fingerprint == key && time.Since(entry.refreshed) < cacheTTL {
		row.Tags = append([]Tag(nil), entry.tags...)
		return row, nil
	}
	var previous []Tag
	if cached && entry.fingerprint == key {
		previous = entry.tags
	}

	requestCtx, cancel := context.WithTimeout(ctx, browseTimeout)
	defer cancel()
	upstream, err := b.listTags(requestCtx, row)
	if err != nil {
		return row, fmt.Errorf("list tags of %s: %w", repo, err)
	}
	tags := mergeTags(previous, upstream)
	b.mu.Lock()
	b.entries[repo] = cacheEntry{fingerprint: key, tags: tags, refreshed: time.Now()}
	b.mu.Unlock()
	row.Tags = append([]Tag(nil), tags...)
	return row, nil
}

// ValidateRepos validates only the metadata needed to safely perform a
// server-side upstream refresh. Empty input means use the built-in list.
func ValidateRepos(repos []Repo) error {
	seen := make(map[string]bool, len(repos))
	for _, row := range repos {
		row.Repo = strings.TrimSpace(row.Repo)
		if row.Repo == "" {
			return fmt.Errorf("community repo is required")
		}
		if seen[row.Repo] {
			return fmt.Errorf("duplicate community repo %q", row.Repo)
		}
		seen[row.Repo] = true
		switch strings.TrimSpace(row.Registry) {
		case "", RegistryDockerHub, RegistryQuay, RegistryOCI:
		default:
			return fmt.Errorf("community repo %q has unknown registry %q", row.Repo, row.Registry)
		}
		endpoint := strings.TrimSpace(row.UpstreamAPI)
		if endpoint == "" {
			continue
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return fmt.Errorf("community repo %q has invalid upstream_api", row.Repo)
		}
	}
	return nil
}

// upstreamTag is one tag as a registry listed it.
type upstreamTag struct {
	Name        string
	LastUpdated string
}

// mergeTags orders tags as the registry listed them. A tag that was in the
// previous successful list but is gone upstream stays visible, marked, so an
// operator can still see what a catalog row of theirs pointed at.
func mergeTags(previous []Tag, upstream []upstreamTag) []Tag {
	seen := make(map[string]bool, len(upstream))
	merged := make([]Tag, 0, len(upstream)+len(previous))
	for _, item := range upstream {
		name := strings.TrimSpace(item.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		tag := inferTagMetadata(name)
		tag.LastUpdated = item.LastUpdated
		merged = append(merged, tag)
	}
	for _, tag := range previous {
		name := strings.TrimSpace(tag.Tag)
		if name == "" || seen[name] {
			continue
		}
		tag.Tag = name
		tag.UpstreamGone = true
		merged = append(merged, tag)
	}
	return merged
}
