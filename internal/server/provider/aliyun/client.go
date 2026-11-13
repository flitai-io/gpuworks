package aliyun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// product is one Alibaba Cloud OpenAPI product: its RPC version and the
// endpoint host for a region ("" is the product's central endpoint).
type product struct {
	name    string
	version string
	host    func(region string) string
}

var (
	ecsProduct = product{name: "ecs", version: "2014-05-26", host: func(region string) string {
		if region == "" {
			return centralECSHost
		}
		return "ecs." + region + ".aliyuncs.com"
	}}
	stsProduct = product{name: "sts", version: "2015-04-01", host: func(string) string { return "sts.aliyuncs.com" }}
	// csProduct is Container Service for Kubernetes (ACK), an ROA-style API
	// whose central endpoint answers for every region.
	csProduct = product{name: "cs", version: "2015-12-15", host: func(string) string { return "cs.aliyuncs.com" }}
)

// centralECSHost serves every region (the RegionId parameter routes the
// call). Some regional hosts are unreachable from some networks (several
// answered plain HTTP or timed out from a mainland office, 2026-10-14), so a
// regional call that fails in transport is retried here.
const centralECSHost = "ecs.aliyuncs.com"

const (
	attemptsPerHost = 3
	retryBase       = 300 * time.Millisecond
)

// apiError is the error body every Alibaba Cloud RPC API returns.
type apiError struct {
	Status    int    `json:"-"`
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	RequestID string `json:"RequestId"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("aliyun: %s: %s (HTTP %d, request %s)", e.Code, e.Message, e.Status, e.RequestID)
}

// retryable: throttling and server-side failures clear up by themselves.
func (e *apiError) retryable() bool {
	return e.Status >= 500 || strings.HasPrefix(e.Code, "Throttling") || e.Code == "ServiceUnavailable"
}

// accessKey is what a request is signed with.
type accessKey struct {
	id, secret, token string
}

type stsEntry struct {
	key     accessKey
	expires time.Time
}

type client struct {
	http *http.Client
	// baseURL maps an endpoint host to the URL requests go to (tests point
	// hosts at a fake server).
	baseURL func(host string) *url.URL
	now     func() time.Time
	nonce   func() string
	sleep   func(context.Context, time.Duration) error

	mu  sync.Mutex
	sts map[string]stsEntry
}

func newClient() *client {
	return &client{
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: func(host string) *url.URL { return &url.URL{Scheme: "https", Host: host} },
		now:     time.Now,
		nonce:   randomNonce,
		sleep:   sleepContext,
		sts:     map[string]stsEntry{},
	}
}

func randomNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// call invokes an RPC action and decodes the JSON response into out.
func (c *client) call(ctx context.Context, creds provider.Credentials, p product, region, action string, params map[string]string, out any) error {
	key, err := c.accessKey(ctx, creds)
	if err != nil {
		return err
	}
	return c.callWithKey(ctx, creds, key, p, region, action, params, out)
}

func (c *client) callWithKey(ctx context.Context, creds provider.Credentials, key accessKey, p product, region, action string, params map[string]string, out any) error {
	query := map[string]string{}
	for k, v := range params {
		query[k] = v
	}
	if region != "" {
		query["RegionId"] = region
	}
	bases := c.bases(creds, p, region)
	var lastErr, transportErr error
	for i, base := range bases {
		// A transport failure on a regional host goes to the next host rather
		// than spend the retries on a host this network cannot use.
		body, err := c.retryHost(ctx, base, key, p, action, query, 0, i < len(bases)-1)
		if err == nil {
			return decodeResponse(action, body, out)
		}
		lastErr = err
		var apiErr *apiError
		if !errors.As(err, &apiErr) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if transportErr == nil {
				transportErr = err
			}
			continue
		}
		if apiErr.Code == "InvalidOperation.NotSupportedEndpoint" && transportErr != nil {
			// The central host does not serve this region, so the regional
			// host is the only way in and its failure may have been a passing
			// reset or timeout: spend its remaining retries before giving up.
			body, err := c.retryHost(ctx, bases[0], key, p, action, query, 1, false)
			if err == nil {
				return decodeResponse(action, body, out)
			}
			if errors.As(err, &apiErr) || ctx.Err() != nil {
				return err
			}
			return fmt.Errorf("%w (the central endpoint does not serve region %s)", err, region)
		}
		if !apiErr.retryable() {
			return err
		}
	}
	return lastErr
}

// retryHost sends the call to one host, from attempt `from` up to
// attemptsPerHost with exponential backoff, retrying throttling and
// server-side failures. A transport failure ends it early when
// stopOnTransport is set; otherwise it is retried too.
func (c *client) retryHost(ctx context.Context, base endpoint, key accessKey, p product, action string, query map[string]string, from int, stopOnTransport bool) ([]byte, error) {
	var lastErr error
	for attempt := from; attempt < attemptsPerHost; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, retryBase<<(attempt-1)); err != nil {
				return nil, err
			}
		}
		body, err := c.do(ctx, base.host, base.url, key, p, action, http.MethodPost, "/", query)
		if err == nil {
			return body, nil
		}
		lastErr = err
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			if !apiErr.retryable() {
				return nil, err
			}
			continue
		}
		if ctx.Err() != nil || stopOnTransport {
			return nil, err
		}
	}
	return nil, lastErr
}

// getROA reads an ROA-style resource (ACK) from the product's central
// endpoint and decodes the JSON response into out, retrying throttling and
// server-side failures like call does.
func (c *client) getROA(ctx context.Context, creds provider.Credentials, p product, action, path string, query map[string]string, out any) error {
	key, err := c.accessKey(ctx, creds)
	if err != nil {
		return err
	}
	host := p.host("")
	var lastErr error
	for attempt := 0; attempt < attemptsPerHost; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, retryBase<<(attempt-1)); err != nil {
				return err
			}
		}
		body, err := c.do(ctx, host, c.baseURL(host), key, p, action, http.MethodGet, path, query)
		if err == nil {
			return decodeResponse(action, body, out)
		}
		lastErr = err
		var apiErr *apiError
		if errors.As(err, &apiErr) && !apiErr.retryable() {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

func decodeResponse(action string, body []byte, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("aliyun: decode %s response: %w", action, err)
	}
	return nil
}

// endpoint is a host requests are signed for and the URL they go to.
type endpoint struct {
	host string
	url  *url.URL
}

// bases are the endpoints to try, in order: an operator override (a private
// gateway) alone, otherwise the regional host then the central one.
func (c *client) bases(creds provider.Credentials, p product, region string) []endpoint {
	if override := strings.TrimSpace(creds.Endpoint); override != "" {
		if u, err := url.Parse(strings.TrimRight(override, "/")); err == nil && u.Host != "" {
			return []endpoint{{host: u.Host, url: u}}
		}
	}
	hosts := []string{p.host(region)}
	if central := p.host(""); central != hosts[0] {
		hosts = append(hosts, central)
	}
	out := make([]endpoint, 0, len(hosts))
	for _, host := range hosts {
		out = append(out, endpoint{host: host, url: c.baseURL(host)})
	}
	return out
}

func (c *client) do(ctx context.Context, host string, base *url.URL, key accessKey, p product, action, method, path string, query map[string]string) ([]byte, error) {
	headers := map[string]string{
		"x-acs-action":          action,
		"x-acs-version":         p.version,
		"x-acs-date":            c.now().UTC().Format("2006-01-02T15:04:05Z"),
		"x-acs-signature-nonce": c.nonce(),
		"x-acs-content-sha256":  emptyPayloadHash,
	}
	if key.token != "" {
		headers["x-acs-security-token"] = key.token
	}
	authorization := sign(signedRequest{method: method, host: host, path: path, query: query, headers: headers}, key.id, key.secret)
	u := *base
	u.Path = path
	u.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Authorization", authorization)
	req.Host = host
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aliyun: %s %s: %w", action, host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("aliyun: %s %s: read response: %w", action, host, err)
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &apiError{Status: resp.StatusCode}
		if json.Unmarshal(body, apiErr) != nil || apiErr.Code == "" {
			apiErr.Code = http.StatusText(resp.StatusCode)
			apiErr.Message = strings.TrimSpace(string(body))
			if len(apiErr.Message) > 200 {
				apiErr.Message = apiErr.Message[:200]
			}
		}
		return nil, apiErr
	}
	return body, nil
}

// accessKey resolves the key a request is signed with: the AccessKey itself,
// or, with a RAM role configured, STS credentials for that role, cached until
// five minutes before they expire.
func (c *client) accessKey(ctx context.Context, creds provider.Credentials) (accessKey, error) {
	base := accessKey{id: strings.TrimSpace(creds.PublicKey), secret: strings.TrimSpace(creds.PrivateKey)}
	if base.id == "" || base.secret == "" {
		return accessKey{}, errors.New("aliyun: AccessKey ID and AccessKey Secret are required")
	}
	role := roleARN(creds)
	if role == "" {
		return base, nil
	}
	cacheKey := base.id + "\x00" + role
	c.mu.Lock()
	entry, ok := c.sts[cacheKey]
	c.mu.Unlock()
	if ok && c.now().Before(entry.expires.Add(-5*time.Minute)) {
		return entry.key, nil
	}
	var out struct {
		Credentials struct {
			AccessKeyID     string `json:"AccessKeyId"`
			AccessKeySecret string `json:"AccessKeySecret"`
			SecurityToken   string `json:"SecurityToken"`
			Expiration      string `json:"Expiration"`
		} `json:"Credentials"`
	}
	params := map[string]string{"RoleArn": role, "RoleSessionName": "gpuworks", "DurationSeconds": "3600"}
	if err := c.callWithKey(ctx, creds, base, stsProduct, "", "AssumeRole", params, &out); err != nil {
		return accessKey{}, fmt.Errorf("aliyun: assume role %s: %w", role, err)
	}
	expires, err := time.Parse(time.RFC3339, out.Credentials.Expiration)
	if err != nil {
		expires = c.now().Add(55 * time.Minute)
	}
	key := accessKey{id: out.Credentials.AccessKeyID, secret: out.Credentials.AccessKeySecret, token: out.Credentials.SecurityToken}
	if key.id == "" || key.secret == "" || key.token == "" {
		return accessKey{}, fmt.Errorf("aliyun: assume role %s returned no credentials", role)
	}
	c.mu.Lock()
	c.sts[cacheKey] = stsEntry{key: key, expires: expires}
	c.mu.Unlock()
	return key, nil
}

func roleARN(creds provider.Credentials) string {
	if creds.Extra == nil {
		return ""
	}
	return strings.TrimSpace(creds.Extra["role_arn"])
}
