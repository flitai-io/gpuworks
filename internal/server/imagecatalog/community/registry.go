package community

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

const (
	// requestTimeout bounds one page request to a registry.
	requestTimeout = 8 * time.Second
	// maxPages caps how far a tag list is followed: at 100 tags a
	// page that is a thousand tags, more than any repository a user browses
	// by name, and it bounds how long one browse can hold a request.
	maxPages = 10
	// browseTimeout bounds a whole browse, every page included.
	browseTimeout = maxPages * requestTimeout
	// maxBody caps one page's response body.
	maxBody = 4 << 20
)

// errCredentials reports a registry that refused an anonymous tag
// list, or rejected the configured login.
var errCredentials = errors.New("community registry requires credentials")

// ErrRepositoryNotFound identifies a repository the upstream registry cannot find.
var ErrRepositoryNotFound = errors.New("community repository not found")

// inferRegistry picks the tag-list API from a repository path: a
// path without a registry host is on Docker Hub, quay.io has its own API, and
// every other host is asked through the OCI distribution API.
func inferRegistry(repo string) string {
	switch host, _ := splitHost(repo); host {
	case "docker.io":
		return RegistryDockerHub
	case "quay.io":
		return RegistryQuay
	default:
		return RegistryOCI
	}
}

// splitHost names a repository's registry host and its path there, in the
// registry's own normalized spelling: Docker Hub repositories (written with or
// without a host) come back as docker.io and namespace/name.
func splitHost(repo string) (host, path string) {
	repo = strings.TrimSpace(repo)
	for _, alias := range []string{"index.docker.io/", "registry-1.docker.io/"} {
		if strings.HasPrefix(repo, alias) {
			repo = "docker.io/" + strings.TrimPrefix(repo, alias)
		}
	}
	host, path, _ = strings.Cut(pullref.NormalizeRef(repo), "/")
	return strings.ToLower(host), path
}

func registryOf(row Repo) string {
	if registry := strings.TrimSpace(row.Registry); registry != "" {
		return registry
	}
	return inferRegistry(row.Repo)
}

// tagListURL is the initial request used by each registry dialect. Overrides
// belong to the resolved repository, including lookup's sibling semantics.
func tagListURL(row Repo) string {
	if target := strings.TrimSpace(row.UpstreamAPI); target != "" {
		return target
	}
	host, path := splitHost(row.Repo)
	switch registryOf(row) {
	case RegistryDockerHub:
		return "https://hub.docker.com/v2/repositories/" + path + "/tags"
	case RegistryQuay:
		return "https://quay.io/api/v1/repository/" + path + "/tag/"
	case RegistryOCI:
		return "https://" + host + "/v2/" + path + "/tags/list?n=1000"
	default:
		return ""
	}
}

// AccessHost names the initial tag API host without exposing its configured
// path, query, credentials or port. Unknown or unusable targets stay empty.
func (row Repo) AccessHost() string {
	target, err := url.Parse(tagListURL(row))
	if err != nil || target.Scheme != "https" || target.User != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return ""
		}
		return address.Unmap().String()
	}
	for _, ch := range host {
		if ch > 127 {
			return ""
		}
	}
	return host
}

// listTags returns every tag the registry lists for row.Repo, in the
// registry's order, following pagination up to maxPages.
func (b *Browser) listTags(ctx context.Context, row Repo) ([]upstreamTag, error) {
	switch registryOf(row) {
	case RegistryDockerHub:
		return b.listDockerHubTags(ctx, row)
	case RegistryQuay:
		return b.listQuayTags(ctx, row)
	case RegistryOCI:
		return b.listOCITags(ctx, row)
	default:
		return nil, fmt.Errorf("unknown community registry %q", row.Registry)
	}
}

func (b *Browser) listDockerHubTags(ctx context.Context, row Repo) ([]upstreamTag, error) {
	next := tagListURL(row)
	if !strings.Contains(next, "?") {
		next += "?page_size=100"
	}
	var out []upstreamTag
	for page := 0; page < maxPages && next != ""; page++ {
		var payload struct {
			Next    *string `json:"next"`
			Results []struct {
				Name        string `json:"name"`
				LastUpdated string `json:"last_updated"`
			} `json:"results"`
		}
		if _, err := b.getJSON(ctx, next, nil, &payload); err != nil {
			return nil, err
		}
		for _, item := range payload.Results {
			out = append(out, upstreamTag{Name: item.Name, LastUpdated: item.LastUpdated})
		}
		next = ""
		if payload.Next != nil {
			next = strings.TrimSpace(*payload.Next)
		}
	}
	return out, nil
}

func (b *Browser) listQuayTags(ctx context.Context, row Repo) ([]upstreamTag, error) {
	base := tagListURL(row)
	var out []upstreamTag
	for page := 1; page <= maxPages; page++ {
		pageURL, err := url.Parse(base)
		if err != nil {
			return nil, err
		}
		query := pageURL.Query()
		query.Set("limit", "100")
		query.Set("page", strconv.Itoa(page))
		query.Set("onlyActiveTags", "true")
		pageURL.RawQuery = query.Encode()
		var payload struct {
			HasAdditional bool `json:"has_additional"`
			Tags          []struct {
				Name         string `json:"name"`
				LastModified string `json:"last_modified"`
			} `json:"tags"`
		}
		if _, err := b.getJSON(ctx, pageURL.String(), nil, &payload); err != nil {
			return nil, err
		}
		for _, item := range payload.Tags {
			out = append(out, upstreamTag{Name: item.Name, LastUpdated: item.LastModified})
		}
		if !payload.HasAdditional {
			break
		}
	}
	return out, nil
}

// listOCITags reads the distribution API's tag list. A registry that answers
// 401 with a Bearer challenge is asked for an anonymous token once; harbor and
// most public registries hand one out for public repositories.
func (b *Browser) listOCITags(ctx context.Context, row Repo) ([]upstreamTag, error) {
	next := tagListURL(row)
	origin, err := url.Parse(next)
	if err != nil {
		return nil, err
	}
	var out []upstreamTag
	var authorization string
	for page := 0; page < maxPages && next != ""; page++ {
		pageURL, err := url.Parse(next)
		if err != nil || !sameOrigin(origin, pageURL) || pageURL.User != nil {
			return nil, errors.New("registry pagination changed origin")
		}
		var payload struct {
			Tags []string `json:"tags"`
		}
		header, err := b.getJSON(ctx, next, &authorization, &payload)
		if err != nil {
			return nil, err
		}
		for _, name := range payload.Tags {
			out = append(out, upstreamTag{Name: name})
		}
		next, err = nextLink(next, header.Get("Link"))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

var linkNextPattern = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// nextLink resolves the RFC 8288 rel="next" target of a distribution API
// response against the page it came from.
func nextLink(current, header string) (string, error) {
	match := linkNextPattern.FindStringSubmatch(header)
	if match == nil {
		return "", nil
	}
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(match[1])
	if err != nil {
		return "", err
	}
	return base.ResolveReference(target).String(), nil
}

// getCommunityJSON fetches one page and decodes it into out. When bearer is
// non-nil the request carries it, and a 401 Bearer challenge is answered by
// fetching an anonymous token into it and retrying once.
func (b *Browser) getJSON(ctx context.Context, target string, bearer *string, out any) (http.Header, error) {
	response, err := b.do(ctx, target, bearer)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && bearer != nil {
		challenge := response.Header.Get("WWW-Authenticate")
		response.Body.Close()
		token, tokenErr := b.anonymousToken(ctx, challenge)
		if tokenErr != nil {
			return nil, tokenErr
		}
		*bearer = "Bearer " + token
		if response, err = b.do(ctx, target, bearer); err != nil {
			return nil, err
		}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, errCredentials
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("registry returned %s: %w", response.Status, ErrRepositoryNotFound)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("registry returned %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxBody)).Decode(out); err != nil {
		return nil, fmt.Errorf("decode registry response: %w", err)
	}
	return response.Header, nil
}

func (b *Browser) do(ctx context.Context, target string, bearer *string) (*http.Response, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	applyRegistryLogin(request)
	if bearer != nil && *bearer != "" {
		request.Header.Set("Authorization", *bearer)
	}
	response, err := b.client.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

var challengeParamPattern = regexp.MustCompile(`(\w+)="([^"]*)"`)

// anonymousRegistryToken answers a `Bearer realm=…,service=…,scope=…`
// challenge without credentials (the Docker token protocol).
func (b *Browser) anonymousToken(ctx context.Context, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", errCredentials
	}
	values := map[string]string{}
	for _, match := range challengeParamPattern.FindAllStringSubmatch(params, -1) {
		values[strings.ToLower(match[1])] = match[2]
	}
	realm, err := url.Parse(values["realm"])
	if err != nil || values["realm"] == "" || realm.Scheme != "https" {
		return "", errCredentials
	}
	query := realm.Query()
	for _, key := range []string{"service", "scope"} {
		if values[key] != "" {
			query.Set(key, values[key])
		}
	}
	realm.RawQuery = query.Encode()
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if _, err := b.getJSON(ctx, realm.String(), nil, &payload); err != nil {
		// A missing token endpoint says nothing about repository existence.
		if errors.Is(err, ErrRepositoryNotFound) {
			return "", errors.New("anonymous registry token endpoint returned 404 Not Found")
		}
		if errors.Is(err, errCredentials) {
			return "", err
		}
		return "", fmt.Errorf("anonymous registry token: %w", err)
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}
	return "", errCredentials
}
