package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/selflog"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/storagerules"
)

// Config holds server runtime configuration. cmd/gpuworks fills it from
// server.yaml and the command line; nothing here reads the environment.
type Config struct {
	// SelfLog captures the server's own error-level slog records into the
	// logstore under source=server. Nil disables that drain.
	SelfLog *selflog.Collector
	// DebugLog writes the server's side of a debug session (diagnostics).
	DebugLog *debuglog.Tee
	// LogFile is the server's log file; a diagnostics collection reads it
	// and its rotated generations.
	LogFile string

	// Addr is one address serving every function. It is used when Listen is
	// empty: development, tests, and a --addr override.
	Addr string
	// Listen binds each function to its own addresses (server.yaml).
	Listen serverconfig.Listen
	// ConfigPath is server.yaml; empty means the listen addresses cannot be
	// edited from the console.
	ConfigPath string
	// ListenOverridden is set when --addr replaced the file's listen section,
	// so the console shows it read-only.
	ListenOverridden bool
	// Host is the machine's addresses; zero means the running machine.
	Host serverconfig.Host
	// SelfRestart is set when a service manager starts the process again after
	// it exits, so the console can restart it by exiting.
	SelfRestart bool
	// UpdateChannel is server.yaml's update_channel: the distribution channel
	// for update checks, official catalog updates and console upgrades; empty
	// follows the channel this binary was built for.
	UpdateChannel string
	// ManualUpdateChecks is server.yaml's auto_check_updates: false. The zero
	// value checks by itself, which is what every harness expects.
	ManualUpdateChecks bool
	// UsageReportOff is server.yaml's usage_report: false. The zero value
	// takes part in the user experience improvement program.
	UsageReportOff bool
	// PeerRelay is machine-scoped archive relay policy, resolved at startup.
	PeerRelay serverconfig.PeerRelay
	// Experience, when set, makes this server the experience environment's
	// console (experience.go).
	Experience *ExperienceConsole

	DataDir                      string // base dir for db and future tsdb/logs
	DBPath                       string // sqlite path (derived from DataDir if empty)
	VLLMImage                    string // default vllm image for deployments
	AdminUser                    string // bootstrap admin username
	AdminPassword                string // bootstrap admin password for in-process harnesses
	UdockerBundleSource          string // optional local udocker offline bundle source for SSH installs
	SSHProxy                     string // SOCKS5 proxy (socks5://host:port) for SSH to nodes
	ModelEndpoint                string // optional HF/ModelScope endpoint override for in-process harnesses
	TrustedProxies               string // comma-separated proxy IPs/CIDRs allowed to supply X-Forwarded-* headers
	TLSCertFile                  string
	TLSKeyFile                   string
	OutboundAllowPrivateHosts    []string
	OutboundRestrictPrivateHosts bool
	StorageProbeRoots            []string
	// StorageDeniedPaths are the directories no storage may use; nil is the
	// built-in list, and an empty list denies none beyond the path form rules.
	StorageDeniedPaths []string

	SessionIdleTimeout time.Duration // inactivity deadline for management sessions
	SessionMaxLifetime time.Duration // absolute login deadline

	ProxyMaxBodyBytes int64 // max /v1 request body spooled to disk before routing (413 beyond)

	RecoveryEnabled             bool
	RecoveryGracePeriod         time.Duration
	RecoveryInitialInterval     time.Duration
	RecoveryBackoffMultiplier   float64
	RecoveryMaxInterval         time.Duration
	RecoveryRestartWait         time.Duration
	StaleCheckInterval          time.Duration
	StaleAfter                  time.Duration
	ServerDialReconnectInterval time.Duration
	// MetricsRetention bounds how long raw tsdb samples (and logstore rows) are
	// kept on a first start, the data retention setting's initial value for
	// both (opsStats); the setting decides from then on.
	MetricsRetention time.Duration
}

// DefaultConfig is the configuration before server.yaml and the command line
// are applied.
func DefaultConfig() Config {
	return Config{
		Addr:                        ":" + serverconfig.DefaultPort,
		DataDir:                     "./data",
		VLLMImage:                   "vllm/vllm-openai:latest",
		AdminUser:                   selfinstall.DefaultAdminUser,
		SessionIdleTimeout:          24 * time.Hour,
		SessionMaxLifetime:          7 * 24 * time.Hour,
		ProxyMaxBodyBytes:           proxy.DefaultMaxBodyBytes,
		RecoveryEnabled:             true,
		RecoveryGracePeriod:         2 * time.Minute,
		RecoveryInitialInterval:     30 * time.Second,
		RecoveryBackoffMultiplier:   2,
		RecoveryMaxInterval:         10 * time.Minute,
		RecoveryRestartWait:         time.Minute,
		StaleCheckInterval:          30 * time.Second,
		StaleAfter:                  90 * time.Second,
		ServerDialReconnectInterval: 10 * time.Second,
		MetricsRetention:            72 * time.Hour,
		StorageProbeRoots:           []string{"/opt/gpuworks/models", "/opt/gpuworks/cache", "/mnt/gpuworks"},
		StorageDeniedPaths:          storagerules.DefaultDeniedPaths(),
	}
}

// bindings are the sockets this configuration opens.
func (c Config) bindings() []serverconfig.Binding {
	if !c.Listen.Empty() {
		return serverconfig.Bindings(c.Listen)
	}
	return []serverconfig.Binding{{Addr: c.Addr, Functions: serverconfig.Functions}}
}

// masterSecret returns the secret used to derive the credential-encryption key:
// the file in the data dir, generated once.
func masterSecret(cfg Config, policies ...securefile.Policy) (string, error) {
	path := filepath.Join(cfg.DataDir, "master.key")
	if b, err := securefile.ReadFile(path, policies...); err == nil {
		if len(b) == 0 {
			return "", fmt.Errorf("master key %s is empty; restore the original key before starting", path)
		}
		return string(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate master key: %w", err)
	}
	secret := hex.EncodeToString(raw)
	f, err := securefile.Create(path, policies...)
	if errors.Is(err, os.ErrExist) {
		b, readErr := securefile.ReadFile(path, policies...)
		if readErr != nil {
			return "", readErr
		}
		if len(b) == 0 {
			return "", fmt.Errorf("master key %s is empty", path)
		}
		return string(b), nil
	}
	if err != nil {
		return "", err
	}
	if _, err = f.WriteString(secret); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return secret, nil
}

func normalizeRuntimeConfig(cfg Config) Config {
	policy := (auth.SessionPolicy{IdleTimeout: cfg.SessionIdleTimeout, MaxLifetime: cfg.SessionMaxLifetime}).Normalized()
	cfg.SessionIdleTimeout, cfg.SessionMaxLifetime = policy.IdleTimeout, policy.MaxLifetime
	if cfg.RecoveryGracePeriod <= 0 {
		cfg.RecoveryGracePeriod = 2 * time.Minute
	}
	if cfg.RecoveryInitialInterval <= 0 {
		cfg.RecoveryInitialInterval = 30 * time.Second
	}
	if cfg.RecoveryBackoffMultiplier < 1 {
		cfg.RecoveryBackoffMultiplier = 2
	}
	if cfg.RecoveryMaxInterval <= 0 {
		cfg.RecoveryMaxInterval = 10 * time.Minute
	}
	if cfg.RecoveryRestartWait <= 0 {
		cfg.RecoveryRestartWait = time.Minute
	}
	if cfg.StaleCheckInterval <= 0 {
		cfg.StaleCheckInterval = 30 * time.Second
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 90 * time.Second
	}
	if cfg.ServerDialReconnectInterval <= 0 {
		cfg.ServerDialReconnectInterval = 10 * time.Second
	}
	if cfg.ProxyMaxBodyBytes <= 0 {
		cfg.ProxyMaxBodyBytes = proxy.DefaultMaxBodyBytes
	}
	if cfg.MetricsRetention <= 0 {
		cfg.MetricsRetention = 72 * time.Hour
	}
	return cfg
}
