package provision

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// HostKeyStore persists the fingerprint learned for an SSH endpoint.
// Implementations must serialize concurrent Get/Put operations.
type HostKeyStore interface {
	Get(endpoint string) (string, bool)
	Put(endpoint, fingerprint string) error
}

type memoryHostKeyStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemoryHostKeyStore() *memoryHostKeyStore {
	return &memoryHostKeyStore{m: map[string]string{}}
}

func (s *memoryHostKeyStore) Get(endpoint string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[endpoint]
	return v, ok
}

func (s *memoryHostKeyStore) Put(endpoint, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[endpoint] = fingerprint
	return nil
}

type fileHostKeyStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

// NewFileHostKeyStore opens a persistent TOFU store. A missing file is
// initialized on first Put; malformed existing data is rejected instead of
// silently weakening host-key verification.
func NewFileHostKeyStore(path string) (HostKeyStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("ssh host-key store path is empty")
	}
	s := &fileHostKeyStore{path: path, m: map[string]string{}}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) > 0 {
			if err := json.Unmarshal(b, &s.m); err != nil {
				return nil, fmt.Errorf("decode SSH host-key store: %w", err)
			}
		}
	case os.IsNotExist(err):
		// Created atomically on the first Put.
	default:
		return nil, fmt.Errorf("read SSH host-key store: %w", err)
	}
	return s, nil
}

func (s *fileHostKeyStore) Get(endpoint string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[endpoint]
	return v, ok
}

func (s *fileHostKeyStore) Put(endpoint, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[endpoint] = fingerprint
	b, err := json.Marshal(s.m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".ssh-hostkeys-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func hostKeyFingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + strings.TrimRight(base64.RawStdEncoding.EncodeToString(sum[:]), "=")
}

// verifyHostKeys turns SSH host key checking on. It is off: keys were pinned
// by address (host:port), and cloud providers hand a released public IP to
// the next instance, whose different key then blocked uninstall and
// out-of-band recovery for good (todo T77, 2026-10-20). Turning it back on
// should key the pin to the node or cloud instance, not the address.
var verifyHostKeys = false

// hostKeyCallback is the SSH host key check for endpoint: trust on first use
// against store when verifyHostKeys is on, else any key, recording nothing.
func hostKeyCallback(store HostKeyStore, endpoint string) ssh.HostKeyCallback {
	if !verifyHostKeys {
		return ssh.InsecureIgnoreHostKey()
	}
	return tofuHostKeyCallback(store, endpoint)
}

// tofuHostKeyCallback records the first key for an endpoint and rejects all
// later changes. The endpoint is canonicalized as host:port by the caller.
func tofuHostKeyCallback(store HostKeyStore, endpoint string) ssh.HostKeyCallback {
	if store == nil {
		store = newMemoryHostKeyStore()
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := hostKeyFingerprint(key)
		if want, ok := store.Get(endpoint); ok {
			if want != got {
				return &hostKeyMismatchError{endpoint: endpoint}
			}
			return nil
		}
		return store.Put(endpoint, got)
	}
}
