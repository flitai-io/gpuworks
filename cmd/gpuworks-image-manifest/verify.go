package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
)

// The pre-publish check: every reference a manifest names, canonical and
// mirror alike, must pull whole. A registry can hold a tag whose platform
// manifests are gone (SWR deletes the platform manifests of a deleted tag
// even when another tag's index still lists them), and a catalog can name a
// revision nobody published; both reach users as a failed pull, so the check
// refuses to publish them.

const manifestAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

type verifier struct {
	pinnedSources map[string]string
	client        *http.Client
	// endpoint maps a registry host to the base URL its v2 API is served at.
	endpoint func(host string) string
	attempts int
	backoff  time.Duration
	sem      chan struct{}

	mu       sync.Mutex
	tokens   map[string]string // host + " " + repo -> anonymous bearer token
	contents map[string][]byte // manifest digest -> body that hashes to it
}

func newVerifier(client *http.Client, endpoint func(string) string) *verifier {
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	// A redirect from a blob HEAD points at the storage that holds the blob,
	// which already proves it exists; following it would only probe a CDN.
	redirectless := *client
	redirectless.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if endpoint == nil {
		endpoint = registryEndpoint
	}
	return &verifier{
		client: &redirectless, endpoint: endpoint, attempts: 3, backoff: time.Second,
		sem: make(chan struct{}, 16), tokens: map[string]string{}, contents: map[string][]byte{},
	}
}

func registryEndpoint(host string) string {
	if host == "docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + host
}

// verifyReport is what a check found. Problems are empty when every reference
// pulls whole.
type verifyReport struct {
	Refs      int
	Platforms int
	Blobs     int
	Problems  []string
}

// refResult is one reference's outcome: its platform manifests by platform
// (an image without an index is keyed "") and what was wrong with it.
type refResult struct {
	digest    string
	ref       string
	platforms map[string]string
	blobs     int
	problems  []string
}

// Release publication only needs registry metadata to validate the immutable
// source pins. The full verifier also checks platform manifests and layers.
func verifyPublicationPins(ctx context.Context, body []byte, v *verifier) (int, error) {
	m, err := manifest.Parse(body)
	if err != nil {
		return 0, err
	}
	checked := 0
	for _, s := range m.Series {
		for _, r := range s.Versions {
			if r.RegistryDigest == "" {
				continue
			}
			ref := v.pinnedSources[r.Name]
			host, repo, tag, err := splitRef(ref)
			if err != nil {
				return checked, err
			}
			actual, err := v.resolve(ctx, host, repo, tag)
			if err != nil {
				return checked, fmt.Errorf("%s: %w", ref, err)
			}
			if actual != r.RegistryDigest {
				return checked, fmt.Errorf("%s: registry digest %s, matrix pins %s", ref, actual, r.RegistryDigest)
			}
			checked++
		}
	}
	return checked, nil
}

func verifyManifest(ctx context.Context, data []byte, v *verifier) (verifyReport, error) {
	m, err := manifest.Parse(data)
	if err != nil {
		return verifyReport{}, err
	}
	type entry struct{ canonical, ref, digest, pinRef string }
	var entries []entry
	for _, series := range m.Series {
		for _, version := range series.Versions {
			pinRef := version.ImageRef
			if origin := v.pinnedSources[version.Name]; origin != "" {
				pinRef = origin
			}
			for _, ref := range append([]string{version.ImageRef}, version.RegistrySources...) {
				entries = append(entries, entry{canonical: version.ImageRef, ref: ref, digest: version.RegistryDigest, pinRef: pinRef})
			}
		}
	}
	// Docker Hub counts manifest GETs against an anonymous pull limit but not
	// HEADs, so it is checked last: by then the bodies it lists are cached by
	// digest from the mirrors, and it is only asked whether it has them.
	results := make(map[string]refResult, len(entries))
	var resultsMu sync.Mutex
	for _, hub := range []bool{false, true} {
		var wg sync.WaitGroup
		for _, e := range entries {
			if isDockerHub(e.ref) != hub {
				continue
			}
			wg.Add(1)
			go func(ref string) {
				defer wg.Done()
				result := v.checkRef(ctx, ref)
				resultsMu.Lock()
				results[ref] = result
				resultsMu.Unlock()
			}(e.ref)
		}
		wg.Wait()
	}

	var report verifyReport
	for _, e := range entries {
		result := results[e.ref]
		if e.digest != "" && e.ref == e.pinRef && result.digest != "" && result.digest != e.digest {
			report.Problems = append(report.Problems, fmt.Sprintf("%s: registry digest %s, matrix pins %s", e.ref, result.digest, e.digest))
		}
		report.Refs++
		report.Platforms += len(result.platforms)
		report.Blobs += result.blobs
		for _, problem := range result.problems {
			report.Problems = append(report.Problems, e.ref+": "+problem)
		}
		canonical := results[e.canonical]
		if e.ref == e.canonical || len(result.problems) > 0 || len(canonical.problems) > 0 {
			continue
		}
		if diff := platformDiff(canonical.platforms, result.platforms); diff != "" {
			report.Problems = append(report.Problems, fmt.Sprintf("%s: serves different content than %s: %s", e.ref, e.canonical, diff))
		}
	}
	return report, nil
}

func isDockerHub(ref string) bool {
	return strings.HasPrefix(pullref.NormalizeRef(ref), "docker.io/")
}

func platformDiff(want, got map[string]string) string {
	keys := map[string]bool{}
	for key := range want {
		keys[key] = true
	}
	for key := range got {
		keys[key] = true
	}
	var diffs []string
	for key := range keys {
		if want[key] != got[key] {
			diffs = append(diffs, fmt.Sprintf("%s %s vs %s", platformName(key), orNone(want[key]), orNone(got[key])))
		}
	}
	sort.Strings(diffs)
	return strings.Join(diffs, "; ")
}

func platformName(key string) string {
	if key == "" {
		return "image"
	}
	return key
}

func orNone(digest string) string {
	if digest == "" {
		return "none"
	}
	return digest
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type manifestBody struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    *descriptor  `json:"config"`
	Layers    []descriptor `json:"layers"`
}

func (v *verifier) checkRef(ctx context.Context, ref string) refResult {
	result := refResult{ref: ref, platforms: map[string]string{}}
	host, repo, reference, err := splitRef(ref)
	if err != nil {
		result.problems = append(result.problems, err.Error())
		return result
	}
	digest, err := v.resolve(ctx, host, repo, reference)
	if err != nil {
		if errors.Is(err, errNotFound) {
			err = errors.New("tag not found")
		}
		result.problems = append(result.problems, err.Error())
		return result
	}
	result.digest = digest
	top, err := v.body(ctx, host, repo, digest)
	if err != nil {
		result.problems = append(result.problems, err.Error())
		return result
	}
	images := map[string]manifestBody{}
	if len(top.Manifests) == 0 {
		images[""] = top
		result.platforms[""] = digest
	}
	for _, child := range top.Manifests {
		if child.Platform == nil || child.Platform.OS == "unknown" {
			continue // build attestations are not runnable images
		}
		key := child.Platform.OS + "/" + child.Platform.Architecture
		if child.Platform.Variant != "" {
			key += "/" + child.Platform.Variant
		}
		result.platforms[key] = child.Digest
		if _, err := v.resolve(ctx, host, repo, child.Digest); err != nil {
			result.problems = append(result.problems, fmt.Sprintf("%s manifest %s: %v", key, child.Digest, err))
			continue
		}
		body, err := v.body(ctx, host, repo, child.Digest)
		if err != nil {
			result.problems = append(result.problems, fmt.Sprintf("%s manifest %s: %v", key, child.Digest, err))
			continue
		}
		images[key] = body
	}

	blobPlatform := map[string]string{}
	for key, image := range images {
		if image.Config == nil {
			result.problems = append(result.problems, fmt.Sprintf("%s manifest has no config", platformName(key)))
			continue
		}
		for _, blob := range append([]descriptor{*image.Config}, image.Layers...) {
			if _, seen := blobPlatform[blob.Digest]; !seen {
				blobPlatform[blob.Digest] = key
			}
		}
	}
	var wg sync.WaitGroup
	var problemsMu sync.Mutex
	for blob, key := range blobPlatform {
		wg.Add(1)
		go func(blob, key string) {
			defer wg.Done()
			if err := v.headBlob(ctx, host, repo, blob); err != nil {
				problemsMu.Lock()
				result.problems = append(result.problems, fmt.Sprintf("%s blob %s: %v", platformName(key), blob, err))
				problemsMu.Unlock()
			}
		}(blob, key)
	}
	wg.Wait()
	result.blobs = len(blobPlatform)
	sort.Strings(result.problems)
	return result
}

// splitRef returns the registry host, repository and tag or digest of ref.
func splitRef(ref string) (host, repo, reference string, err error) {
	full := pullref.NormalizeRef(ref)
	host, path, _ := strings.Cut(full, "/")
	if at := strings.LastIndex(path, "@"); at >= 0 {
		return host, path[:at], path[at+1:], nil
	}
	slash := strings.LastIndex(path, "/")
	colon := strings.LastIndex(path, ":")
	if colon <= slash || colon == len(path)-1 {
		return "", "", "", fmt.Errorf("reference %q names no tag", ref)
	}
	return host, path[:colon], path[colon+1:], nil
}

var (
	errNotFound         = errors.New("not found")
	errAnonymousRefused = errors.New("registry refuses anonymous pulls")
)

// resolve asks the registry for reference without downloading it and returns
// the digest it serves.
func (v *verifier) resolve(ctx context.Context, host, repo, reference string) (string, error) {
	resp, err := v.request(ctx, http.MethodHead, host, repo, "/manifests/"+reference, manifestAccept)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", errNotFound
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("HEAD manifest: HTTP %d", resp.StatusCode)
	}
	if digest := resp.Header.Get("Docker-Content-Digest"); digest != "" {
		return digest, nil
	}
	if strings.HasPrefix(reference, "sha256:") {
		return reference, nil
	}
	body, err := v.get(ctx, host, repo, reference)
	if err != nil {
		return "", err
	}
	return sha256Digest(body), nil
}

// body returns the manifest with digest, from the cache when another registry
// already served the same bytes.
func (v *verifier) body(ctx context.Context, host, repo, digest string) (manifestBody, error) {
	v.mu.Lock()
	raw, cached := v.contents[digest]
	v.mu.Unlock()
	if !cached {
		var err error
		if raw, err = v.get(ctx, host, repo, digest); err != nil {
			return manifestBody{}, err
		}
		if got := sha256Digest(raw); got != digest {
			return manifestBody{}, fmt.Errorf("manifest %s hashes to %s", digest, got)
		}
		v.mu.Lock()
		v.contents[digest] = raw
		v.mu.Unlock()
	}
	var body manifestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return manifestBody{}, fmt.Errorf("manifest %s: %w", digest, err)
	}
	return body, nil
}

func (v *verifier) get(ctx context.Context, host, repo, reference string) ([]byte, error) {
	resp, err := v.request(ctx, http.MethodGet, host, repo, "/manifests/"+reference, manifestAccept)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET manifest: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func (v *verifier) headBlob(ctx context.Context, host, repo, digest string) error {
	resp, err := v.request(ctx, http.MethodHead, host, repo, "/blobs/"+digest, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	default:
		return fmt.Errorf("HEAD blob: HTTP %d", resp.StatusCode)
	}
}

// request sends one registry API call with the anonymous pull token, fetching
// it on the registry's challenge, and retries what a busy registry answers
// transiently.
func (v *verifier) request(ctx context.Context, method, host, repo, path, accept string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < v.attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(v.backoff * time.Duration(attempt)):
			}
		}
		resp, err := v.send(ctx, method, host, repo, path, accept)
		if err == nil && resp.StatusCode == http.StatusUnauthorized {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if err = v.authorize(ctx, host, repo, challenge); errors.Is(err, errAnonymousRefused) {
				return nil, err
			}
			if err == nil {
				resp, err = v.send(ctx, method, host, repo, path, accept)
			}
		}
		if err == nil && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return resp, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
			resp.Body.Close()
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

func (v *verifier) send(ctx context.Context, method, host, repo, path, accept string) (*http.Response, error) {
	select {
	case v.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-v.sem }()
	req, err := http.NewRequestWithContext(ctx, method, v.endpoint(host)+"/v2/"+repo+path, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	v.mu.Lock()
	token := v.tokens[host+" "+repo]
	v.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return v.client.Do(req)
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// authorize fetches the anonymous pull token a Bearer challenge offers.
func (v *verifier) authorize(ctx context.Context, host, repo, challenge string) error {
	scheme, _, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("%w (challenge %q)", errAnonymousRefused, challenge)
	}
	params := map[string]string{}
	for _, match := range challengeParam.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(match[1])] = match[2]
	}
	if params["realm"] == "" {
		return fmt.Errorf("bearer challenge without realm: %q", challenge)
	}
	query := url.Values{"scope": {"repository:" + repo + ":pull"}}
	if params["service"] != "" {
		query.Set("service", params["service"])
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, params["realm"]+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anonymous token: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("anonymous token: %w", err)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return errors.New("anonymous token: empty")
	}
	v.mu.Lock()
	v.tokens[host+" "+repo] = token
	v.mu.Unlock()
	return nil
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
