package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// httpDoer is the part of *http.Client the driver uses.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func newHTTPClient() httpDoer { return &http.Client{Timeout: 60 * time.Second} }

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

const (
	// requestAttempts is how many times a request is sent while the API
	// answers 429 or 5xx or the connection fails.
	requestAttempts = 4
	// maxRetryWait caps a Retry-After the API asks for: a longer wait is
	// reported to the caller instead.
	maxRetryWait = 30 * time.Second
	// maxBody bounds what is read of a response (a kubeconfig is some KiB,
	// a page of 200 droplets some MiB).
	maxBody = 32 << 20
)

// APIError is an error answer of the DigitalOcean API:
// {"id":"unauthorized","message":"Unable to authenticate you"}.
type APIError struct {
	Method    string
	Path      string
	Status    int
	ID        string
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("digitalocean: %s %s: status %d", e.Method, e.Path, e.Status)
	if e.ID != "" {
		msg += " (" + e.ID + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RequestID != "" {
		msg += " [request " + e.RequestID + "]"
	}
	return msg
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func newAPIError(method, path string, resp *http.Response, body []byte) *APIError {
	e := &APIError{Method: method, Path: path, Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
	var parsed struct {
		ID        string `json:"id"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		e.ID, e.Message = parsed.ID, parsed.Message
		if e.RequestID == "" {
			e.RequestID = parsed.RequestID
		}
	}
	if e.ID == "" && e.Message == "" {
		// Not the API's own error body (a proxy page, a bare status).
		e.Message = strings.TrimSpace(string(body[:min(len(body), 200)]))
	}
	return e
}

func (d *Driver) base(creds provider.Credentials) string {
	return creds.EndpointOr(d.baseURL)
}

// get is a GET returning the response body.
func (d *Driver) get(ctx context.Context, creds provider.Credentials, path string, query url.Values) ([]byte, error) {
	return d.do(ctx, creds, http.MethodGet, path, query, nil)
}

// getJSON is a GET decoded into out.
func (d *Driver) getJSON(ctx context.Context, creds provider.Credentials, path string, query url.Values, out any) error {
	body, err := d.get(ctx, creds, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("digitalocean: decode %s: %w", path, err)
	}
	return nil
}

// do sends one request, repeating it while the API rate-limits (429). A GET
// or DELETE is also repeated after a 5xx or a failed connection; a POST is
// not, as it might have created something (a billed droplet) already.
func (d *Driver) do(ctx context.Context, creds provider.Credentials, method, path string, query url.Values, payload any) ([]byte, error) {
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, errors.New("digitalocean: API token is required")
	}
	var encoded []byte
	if payload != nil {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			return nil, fmt.Errorf("digitalocean: encode request: %w", err)
		}
	}
	target := d.base(creds) + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	safeToRepeat := method == http.MethodGet || method == http.MethodDelete
	var lastErr error
	for attempt := 1; attempt <= requestAttempts; attempt++ {
		var reader io.Reader
		if encoded != nil {
			reader = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+creds.APIKey)
		req.Header.Set("Accept", "application/json")
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		wait := time.Duration(attempt) * time.Second
		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("digitalocean: %s %s: %w", method, path, err)
			if !safeToRepeat || ctx.Err() != nil {
				return nil, lastErr
			}
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
			resp.Body.Close()
			switch {
			case readErr != nil:
				lastErr = fmt.Errorf("digitalocean: %s %s: read response: %w", method, path, readErr)
				if !safeToRepeat {
					return nil, lastErr
				}
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return body, nil
			default:
				apiErr := newAPIError(method, path, resp, body)
				repeat := resp.StatusCode == http.StatusTooManyRequests || (safeToRepeat && resp.StatusCode >= 500)
				if !repeat {
					return nil, apiErr
				}
				lastErr = apiErr
				if after, ok := retryAfter(resp); ok {
					if after > maxRetryWait {
						return nil, apiErr
					}
					wait = after
				}
			}
		}
		if attempt == requestAttempts {
			break
		}
		if err := d.sleep(ctx, wait); err != nil {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// retryAfter is the wait a 429 asks for in Retry-After (whole seconds).
func retryAfter(resp *http.Response) (time.Duration, bool) {
	seconds, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// pageSize is the largest page the API serves.
const pageSize = 200

// maxPages bounds a listing, so a server that keeps naming a next page
// cannot loop the driver.
const maxPages = 1000

// listAll reads every page of a collection. decode appends one page's items
// and says whether the response names a next page and how many items the
// collection has in all (meta.total). The result is complete or an error:
// callers (the instance sync) remove what a listing does not show.
func (d *Driver) listAll(ctx context.Context, creds provider.Credentials, path string, query url.Values, decode func(body []byte) (hasNext bool, total int, count int, err error)) error {
	seen := 0
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("per_page", strconv.Itoa(pageSize))
		q.Set("page", strconv.Itoa(page))
		body, err := d.get(ctx, creds, path, q)
		if err != nil {
			return err
		}
		hasNext, total, count, err := decode(body)
		if err != nil {
			return fmt.Errorf("digitalocean: decode %s page %d: %w", path, page, err)
		}
		seen += count
		if !hasNext {
			if total > 0 && seen < total {
				return fmt.Errorf("digitalocean: %s listed %d of %d items", path, seen, total)
			}
			return nil
		}
		if count == 0 {
			return fmt.Errorf("digitalocean: %s page %d is empty but names a next page", path, page)
		}
	}
	return fmt.Errorf("digitalocean: %s has more than %d pages", path, maxPages)
}

// pageLinks is the paging part of a list response.
type pageLinks struct {
	Links struct {
		Pages struct {
			Next string `json:"next"`
		} `json:"pages"`
	} `json:"links"`
	Meta struct {
		Total int `json:"total"`
	} `json:"meta"`
}

func (p pageLinks) hasNext() bool { return strings.TrimSpace(p.Links.Pages.Next) != "" }
