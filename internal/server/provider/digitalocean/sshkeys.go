package digitalocean

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// platformKeyName is the name the platform key is registered under in the
// account.
const platformKeyName = "gpuworks-platform"

// accountKey is an SSH key of the account (/v2/account/keys).
type accountKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

// platformKeyCache maps a token to the fingerprint of the account key
// AuthorizePublicKey found or registered for the platform key.
type platformKeyCache struct {
	mu sync.Mutex
	m  map[string]string
}

func tokenID(creds provider.Credentials) string {
	sum := sha256.Sum256([]byte(creds.APIKey))
	return hex.EncodeToString(sum[:8])
}

func (c *platformKeyCache) get(creds provider.Credentials) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[tokenID(creds)]
}

func (c *platformKeyCache) set(creds provider.Credentials, fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]string{}
	}
	c.m[tokenID(creds)] = fingerprint
}

// accountKeys lists every SSH key of the account.
func (d *Driver) accountKeys(ctx context.Context, creds provider.Credentials) ([]accountKey, error) {
	var keys []accountKey
	err := d.listAll(ctx, creds, "/v2/account/keys", nil, func(body []byte) (bool, int, int, error) {
		var page struct {
			pageLinks
			SSHKeys []accountKey `json:"ssh_keys"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return false, 0, 0, err
		}
		keys = append(keys, page.SSHKeys...)
		return page.hasNext(), page.Meta.Total, len(page.SSHKeys), nil
	})
	return keys, err
}

// samePublicKey compares key type and key data, ignoring the comment
// (DigitalOcean stores the key as given, comment included).
func samePublicKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

// registerMu serializes AuthorizePublicKey: two concurrent registrations of
// one key would make the second fail as a duplicate.
var registerMu sync.Mutex

// AuthorizePublicKey implements provider.PublicKeyAuthorizer: it makes sure
// the platform public key is one of the account's SSH keys, reusing a key
// already there and never touching the others. externalID is unused.
//
// An account key reaches only droplets created after it was added; DigitalOcean
// has no way to put one on an existing droplet. A droplet created elsewhere
// therefore needs an SSH key or password from the operator at install.
func (d *Driver) AuthorizePublicKey(ctx context.Context, creds provider.Credentials, externalID, publicKey string) (string, error) {
	if len(strings.Fields(publicKey)) < 2 {
		return "", errors.New("digitalocean: invalid public key")
	}
	registerMu.Lock()
	defer registerMu.Unlock()
	keys, err := d.accountKeys(ctx, creds)
	if err != nil {
		return "", err
	}
	const limit = "DigitalOcean puts account keys only on droplets created afterwards"
	for _, k := range keys {
		if samePublicKey(k.PublicKey, publicKey) {
			d.platformKeys.set(creds, k.Fingerprint)
			return "already in the DigitalOcean account SSH keys (" + k.Name + "); " + limit + ", so a droplet created elsewhere needs an SSH key or password", nil
		}
	}
	raw, err := d.do(ctx, creds, http.MethodPost, "/v2/account/keys", nil, map[string]string{"name": platformKeyName, "public_key": strings.TrimSpace(publicKey)})
	if err != nil {
		return "", err
	}
	var resp struct {
		SSHKey accountKey `json:"ssh_key"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.SSHKey.Fingerprint == "" {
		return "", errors.New("digitalocean: the registered SSH key came back without a fingerprint")
	}
	d.platformKeys.set(creds, resp.SSHKey.Fingerprint)
	return "added to the DigitalOcean account SSH keys as " + platformKeyName + "; " + limit + ", so a droplet created elsewhere needs an SSH key or password", nil
}

// platformKeyFingerprint is the fingerprint of the account key holding the
// platform key: the one AuthorizePublicKey registered or found, else a key
// named platformKeyName. "" when there is none (a self-hosted server before
// its key was registered): the droplet is then created without it.
func (d *Driver) platformKeyFingerprint(ctx context.Context, creds provider.Credentials) (string, error) {
	if fp := d.platformKeys.get(creds); fp != "" {
		return fp, nil
	}
	keys, err := d.accountKeys(ctx, creds)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Name == platformKeyName {
			return k.Fingerprint, nil
		}
	}
	return "", nil
}
