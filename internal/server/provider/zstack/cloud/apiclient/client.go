// Package apiclient handles authenticated ZStack Cloud API communication.
package apiclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Cloud's legacy "EEE, dd MMM yyyy HH:mm:ss z" parser requires a colon
// in numeric timezone offsets, unlike time.RFC1123Z.
const accessKeyDateLayout = "Mon, 02 Jan 2006 15:04:05 -07:00"

// APIClient signs requests and manages retries, timeouts and asynchronous jobs.
type APIClient struct {
	endpoint, accessKeyID, accessKeySecret string
	http                                   *http.Client
	policy                                 clientPolicy
}

// New creates an APIClient for the configured endpoint and AccessKey.
func New(creds provider.Credentials, options ...ClientOption) (*APIClient, error) {
	policy, err := defaultClientPolicy().override(options)
	if err != nil {
		return nil, err
	}
	return newClientWithPolicy(creds, policy)
}

func newClientWithPolicy(creds provider.Credentials, policy clientPolicy) (*APIClient, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(creds.Endpoint), "/"))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("zstack: invalid Cloud endpoint")
	}
	accessKeyID := strings.TrimSpace(creds.PublicKey)
	accessKeySecret := strings.TrimSpace(creds.PrivateKey)
	if accessKeyID == "" {
		return nil, fmt.Errorf("zstack: AccessKey ID is required")
	}
	if accessKeySecret == "" {
		return nil, fmt.Errorf("zstack: AccessKey Secret is required")
	}
	return &APIClient{
		endpoint: strings.TrimSuffix(u.String(), "/v1"), accessKeyID: accessKeyID,
		accessKeySecret: accessKeySecret, policy: policy,
		http: &http.Client{
			// Per-attempt contexts allow call overrides without mutating a
			// shared HTTP client. DefaultTransport reuses connections.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *APIClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, &APIError{Kind: ErrorRequest}
	}
	uri, err := url.Parse("/v1" + path)
	if err != nil || uri.Fragment != "" {
		return nil, &APIError{Kind: ErrorRequest}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+uri.String(), body)
	if err != nil {
		return nil, &APIError{Kind: ErrorRequest}
	}
	// Match the ZStack SDK's local timezone for legacy rest.checkTimeZone=false.
	c.signRequest(req, uri.Path, time.Now().Format(accessKeyDateLayout))
	return req, nil
}

func (c *APIClient) signRequest(req *http.Request, path, date string) {
	mac := hmac.New(sha1.New, []byte(c.accessKeySecret))
	fmt.Fprintf(mac, "%s\n%s\n%s", req.Method, date, path)
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", "ZStack "+c.accessKeyID+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// OperationContext shares one total budget across multiple API calls.
// The caller must call the returned cancel function when the operation ends.
func (c *APIClient) OperationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.policy.operationTimeout)
}

// Get queries an API path and decodes its response into out.
func (c *APIClient) Get(ctx context.Context, path string, out any, options ...ClientOption) error {
	policy, err := c.policy.override(options)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, policy.operationTimeout)
	defer cancel()
	reply, err := c.request(ctx, policy, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	if reply.status != http.StatusOK {
		return &APIError{Kind: ErrorResponse, StatusCode: reply.status}
	}
	if err := json.Unmarshal(reply.body, out); err != nil {
		return &APIError{Kind: ErrorResponse, StatusCode: reply.status}
	}
	return nil
}
