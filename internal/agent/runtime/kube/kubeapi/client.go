// Package kubeapi is the small part of the Kubernetes API the agent's kube
// runtime uses, spoken over plain HTTP with the pod's service account: pods,
// nodes, its own DaemonSet and the kube-system namespace. It deliberately
// is not client-go, which would multiply the agent binary for the handful
// of calls made here; the types below carry only the fields read or
// written.
package kubeapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// In-cluster service account files.
const (
	saDir       = "/var/run/secrets/kubernetes.io/serviceaccount"
	saTokenFile = saDir + "/token"
	saCAFile    = saDir + "/ca.crt"
)

// Client talks to one API server.
type Client struct {
	base  string
	token func() (string, error)
	http  *http.Client
}

// New returns a client for base (scheme://host:port) authenticating with
// the bearer token token returns, read per request so a rotated service
// account token is picked up.
func New(base string, token func() (string, error), httpc *http.Client) *Client {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: httpc}
}

// InCluster returns a client for the API server of the cluster this pod runs
// in, or an error when it does not run in one.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST is unset)")
	}
	ca, err := os.ReadFile(saCAFile)
	if err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA holds no certificate")
	}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		// The agent's own proxy settings are for registries and model hubs;
		// the API server is always reached directly.
		Proxy: nil,
	}
	read := func() (string, error) {
		b, err := os.ReadFile(saTokenFile)
		return strings.TrimSpace(string(b)), err
	}
	return New("https://"+net.JoinHostPort(host, port), read, &http.Client{Transport: tr}), nil
}

// APIError is a non-2xx answer, carrying the API server's Status.
type APIError struct {
	Code    int
	Reason  string // NotFound, AlreadyExists, Conflict, Invalid, Forbidden, Gone …
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("kubernetes API %d %s: %s", e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("kubernetes API %d %s", e.Code, e.Reason)
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return hasCode(err, http.StatusNotFound) }

// IsForbidden reports a 403: the service account may not do this.
func IsForbidden(err error) bool { return hasCode(err, http.StatusForbidden) }

// IsAlreadyExists reports a 409 AlreadyExists.
func IsAlreadyExists(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == http.StatusConflict && e.Reason == "AlreadyExists"
}

func hasCode(err error, code int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if c.token != nil {
		tok, err := c.token()
		if err != nil {
			return nil, fmt.Errorf("service account token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, decodeStatus(resp)
	}
	return resp, nil
}

func decodeStatus(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{Code: resp.StatusCode}
	var st struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &st) == nil && (st.Reason != "" || st.Message != "") {
		e.Reason, e.Message = st.Reason, st.Message
	} else {
		e.Reason, e.Message = http.StatusText(resp.StatusCode), strings.TrimSpace(string(raw))
	}
	return e
}

func (c *Client) jsonCall(ctx context.Context, method, path, contentType string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if raw, ok := in.([]byte); ok {
			body = raw
		} else if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	resp, err := c.do(ctx, method, path, contentType, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Get reads path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.jsonCall(ctx, http.MethodGet, path, "", nil, out)
}

// Create posts obj to a collection path and decodes the created object.
func (c *Client) Create(ctx context.Context, path string, obj, out any) error {
	return c.jsonCall(ctx, http.MethodPost, path, "application/json", obj, out)
}

// MergePatch applies a JSON merge patch. Lists in it replace the object's
// lists whole.
func (c *Client) MergePatch(ctx context.Context, path string, patch any, out any) error {
	return c.jsonCall(ctx, http.MethodPatch, path, "application/merge-patch+json", patch, out)
}

// Delete deletes the object at path with the grace period given (nil keeps
// the object's own). A missing object is not an error: what the caller
// wanted is already true.
func (c *Client) Delete(ctx context.Context, path string, graceSeconds *int64) error {
	var body any
	if graceSeconds != nil {
		body = map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "gracePeriodSeconds": *graceSeconds, "propagationPolicy": "Background"}
	}
	err := c.jsonCall(ctx, http.MethodDelete, path, "application/json", body, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Stream opens a streaming GET (pod logs).
func (c *Client) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
