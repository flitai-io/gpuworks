package community

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type registryLogin struct{ host, username, secret string }
type registryLoginKey struct{}

// authContext binds one browse to the repository's own login. An API override,
// pagination link or token challenge cannot borrow another host's credentials.
func (b *Browser) authContext(ctx context.Context, row Repo) (context.Context, string, error) {
	if b.Credentials == nil || registryOf(row) != RegistryOCI {
		return ctx, "", nil
	}
	host, _ := splitHost(row.Repo)
	username, secret, err := b.Credentials(host)
	if err != nil {
		return ctx, "", errors.New("cannot read registry credentials")
	}
	if username == "" || secret == "" {
		return ctx, "", nil
	}
	// Rotation and deletion invalidate cached private tag lists. Only this
	// in-memory fingerprint is retained; neither credentials nor tokens are stored.
	raw, _ := json.Marshal([]string{host, username, secret})
	sum := sha256.Sum256(raw)
	ctx = context.WithValue(ctx, registryLoginKey{}, registryLogin{host, username, secret})
	return ctx, hex.EncodeToString(sum[:]), nil
}

func applyRegistryLogin(req *http.Request) {
	login, ok := req.Context().Value(registryLoginKey{}).(registryLogin)
	if ok && req.URL.Scheme == "https" && req.URL.User == nil && strings.EqualFold(req.URL.Host, login.host) {
		req.SetBasicAuth(login.username, login.secret)
	}
}

func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// Go's default redirect policy also forwards Authorization to subdomains.
// Registry passwords and scoped bearer tokens must remain on the exact origin.
func scopedRedirectClient(client HTTPClient) HTTPClient {
	c, ok := client.(*http.Client)
	if !ok {
		return client
	}
	clone := *c
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 {
			previous := via[len(via)-1]
			if previous.Header.Get("Authorization") != "" && !sameOrigin(previous.URL, req.URL) {
				return errors.New("authenticated registry redirect changed origin")
			}
		}
		if c.CheckRedirect != nil {
			return c.CheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &clone
}
