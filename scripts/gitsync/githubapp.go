package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// githubApp mints installation tokens for a GitHub App: a token lasts an
// hour, so it is reused until five minutes before it expires. The App's own
// rate limit is separate from every personal account's.
type githubApp struct {
	api        string // https://api.github.com
	appID      string
	key        *rsa.PrivateKey
	repo       string // owner/name, to find the installation
	client     *http.Client
	now        func() time.Time
	mu         sync.Mutex
	token      string
	expires    time.Time
	installURL string
}

func parseAppKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app key: no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not an RSA key")
	}
	return key, nil
}

// Creds is a remote.Creds: git authenticates as x-access-token.
func (g *githubApp) Creds(ctx context.Context) (string, string, error) {
	token, err := g.installationToken(ctx)
	return "x-access-token", token, err
}

func (g *githubApp) installationToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && g.now().Before(g.expires.Add(-5*time.Minute)) {
		return g.token, nil
	}
	jwt, err := g.jwt()
	if err != nil {
		return "", err
	}
	if g.installURL == "" {
		var installation struct {
			AccessTokensURL string `json:"access_tokens_url"`
		}
		if err := g.call(ctx, http.MethodGet, g.api+"/repos/"+g.repo+"/installation", jwt, http.StatusOK, &installation); err != nil {
			return "", fmt.Errorf("find the app's installation on %s: %w", g.repo, err)
		}
		if installation.AccessTokensURL == "" {
			return "", fmt.Errorf("the app's installation on %s names no access_tokens_url", g.repo)
		}
		g.installURL = installation.AccessTokensURL
	}
	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.call(ctx, http.MethodPost, g.installURL, jwt, http.StatusCreated, &minted); err != nil {
		return "", fmt.Errorf("mint an installation token: %w", err)
	}
	if minted.Token == "" {
		return "", errors.New("mint an installation token: empty token")
	}
	g.token, g.expires = minted.Token, minted.ExpiresAt
	return g.token, nil
}

func (g *githubApp) call(ctx context.Context, method, url, jwt string, want int, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != want {
		return fmt.Errorf("%s %s: %d %s", method, url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// jwt signs the App's own credential: RS256, issued a minute in the past to
// allow for clock drift, valid for nine of the ten minutes GitHub accepts.
func (g *githubApp) jwt() (string, error) {
	now := g.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": g.appID})
	if err != nil {
		return "", err
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
