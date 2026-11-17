package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

func (d *Driver) ListSSHKeys(ctx context.Context, creds provider.Credentials) ([]string, error) {
	resp, err := d.do(ctx, creds, http.MethodGet, "/account/ssh-keys")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("runpod: list SSH keys: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("runpod: decode SSH keys: %w", err)
	}
	var out struct {
		Keys    []string `json:"keys"`
		SSHKeys []string `json:"sshKeys"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("runpod: decode SSH keys: %w", err)
	}
	if len(out.Keys) == 0 {
		out.Keys = out.SSHKeys
	}
	if len(out.Keys) == 0 {
		var keys []string
		if err := json.Unmarshal(data, &keys); err == nil {
			return keys, nil
		}
	}
	return out.Keys, nil
}

// accountKeysMu serializes AuthorizePublicKey's list-then-replace: two
// concurrent appends would otherwise each drop the other's key.
var accountKeysMu sync.Mutex

// AuthorizePublicKey adds publicKey to the account's SSH keys, keeping every
// key already there. RunPod writes account keys into a pod when it starts, so
// pods created afterwards accept the platform key; externalID is unused.
func (d *Driver) AuthorizePublicKey(ctx context.Context, creds provider.Credentials, _ string, publicKey string) (provision.CheckResult, error) {
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return provision.CheckResult{}, errors.New("runpod: invalid public key")
	}
	accountKeysMu.Lock()
	defer accountKeysMu.Unlock()
	keys, err := d.ListSSHKeys(ctx, creds)
	if err != nil {
		return provision.CheckResult{}, err
	}
	for _, key := range keys {
		if f := strings.Fields(key); len(f) >= 2 && f[0] == fields[0] && f[1] == fields[1] {
			return provider.PlatformKeyAccountCheck("runpod", false, "already in the RunPod account SSH keys"), nil
		}
	}
	if err := d.ReplaceSSHKeys(ctx, creds, append(append([]string(nil), keys...), strings.TrimSpace(publicKey))); err != nil {
		return provision.CheckResult{}, err
	}
	return provider.PlatformKeyAccountCheck("runpod", true, "added to the RunPod account SSH keys (pods started from now on accept it)"), nil
}

func (d *Driver) ReplaceSSHKeys(ctx context.Context, creds provider.Credentials, keys []string) error {
	resp, err := d.doJSON(ctx, creds, http.MethodPut, "/account/ssh-keys", map[string]any{"keys": keys})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("runpod: replace SSH keys: status %d", resp.StatusCode)
	}
	return nil
}
