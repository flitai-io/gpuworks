// Package expose publishes this server on a public address through an
// "exposure provider" — Cloudflare's free quick tunnel, or a named tunnel on
// the operator's own domain.
//
// One data dir holds one entry point. The CLI commits the desired
// configuration to config.json in a short file transaction and starts the
// connector service (`gpuworks expose run`, see daemon.go) — a background
// process by default, a systemd unit with --systemd. The service's supervisor
// converges on the configuration and writes its observations to status.json.
// The server takes no part in it beyond recording the address it listens on,
// so restarting or upgrading the server leaves the address alone.
//
// See docs/how-to/expose-public-address.md.
package expose

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Provider IDs. A provider is one way to publish an entry point; it is not a
// vendor hierarchy, and the two Cloudflare providers share the cloudflared
// acquisition, the process host and the supervision.
const (
	ProviderQuick = "cloudflare-quick"
	ProviderNamed = "cloudflare-named"
)

// Desired state, written by the CLI.
const (
	DesiredEnabled  = "enabled"
	DesiredDisabled = "disabled"
)

// Observed states, written by the supervisor. Ready is the only one that means
// the address serves this server right now.
const (
	StateDisabled      = "disabled"
	StateStarting      = "starting"
	StateReady         = "ready"
	StateAwaitingRoute = "awaiting_route"
	StateDegraded      = "degraded"
	StateFailed        = "failed"
)

// ErrNoConfig means nothing has been asked for yet.
var ErrNoConfig = errors.New("no exposure configured")

// Config is the desired state: what the operator asked for. It is the only
// file the CLI writes, and it never holds a credential — the token lives in
// its own restricted file, referenced by path.
type Config struct {
	// Revision identifies this request. It changes when the operator asks for
	// something explicitly new, so a stale task can never overwrite the result
	// of a newer one.
	Revision string `json:"revision"`
	Provider string `json:"provider"`
	Desired  string `json:"desired"`
	// TokenFile is the managed credential file for providers that need one.
	TokenFile string `json:"token_file,omitempty"`
	// Hostname is set only when the operator chose among several discovered
	// routes. Automatic discovery results are never persisted here.
	Hostname string `json:"hostname,omitempty"`
	// Origin is the local address the connector forwards to: the server's
	// own listening address, as the CLI found it when the request was made.
	Origin string `json:"origin,omitempty"`
	// UpdatedAt records the last write, for operators reading the file.
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate rejects a configuration the supervisor could not act on.
func (c *Config) Validate() error {
	if c.Revision == "" {
		return errors.New("config has no revision")
	}
	if c.Provider != ProviderQuick && c.Provider != ProviderNamed {
		return fmt.Errorf("unknown provider %q", c.Provider)
	}
	switch c.Desired {
	case DesiredEnabled, DesiredDisabled:
	default:
		return fmt.Errorf("invalid desired state %q", c.Desired)
	}
	if c.Provider == ProviderNamed && c.Desired == DesiredEnabled && c.TokenFile == "" {
		return errors.New("cloudflare-named needs a token file")
	}
	return nil
}

// Status is the observation snapshot. It is not proof of liveness: the CLI
// checks the supervisor lock and the revision alongside it.
type Status struct {
	Revision string `json:"revision"`
	Provider string `json:"provider"`
	State    string `json:"state"`
	URL      string `json:"url,omitempty"`
	// LocalOrigin is the address the connector was pointed at — the server's
	// own listening address, which the CLI reads from here rather than storing
	// a second copy for the operator to maintain.
	LocalOrigin   string `json:"local_origin,omitempty"`
	Binary        string `json:"binary,omitempty"`
	BinaryVersion string `json:"binary_version,omitempty"`
	Edge          string `json:"edge,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	// Candidates are discovered hostnames the operator may choose from; the
	// automatic result is an observation, not configuration.
	Candidates []string `json:"candidates,omitempty"`
	Failures   int      `json:"failures,omitempty"`
	LastError  string   `json:"last_error,omitempty"`
	// Message explains a non-ready state in operator terms.
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store is the state directory under the data dir the CLI and the server
// agree on.
type Store struct {
	root string
}

// NewStore roots a store at the data dir's expose subdirectory.
func NewStore(dataDir string) *Store {
	return &Store{root: filepath.Join(dataDir, "expose")}
}

// Root is the state directory itself.
func (s *Store) Root() string { return s.root }

// ConfigPath is the desired state the CLI writes.
func (s *Store) ConfigPath() string { return filepath.Join(s.root, "config.json") }

// StatusPath is the observation the server writes.
func (s *Store) StatusPath() string { return filepath.Join(s.root, "status.json") }

// ServerOriginPath is where the server records the address it listens on.
func (s *Store) ServerOriginPath() string { return filepath.Join(s.root, "server-origin") }

// DaemonLogPath is the connector service's own log when it runs as a
// background process; under systemd it goes to the journal instead.
func (s *Store) DaemonLogPath() string { return filepath.Join(s.root, "logs", "service.log") }

// LogPath is the connector log the connector service rotates.
func (s *Store) LogPath() string { return filepath.Join(s.root, "logs", "connector.log") }

// CredentialsDir holds provider credentials, separate from configuration so
// neither file has to carry the other's permissions.
func (s *Store) CredentialsDir() string { return filepath.Join(s.root, "credentials") }

// BinDir is the verified cloudflared cache.
func (s *Store) BinDir() string { return filepath.Join(s.root, "bin") }

// dirMode is how the state directories are created: group-writable, and setgid
// so that files created in them keep the group of the directory.
//
// That is what makes the CLI and the server interchangeable: the CLI may run as
// root (writing the configuration and the credential) while the server runs as
// its service user, and the server has to be able to read what the CLI wrote.
// The setgid bit has to be asked for as os.ModeSetgid: a numeric 0o2775 loses
// the bit, because Go reads permissions from ModePerm only.
const dirMode = 0o775 | os.ModeSetgid

// Ensure creates the directories. They are shared between the account that runs
// the CLI and the account the server runs as.
func (s *Store) Ensure() error {
	for _, dir := range []string{s.root, filepath.Join(s.root, "logs"), s.CredentialsDir(), s.BinDir()} {
		if err := ensureStateDirectory(dir); err != nil {
			return err
		}
	}
	return nil
}

// ReadConfig loads the desired state. A missing file is ErrNoConfig.
func (s *Store) ReadConfig() (*Config, error) {
	raw, err := os.ReadFile(s.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoConfig
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", s.ConfigPath(), err)
	}
	return &c, nil
}

// WriteConfig replaces the desired state atomically.
func (s *Store) WriteConfig(c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	c.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.ConfigPath(), append(raw, '\n'), 0o664)
}

// ReadStatus loads the last observation. A missing file is not an error: it
// means the supervisor has not run yet.
func (s *Store) ReadStatus() (*Status, error) {
	raw, err := os.ReadFile(s.StatusPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		// A truncated observation is stale, not fatal: report nothing rather
		// than blocking the operator on a file the server owns.
		return nil, nil //nolint:nilerr // A truncated observation is stale and must not prevent reading the durable configuration.
	}
	return &st, nil
}

// WriteStatus records an observation atomically.
func (s *Store) WriteStatus(st *Status) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	st.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.StatusPath(), append(raw, '\n'), 0o664)
}

// tokenMode is how a managed credential is written: readable by its owner and
// by the group of the credentials directory, and by nobody else.
//
// The plan asks for the minimum that lets the two accounts read it — 0600 in a
// single-account setup, 0640 when the CLI and the server are different users.
// Which one this is cannot be known at write time (the CLI does not know who
// will run the server), so the file is written for the group and the directory's
// setgid bit decides who that group is. It never goes in config.json, which may
// stay group-writable, and never in a log or a status file.
const tokenMode = 0o640

// WriteToken stores a provider credential in its own file.
func (s *Store) WriteToken(token string) (string, error) {
	if err := s.Ensure(); err != nil {
		return "", err
	}
	path := filepath.Join(s.CredentialsDir(), "tunnel-token")
	if err := writeFileAtomic(path, []byte(token+"\n"), tokenMode); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveLocal removes what this tool owns locally: the configuration, the
// observation, the per-run isolation directory and the managed credential. The
// binary cache and the diagnostic log stay — they are reusable and useful after
// the entry point is gone. A token file the operator named with --token-file is
// not ours to delete, and nothing here touches the remote tunnel.
func (s *Store) RemoveLocal(managedTokenFile string) error {
	if err := os.RemoveAll(filepath.Join(s.root, "run")); err != nil {
		return err
	}
	paths := []string{s.ConfigPath(), s.StatusPath()}
	if managedTokenFile != "" && filepath.Dir(managedTokenFile) == s.CredentialsDir() {
		paths = append(paths, managedTokenFile)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// NewRevision mints an identifier for one explicit configuration request.
func NewRevision() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// ResolveDataDir finds the data dir, in order: an explicit flag, server.yaml
// next to the running binary, the default installation's server.yaml, and
// finally the development default. The second return value says which rule
// matched, so the CLI can tell the operator what it used instead of making
// them pass --data every time.
func ResolveDataDir(explicit string) (string, string) {
	if explicit != "" {
		return explicit, "--data"
	}
	if exe, err := os.Executable(); err == nil {
		if f, err := serverconfig.Load(serverconfig.Path(filepath.Dir(exe))); err == nil && f.DataDir != "" {
			return f.DataDir, "server.yaml next to the binary"
		}
	}
	if f, err := serverconfig.Load(serverconfig.Path(selfinstall.DefaultDir)); err == nil && f.DataDir != "" {
		return f.DataDir, "the default installation's server.yaml"
	}
	return "./data", "the development default"
}

// writeFileAtomic writes through a temporary neighbour and a rename, so a
// reader never sees a half-written file and a crash never leaves a partial
// config or status behind.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return writeStateFileAtomic(path, data, mode)
}
