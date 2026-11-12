package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/storagerules"
)

func routeHost(ip string) serverconfig.Host {
	return serverconfig.Host{RouteIP: func() (net.IP, error) { return net.ParseIP(ip), nil }}
}

func TestFirstStartDetectsAndRecordsTheListenAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	if err := os.WriteFile(path, []byte("# written by install\ndata_dir: /srv/data\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	file, err := loadServerConfig(path, false, routeHost("192.0.2.156"))
	if err != nil || len(file.Listen.Console) != 1 || file.Listen.Console[0] != "192.0.2.156:8800" || file.DataDir != "/srv/data" {
		t.Fatalf("first start = %+v, %v", file, err)
	}
	reloaded, err := serverconfig.Load(path)
	if err != nil || reloaded.Listen.Console[0] != "192.0.2.156:8800" {
		t.Fatalf("detected address not written back: %+v, %v", reloaded, err)
	}
	// Later starts keep what the file says, even if the route changed.
	file, err = loadServerConfig(path, false, routeHost("10.9.9.9"))
	if err != nil || file.Listen.Console[0] != "192.0.2.156:8800" {
		t.Fatalf("second start = %+v, %v", file, err)
	}
}

func TestFirstStartFailsWithoutAnAddressToBind(t *testing.T) {
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	if err := os.WriteFile(path, nil, 0o664); err != nil {
		t.Fatal(err)
	}
	none := serverconfig.Host{
		RouteIP:    func() (net.IP, error) { return nil, errors.New("network is unreachable") },
		Interfaces: func() ([]serverconfig.Interface, error) { return nil, nil },
	}
	if _, err := loadServerConfig(path, false, none); !errors.Is(err, serverconfig.ErrNoAddress) {
		t.Fatalf("err = %v", err)
	}
	// --addr needs no detection.
	if _, err := loadServerConfig(path, true, none); err != nil {
		t.Fatalf("--addr run: %v", err)
	}
	if _, err := loadServerConfig(filepath.Join(t.TempDir(), "missing.yaml"), false, none); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file err = %v", err)
	}
}

func TestFlagsOverrideTheFile(t *testing.T) {
	file := serverconfig.File{
		Listen:  serverconfig.Listen{Console: []string{"10.0.0.1:8800"}},
		DataDir: "/file/data", TrustedProxies: []string{"10.0.0.0/8", "::1/128"},
		MetricsRetention: "168h", UdockerBundleSource: "/bundle",
		SessionIdleTimeout: "2h", SessionMaxLifetime: "72h",
	}
	cfg := app.DefaultConfig()
	applyServerConfig(&cfg, file, map[string]bool{})
	if len(cfg.Listen.Console) != 1 || cfg.ListenOverridden || cfg.DataDir != "/file/data" ||
		cfg.TrustedProxies != "10.0.0.0/8,::1/128" || cfg.MetricsRetention != 168*time.Hour ||
		cfg.UdockerBundleSource != "/bundle" || cfg.SessionIdleTimeout != 2*time.Hour || cfg.SessionMaxLifetime != 72*time.Hour {
		t.Fatalf("file values = %+v", cfg)
	}

	cfg = app.DefaultConfig()
	cfg.Addr, cfg.DataDir = "127.0.0.1:9000", "/flag/data"
	applyServerConfig(&cfg, file, map[string]bool{"addr": true, "data": true})
	if !cfg.Listen.Empty() || !cfg.ListenOverridden || cfg.Addr != "127.0.0.1:9000" || cfg.DataDir != "/flag/data" {
		t.Fatalf("flag values = %+v", cfg)
	}
}

func TestServerSecurityPolicyComesOnlyFromTheHostConfiguration(t *testing.T) {
	cfg := app.DefaultConfig()
	applyServerConfig(&cfg, serverconfig.File{TLSCertFile: "cert.pem", TLSKeyFile: "key.pem", OutboundAllowPrivateHosts: []string{"models.corp"}, StorageProbeRoots: []string{"/srv/models"}}, nil)
	if cfg.TLSCertFile != "cert.pem" || cfg.TLSKeyFile != "key.pem" || len(cfg.OutboundAllowPrivateHosts) != 1 || cfg.OutboundAllowPrivateHosts[0] != "models.corp" || len(cfg.StorageProbeRoots) != 1 || cfg.StorageProbeRoots[0] != "/srv/models" {
		t.Fatalf("security policy not applied: %+v", cfg)
	}
	cfg = app.DefaultConfig()
	applyServerConfig(&cfg, serverconfig.File{}, nil)
	if len(cfg.StorageProbeRoots) != 3 || len(cfg.OutboundAllowPrivateHosts) != 0 || cfg.TLSCertFile != "" {
		t.Fatalf("empty file discarded security defaults: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.StorageDeniedPaths, storagerules.DefaultDeniedPaths()) {
		t.Fatalf("absent storage_denied_paths dropped the built-in list: %v", cfg.StorageDeniedPaths)
	}
	for _, denied := range [][]string{{}, {"/opt/gpuworks"}} {
		cfg = app.DefaultConfig()
		applyServerConfig(&cfg, serverconfig.File{StorageDeniedPaths: &denied}, nil)
		if cfg.StorageDeniedPaths == nil || !reflect.DeepEqual(cfg.StorageDeniedPaths, denied) {
			t.Fatalf("storage_denied_paths %v applied as %v", denied, cfg.StorageDeniedPaths)
		}
	}
}

// A unit written before server.yaml passes everything through Environment
// lines and no --config, which is how an un-adopted installation upgrades.
func TestLegacyEnvBridgesUnitsWrittenBeforeServerYAML(t *testing.T) {
	t.Setenv("GPUWORKS_ADDR", ":8800")
	t.Setenv("GPUWORKS_DATA_DIR", "/opt/gpuworks/server/data")
	t.Setenv("GPUWORKS_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")
	cfg := app.DefaultConfig()
	applyLegacyEnv(&cfg, readLegacyEnv(), map[string]bool{}, nil)
	if cfg.Addr != ":8800" || cfg.DataDir != "/opt/gpuworks/server/data" ||
		cfg.TrustedProxies != "127.0.0.1/32,::1/128" || cfg.ListenOverridden {
		t.Fatalf("env bridge = %+v", cfg)
	}
}

// The command line and server.yaml are the sources of truth; the environment
// only fills what they leave out, so an adopted installation ignores the
// variables its old unit might still export.
func TestLegacyEnvYieldsToFlagsAndServerYAML(t *testing.T) {
	e := legacyEnv{addr: "192.168.1.1:8800", dataDir: "/env/data", trustedProxies: "127.0.0.1/32"}

	file := serverconfig.File{
		Listen:         serverconfig.Listen{Console: []string{"10.0.0.1:8800"}},
		DataDir:        "/file/data",
		TrustedProxies: []string{"10.0.0.0/8"},
	}
	cfg := app.DefaultConfig()
	applyServerConfig(&cfg, file, map[string]bool{})
	applyLegacyEnv(&cfg, e, map[string]bool{}, &file)
	if cfg.Listen.Console[0] != "10.0.0.1:8800" || cfg.Addr != ":8800" || cfg.DataDir != "/file/data" ||
		cfg.TrustedProxies != "10.0.0.0/8" {
		t.Fatalf("file must win over the environment: %+v", cfg)
	}

	// A file that only places the data still leaves room for the other two.
	partial := serverconfig.File{DataDir: "/file/data"}
	cfg = app.DefaultConfig()
	applyServerConfig(&cfg, partial, map[string]bool{})
	applyLegacyEnv(&cfg, e, map[string]bool{}, &partial)
	if cfg.Addr != "192.168.1.1:8800" || cfg.DataDir != "/file/data" || cfg.TrustedProxies != "127.0.0.1/32" {
		t.Fatalf("partial file = %+v", cfg)
	}

	// Flags beat the environment even without a file, as they always did.
	cfg = app.DefaultConfig()
	cfg.Addr, cfg.DataDir, cfg.TrustedProxies = "127.0.0.1:9000", "/flag/data", "10.1.0.0/16"
	applyLegacyEnv(&cfg, e, map[string]bool{"addr": true, "data": true, "trusted-proxies": true}, nil)
	if cfg.Addr != "127.0.0.1:9000" || cfg.DataDir != "/flag/data" || cfg.TrustedProxies != "10.1.0.0/16" {
		t.Fatalf("flags must win over the environment: %+v", cfg)
	}

	// A file listen makes the environment's address moot even unflagged.
	withListen := serverconfig.File{Listen: serverconfig.Listen{Console: []string{"10.0.0.2:8800"}}}
	cfg = app.DefaultConfig()
	applyServerConfig(&cfg, withListen, map[string]bool{})
	applyLegacyEnv(&cfg, e, map[string]bool{}, &withListen)
	if cfg.Listen.Console[0] != "10.0.0.2:8800" {
		t.Fatalf("listen from file = %+v", cfg)
	}

	// Blank variables bridge nothing.
	cfg = app.DefaultConfig()
	applyLegacyEnv(&cfg, legacyEnv{addr: "  ", dataDir: "", trustedProxies: ""}, map[string]bool{}, nil)
	if cfg.Addr != ":"+serverconfig.DefaultPort || cfg.DataDir != "./data" || cfg.TrustedProxies != "" {
		t.Fatalf("blank environment changed the config: %+v", cfg)
	}
}

// An environment address is a caller-provided address: nothing is detected
// and written back into a listen-less server.yaml behind the unit's back.
func TestLegacyEnvAddrSkipsListenDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	if err := os.WriteFile(path, []byte("data_dir: /srv/data\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	e := legacyEnv{addr: ":8800"}
	if e.addr == "" {
		t.Fatal("env addr missing")
	}
	file, err := loadServerConfig(path, e.addr != "", routeHost("192.0.2.156"))
	if err != nil || !file.Listen.Empty() || file.DataDir != "/srv/data" {
		t.Fatalf("listen-less file with an env address = %+v, %v", file, err)
	}
	reloaded, err := serverconfig.Load(path)
	if err != nil || !reloaded.Listen.Empty() {
		t.Fatalf("detection wrote back despite the env address: %+v, %v", reloaded, err)
	}
}

func TestSessionDurationsInServerYAMLKeepDefaultsWhenInvalid(t *testing.T) {
	for _, value := range []string{"", "invalid", "0", "-1h"} {
		t.Run(value, func(t *testing.T) {
			cfg := app.DefaultConfig()
			applyServerConfig(&cfg, serverconfig.File{SessionIdleTimeout: value, SessionMaxLifetime: value}, nil)
			if cfg.SessionIdleTimeout != 24*time.Hour || cfg.SessionMaxLifetime != 7*24*time.Hour {
				t.Fatalf("invalid duration %q changed defaults: idle=%v max=%v", value, cfg.SessionIdleTimeout, cfg.SessionMaxLifetime)
			}
		})
	}
}

func TestServerYAMLAutoCheckUpdatesReachesTheConfiguration(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		name       string
		file       *bool
		wantManual bool
	}{{"absent means automatic", nil, false}, {"true", &on, false}, {"false", &off, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg app.Config
			applyServerConfig(&cfg, serverconfig.File{AutoCheckUpdates: tc.file}, map[string]bool{})
			if cfg.ManualUpdateChecks != tc.wantManual {
				t.Fatalf("ManualUpdateChecks = %v, want %v", cfg.ManualUpdateChecks, tc.wantManual)
			}
		})
	}
}

func TestPeerRelayHostConfigurationReachesStartup(t *testing.T) {
	raw := serverconfig.PeerRelay{RateLimitKiBPerSecond: 8192, BurstKiB: 32, NoProgressTimeout: "3m", ConfirmationTimeout: "7m"}
	cfg := app.DefaultConfig()
	applyServerConfig(&cfg, serverconfig.File{PeerRelay: raw}, nil)
	if !reflect.DeepEqual(cfg.PeerRelay, raw) {
		t.Fatalf("relay host config lost: %+v", cfg.PeerRelay)
	}
	applyServerConfig(&cfg, serverconfig.File{}, nil)
	if !reflect.DeepEqual(cfg.PeerRelay, serverconfig.PeerRelay{}) {
		t.Fatalf("omitted file policy retained previous settings: %+v", cfg.PeerRelay)
	}
	got, err := cfg.PeerRelay.Policy()
	if err != nil || got.RateLimitBytesPerSecond != 64<<20 {
		t.Fatalf("omitted host config no safe default: %+v %v", got, err)
	}
}
