package experience

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// internalHeader carries the environment's own secret on the requests it
// makes to its console, so the guard that refuses visitors' unsupported
// operations lets its seeding through (guard.go).
const internalHeader = "X-Gpuworks-Experience"

// apiClient calls the environment's console as its admin. When the
// password changes, the next refused call logs in again with the new one.
type apiClient struct {
	base, user, secret string
	http               *http.Client

	mu              sync.Mutex
	token, password string
}

func login(ctx context.Context, base, user, password, secret string) (*apiClient, error) {
	c := &apiClient{base: base, user: user, password: password, secret: secret, http: &http.Client{Timeout: 30 * time.Second}}
	if err := c.relogin(ctx); err != nil {
		return nil, fmt.Errorf("log in to the experience console: %w", err)
	}
	return c, nil
}

func (c *apiClient) relogin(ctx context.Context) error {
	c.mu.Lock()
	password := c.password
	c.mu.Unlock()
	var out struct {
		Token string `json:"token"`
	}
	if _, err := c.send(ctx, http.MethodPost, "/api/v1/auth/login", map[string]string{"username": c.user, "password": password}, &out, ""); err != nil {
		return err
	}
	c.mu.Lock()
	c.token = out.Token
	c.mu.Unlock()
	return nil
}

func (c *apiClient) setPassword(password string) {
	c.mu.Lock()
	c.password = password
	c.mu.Unlock()
}

func (c *apiClient) get(ctx context.Context, path string, out any) error {
	return c.call(ctx, http.MethodGet, path, nil, out)
}

func (c *apiClient) post(ctx context.Context, path string, body, out any) error {
	return c.call(ctx, http.MethodPost, path, body, out)
}

// call sends with the current session, logging in again once when the
// session was revoked (a password change does that).
func (c *apiClient) call(ctx context.Context, method, path string, body, out any) error {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	status, err := c.send(ctx, method, path, body, out, token)
	if status != http.StatusUnauthorized {
		return err
	}
	if lerr := c.relogin(ctx); lerr != nil {
		return err
	}
	c.mu.Lock()
	token = c.token
	c.mu.Unlock()
	_, err = c.send(ctx, method, path, body, out, token)
	return err
}

func (c *apiClient) send(ctx context.Context, method, path string, body, out any, token string) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set(internalHeader, c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

// retry calls fn until it succeeds or attempts run out, a second apart: a
// node that just started may not be online yet.
func retry(ctx context.Context, attempts int, fn func() error) error {
	var last error
	for i := 0; i < attempts; i++ {
		if last = fn(); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return last
}
