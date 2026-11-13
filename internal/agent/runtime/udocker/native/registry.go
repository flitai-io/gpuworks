package native

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// Descriptor is the subset of an OCI content descriptor this package needs.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Manifest is a v2 schema 2 / OCI image manifest.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Image is a resolved image: the platform manifest exactly as the registry
// served it (udocker stores these bytes verbatim) plus its parsed form.
type Image struct {
	Ref          Ref
	Manifest     Manifest
	ManifestJSON []byte
}

const manifestAccept = "application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.oci.image.index.v1+json"

// Client talks to one registry. Tokens are cached because a large image pulls
// for longer than a single request but well within a token's lifetime, and
// re-authenticating per blob (as the previous prefetcher did) adds a round
// trip to every layer.
type Client struct {
	HTTP     *http.Client
	Platform Platform
	// Insecure overrides which registries may skip verified TLS (see
	// insecure.go). Nil means loopback plus GPUWORKS_INSECURE_REGISTRIES.
	Insecure func(host string) bool

	mu           sync.Mutex
	tokens       map[string]tokenEntry
	schemes      map[string]string // probed scheme per insecure registry
	insecureHTTP *http.Client
	insecureBase *http.Client
}

// Platform selects a manifest from a multi-architecture index.
type Platform struct {
	OS           string
	Architecture string
}

type tokenEntry struct {
	token   string
	expires time.Time
}

// NewClient returns a Client for linux/amd64 with sane transport limits. The
// per-request timeout is deliberately absent: blob GETs stream for minutes on
// throttled links and a whole-request deadline would kill them mid-transfer.
// Stall detection lives in the download path instead.
func NewClient() *Client {
	return NewClientForPlatform("linux", goruntime.GOARCH)
}

// NewClientForPlatform creates a registry client that resolves multi-arch
// images for the requested host platform. Native udocker pulls must select the
// agent architecture; defaulting to amd64 breaks ARM64 hosts without qemu.
func NewClientForPlatform(goos, goarch string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = RequestProxy
	transport.MaxIdleConnsPerHost = MaxConnectionsLimit
	transport.ResponseHeaderTimeout = 60 * time.Second
	// HTTP/1.1 only. Over HTTP/2 every request to a host shares one TCP
	// connection, so the range parts of a blob (and every layer in flight)
	// would be streams of a single connection: the split would add no
	// parallelism a link could use, and when that connection degrades -- a CDN
	// edge throttling it to a trickle -- reconnecting a stream would join the
	// same bad connection instead of getting a new one.
	//
	// An empty TLSNextProto alone is not enough: the cloned TLS config can
	// already offer "h2" through ALPN (http.DefaultTransport fills it in on its
	// first use), and a server that picks it would then be spoken to in
	// HTTP/1.1.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &Client{
		HTTP:     &http.Client{Transport: transport},
		Platform: Platform{OS: goos, Architecture: goarch},
		tokens:   map[string]tokenEntry{},
	}
}

// Resolve fetches ref's manifest, following a manifest list or OCI index to
// the entry matching the client's platform.
func (c *Client) Resolve(ctx context.Context, ref Ref) (*Image, error) {
	body, err := c.manifest(ctx, ref, ref.Tag)
	if err != nil {
		return nil, err
	}
	var index struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(body, &index) == nil && len(index.Manifests) > 0 {
		digest := ""
		for _, entry := range index.Manifests {
			if entry.Platform.OS == c.Platform.OS && entry.Platform.Architecture == c.Platform.Architecture {
				digest = entry.Digest
				break
			}
		}
		if digest == "" {
			return nil, fmt.Errorf("no %s/%s manifest in index for %s", c.Platform.OS, c.Platform.Architecture, ref.Local)
		}
		if body, err = c.manifest(ctx, ref, digest); err != nil {
			return nil, err
		}
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		// A 200 whose body is an HTML page means the request never reached a
		// registry: datacenter egress to Docker Hub has been observed serving
		// a Cloudflare interstitial here. Naming the shape beats surfacing
		// json's "invalid character '<'".
		if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '<' {
			return nil, fmt.Errorf("parse manifest for %s: registry %s returned HTML instead of a manifest (blocked, rate-limited, or intercepted by a proxy)", ref.Local, ref.Registry)
		}
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if len(manifest.Layers) == 0 {
		// Schema 1 manifests carry fsLayers instead; udocker still handles
		// those, so the caller falls back rather than guessing.
		return nil, fmt.Errorf("manifest for %s has no v2 layers", ref.Local)
	}
	if manifest.Config.Digest == "" {
		return nil, fmt.Errorf("manifest for %s has no config descriptor", ref.Local)
	}
	for _, layer := range manifest.Layers {
		if err := supportedLayerMedia(layer.MediaType); err != nil {
			return nil, err
		}
	}
	return &Image{Ref: ref, Manifest: manifest, ManifestJSON: body}, nil
}

// supportedLayerMedia rejects layer types this package cannot unpack before
// anything is downloaded, rather than producing a broken rootfs. Foreign
// (non-distributable) layers live on another host entirely; encrypted layers
// need keys no node has. Plain, gzip and zstd tars all unpack.
func supportedLayerMedia(mediaType string) error {
	switch {
	case strings.Contains(mediaType, "foreign"), strings.Contains(mediaType, "nondistributable"):
		return fmt.Errorf("foreign layer %q is not supported", mediaType)
	case strings.Contains(mediaType, "encrypted"):
		return fmt.Errorf("encrypted layer %q is not supported", mediaType)
	}
	return nil
}

func (c *Client) manifest(ctx context.Context, ref Ref, reference string) ([]byte, error) {
	url := c.baseURL(ctx, ref.Registry) + "/v2/" + ref.Remote + "/manifests/" + reference
	resp, err := c.get(ctx, ref, url, map[string]string{"Accept": manifestAccept})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest %s: %s", reference, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// BlobURL is the download endpoint for one blob.
func (c *Client) BlobURL(ref Ref, digest string) string {
	return c.knownBaseURL(ref.Registry) + "/v2/" + ref.Remote + "/blobs/" + digest
}

// get issues an authenticated GET, acquiring a bearer token on demand and
// retrying once when the registry rejects a cached token.
func (c *Client) get(ctx context.Context, ref Ref, url string, headers map[string]string) (*http.Response, error) {
	token, err := c.token(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.rawGet(ctx, url, token, headers)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	resp.Body.Close()
	if token, err = c.token(ctx, ref, true); err != nil {
		return nil, err
	}
	return c.rawGet(ctx, url, token, headers)
}

// rawGet sends one GET carrying authorization verbatim as the Authorization
// header value (empty sends none).
func (c *Client) rawGet(ctx context.Context, url, authorization string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return c.clientFor(req.URL.Host).Do(req)
}

// token returns the cached Authorization header value for ref's repository,
// obtaining it from the challenge advertised by the registry: a bearer token,
// or basic credentials for a registry that asks for them. Registries that need
// no auth cache an empty value so the /v2/ probe happens once per pull, not
// once per blob.
func (c *Client) token(ctx context.Context, ref Ref, refresh bool) (string, error) {
	key := ref.Registry + "/" + ref.Remote
	if login, ok := runtime.RegistryAuthForHost(ctx, ref.Registry); ok {
		// A token earned anonymously must not stand in for one earned by a login.
		key += "@" + login.Username
	}
	c.mu.Lock()
	entry, ok := c.tokens[key]
	c.mu.Unlock()
	if ok && !refresh && time.Now().Before(entry.expires) {
		return entry.token, nil
	}
	token, lifetime, err := c.fetchToken(ctx, ref)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[key] = tokenEntry{token: token, expires: time.Now().Add(lifetime)}
	c.mu.Unlock()
	return token, nil
}

func (c *Client) fetchToken(ctx context.Context, ref Ref) (string, time.Duration, error) {
	resp, err := c.rawGet(ctx, c.baseURL(ctx, ref.Registry)+"/v2/", "", nil)
	if err != nil {
		return "", 0, err
	}
	// The challenge is in the headers; draining only permits connection reuse.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return "", time.Hour, nil // open registry
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return "", 0, fmt.Errorf("registry probe: %s", resp.Status)
	}
	challenge := resp.Header.Get("Www-Authenticate")
	login, haveLogin := runtime.RegistryAuthForHost(ctx, ref.Registry)
	if strings.HasPrefix(strings.ToLower(challenge), "basic") && haveLogin {
		return basicAuthorization(login), time.Hour, nil
	}
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer") {
		return "", 0, fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	fields := parseAuthChallenge(challenge)
	realm := fields["realm"]
	if realm == "" {
		return "", 0, fmt.Errorf("auth challenge without realm: %q", challenge)
	}
	url := realm + "?scope=repository:" + ref.Remote + ":pull"
	if service := fields["service"]; service != "" {
		url += "&service=" + service
	}
	// A configured login goes to the token endpoint the registry itself
	// names, as docker does; anonymous pulls send none.
	authorization := ""
	if haveLogin {
		authorization = basicAuthorization(login)
	}
	resp, err = c.rawGet(ctx, url, authorization, nil)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint: %s", resp.Status)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", 0, err
	}
	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return "", 0, fmt.Errorf("token endpoint returned no token")
	}
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 5 * time.Minute
	}
	// Renew well before expiry: a blob that starts near the boundary would
	// otherwise fail mid-transfer.
	if lifetime > time.Minute {
		lifetime -= 30 * time.Second
	}
	return "Bearer " + token, lifetime, nil
}

func basicAuthorization(login proto.SourceAuth) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(login.Username+":"+login.Secret))
}

// parseAuthChallenge splits `Bearer realm="...",service="..."` into fields.
func parseAuthChallenge(challenge string) map[string]string {
	fields := map[string]string{}
	if i := strings.IndexByte(challenge, ' '); i >= 0 {
		challenge = challenge[i+1:]
	}
	for _, part := range strings.Split(challenge, ",") {
		if key, value, ok := strings.Cut(part, "="); ok {
			fields[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return fields
}

// RequestProxy honors a deployment's explicit direct/proxy choice; only an
// unspecified choice inherits the agent's proxy environment.
func RequestProxy(req *http.Request) (*url.URL, error) {
	resolved, err := httpproxy.Proxy(req, runtime.PullRoute(req.Context()), runtime.PullDirect(req.Context()))
	if err != nil {
		return nil, fmt.Errorf("invalid image pull proxy: %w", err)
	}
	return resolved, nil
}
