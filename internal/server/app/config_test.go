package app

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestDefaultConfigHasTheDocumentedDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Addr != ":8800" || cfg.DataDir != "./data" || cfg.AdminUser != "admin" ||
		cfg.ProxyMaxBodyBytes != proxy.DefaultMaxBodyBytes || !cfg.RecoveryEnabled ||
		cfg.RecoveryGracePeriod != 2*time.Minute || cfg.RecoveryInitialInterval != 30*time.Second ||
		cfg.RecoveryBackoffMultiplier != 2 || cfg.RecoveryMaxInterval != 10*time.Minute ||
		cfg.RecoveryRestartWait != time.Minute || cfg.StaleCheckInterval != 30*time.Second ||
		cfg.StaleAfter != 90*time.Second || cfg.ServerDialReconnectInterval != 10*time.Second ||
		cfg.MetricsRetention != 72*time.Hour {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

// The environment is not a configuration source: every value comes from
// server.yaml or the command line.
func TestDefaultConfigIgnoresTheEnvironment(t *testing.T) {
	for key, value := range map[string]string{
		"GPUWORKS_ADDR": "10.0.0.1:1", "GPUWORKS_DATA_DIR": "/elsewhere", "GPUWORKS_DB": "/x.db",
		"GPUWORKS_PUBLIC_URL": "http://x", "GPUWORKS_ADMIN_PASSWORD": "pw", "GPUWORKS_MODEL_ENDPOINT": "http://m",
		"GPUWORKS_TRUSTED_PROXIES": "1.2.3.4", "GPUWORKS_PROXY_MAX_BODY_BYTES": "1", "GPUWORKS_RECOVERY_ENABLED": "false",
		"GPUWORKS_STALE_AFTER": "1s", "GPUWORKS_METRICS_RETENTION": "1h",
		"GPUWORKS_SESSION_IDLE_TIMEOUT": "2h", "GPUWORKS_SESSION_MAX_LIFETIME": "72h",
	} {
		t.Setenv(key, value)
	}
	if got, want := DefaultConfig(), DefaultConfig(); got.Addr != ":8800" || got.DataDir != "./data" || got.DBPath != "" ||
		got.AdminPassword != "" || got.ModelEndpoint != "" || got.TrustedProxies != "" ||
		got.ProxyMaxBodyBytes != want.ProxyMaxBodyBytes || !got.RecoveryEnabled || got.StaleAfter != 90*time.Second ||
		got.MetricsRetention != 72*time.Hour || got.SessionIdleTimeout != 24*time.Hour || got.SessionMaxLifetime != 7*24*time.Hour {
		t.Fatalf("environment leaked into the configuration: %+v", got)
	}
}

func TestManagementSessionPolicyConfiguration(t *testing.T) {
	check := func(cfg Config, idle, max time.Duration) {
		t.Helper()
		if cfg.SessionIdleTimeout != idle || cfg.SessionMaxLifetime != max {
			t.Fatalf("session policy idle=%v max=%v", cfg.SessionIdleTimeout, cfg.SessionMaxLifetime)
		}
	}
	check(DefaultConfig(), 24*time.Hour, 7*24*time.Hour)
	check(normalizeRuntimeConfig(Config{}), 24*time.Hour, 7*24*time.Hour)
	check(normalizeRuntimeConfig(Config{SessionIdleTimeout: 2 * time.Hour, SessionMaxLifetime: 72 * time.Hour}), 2*time.Hour, 72*time.Hour)
	check(normalizeRuntimeConfig(Config{SessionIdleTimeout: -time.Hour, SessionMaxLifetime: -time.Hour}), 24*time.Hour, 7*24*time.Hour)
}

func TestNormalizeRuntimeConfigDefaultsProxyMaxBodyBytes(t *testing.T) {
	if got := normalizeRuntimeConfig(Config{}).ProxyMaxBodyBytes; got != proxy.DefaultMaxBodyBytes {
		t.Fatalf("normalized ProxyMaxBodyBytes = %d, want %d", got, proxy.DefaultMaxBodyBytes)
	}
}

func TestNormalizeRuntimeConfigProtectsTickerDurations(t *testing.T) {
	cfg := normalizeRuntimeConfig(Config{})
	if cfg.RecoveryGracePeriod <= 0 || cfg.RecoveryInitialInterval <= 0 ||
		cfg.RecoveryMaxInterval <= 0 || cfg.RecoveryRestartWait <= 0 ||
		cfg.StaleCheckInterval <= 0 || cfg.StaleAfter <= 0 ||
		cfg.ServerDialReconnectInterval <= 0 || cfg.RecoveryBackoffMultiplier < 1 {
		t.Fatalf("runtime configuration was not normalized: %+v", cfg)
	}
	if cfg.MetricsRetention <= 0 {
		t.Fatalf("runtime configuration did not default MetricsRetention: %+v", cfg)
	}
}

func TestBindingsUseListenOrTheSingleAddress(t *testing.T) {
	single := Config{Addr: "127.0.0.1:0"}.bindings()
	if len(single) != 1 || single[0].Addr != "127.0.0.1:0" || len(single[0].Functions) != 3 {
		t.Fatalf("single address = %+v", single)
	}
	listen := Config{Addr: ":8800", Listen: serverconfig.Listen{Console: []string{"10.0.0.1:8800"}}}.bindings()
	if len(listen) != 2 || listen[0].Addr != "10.0.0.1:8800" || listen[1].Addr != "127.0.0.1:8800" {
		t.Fatalf("listen section = %+v", listen)
	}
}

func TestDefaultPeerRelayConfigurationUsesSharedSafePolicy(t *testing.T) {
	got, err := DefaultConfig().PeerRelay.Policy()
	if err != nil || got.RateLimitBytesPerSecond != 64<<20 || got.BurstBytes != 64<<10 || got.MaxActive != 2 || got.MaxPerNode != 1 || got.NoProgressTimeout != 2*time.Minute || got.ConfirmationTimeout != 5*time.Minute {
		t.Fatalf("default relay policy=%+v err=%v", got, err)
	}
}
