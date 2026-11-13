package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// loadServerConfig reads server.yaml. On the first start of an installation
// the file has no listen section yet; the address the default route leaves
// from is detected and written back, so the file always shows what the server
// binds. A caller-provided address (--addr, or the legacy GPUWORKS_ADDR)
// replaces the section for this run and skips detection.
func loadServerConfig(path string, addrGiven bool, host serverconfig.Host) (serverconfig.File, error) {
	file, err := serverconfig.Load(path)
	if err != nil {
		return serverconfig.File{}, err
	}
	if !file.Listen.Empty() || addrGiven {
		return file, nil
	}
	ip, err := serverconfig.DetectIP(host)
	if err != nil {
		return serverconfig.File{}, err
	}
	file.Listen = serverconfig.Listen{Console: []string{net.JoinHostPort(ip.String(), serverconfig.DefaultPort)}}
	if err := serverconfig.SetListen(path, file.Listen); err != nil {
		return serverconfig.File{}, fmt.Errorf("record the detected listen address in %s: %w", path, err)
	}
	return file, nil
}

// applyServerConfig copies the file into the configuration, except where a
// flag named on the command line already set the value.
func applyServerConfig(cfg *app.Config, file serverconfig.File, given map[string]bool) {
	cfg.PeerRelay = file.PeerRelay
	cfg.TLSCertFile, cfg.TLSKeyFile = file.TLSCertFile, file.TLSKeyFile
	cfg.OutboundAllowPrivateHosts = append([]string(nil), file.OutboundAllowPrivateHosts...)
	cfg.OutboundRestrictPrivateHosts = file.OutboundRestrictPrivateHosts
	if len(file.StorageProbeRoots) > 0 {
		cfg.StorageProbeRoots = append([]string(nil), file.StorageProbeRoots...)
	}
	if file.StorageDeniedPaths != nil {
		cfg.StorageDeniedPaths = append([]string{}, (*file.StorageDeniedPaths)...)
	}
	if given["addr"] {
		cfg.ListenOverridden = !file.Listen.Empty()
	} else {
		cfg.Listen = file.Listen
	}
	if !given["data"] && file.DataDir != "" {
		cfg.DataDir = file.DataDir
	}
	if !given["trusted-proxies"] && len(file.TrustedProxies) > 0 {
		cfg.TrustedProxies = strings.Join(file.TrustedProxies, ",")
	}
	if !given["metrics-retention"] && file.MetricsRetention != "" {
		if d, err := time.ParseDuration(file.MetricsRetention); err == nil && d > 0 {
			cfg.MetricsRetention = d
		}
	}
	if d, err := time.ParseDuration(file.SessionIdleTimeout); err == nil && d > 0 {
		cfg.SessionIdleTimeout = d
	}
	if d, err := time.ParseDuration(file.SessionMaxLifetime); err == nil && d > 0 {
		cfg.SessionMaxLifetime = d
	}
	if file.UdockerBundleSource != "" {
		cfg.UdockerBundleSource = file.UdockerBundleSource
	}
	cfg.SSHProxy = strings.TrimSpace(file.SSHProxy)
	cfg.UpdateChannel = file.UpdateChannel
	cfg.ManualUpdateChecks = file.AutoCheckUpdates != nil && !*file.AutoCheckUpdates
	cfg.UsageReportOff = file.UsageReport != nil && !*file.UsageReport
	// Test and CI environments switch it off without touching server.yaml.
	if strings.EqualFold(os.Getenv("GPUWORKS_USAGE_REPORT"), "off") {
		cfg.UsageReportOff = true
	}
}

// legacyEnv is the environment a unit written before server.yaml configured
// the server passes to it: GPUWORKS_ADDR, GPUWORKS_DATA_DIR and
// GPUWORKS_TRUSTED_PROXIES Environment lines. An old unit starts a new binary
// without --config, so without this bridge the server would fall back to
// ./data relative to the unit's working directory and fail to create it.
type legacyEnv struct {
	addr, dataDir, trustedProxies string
}

func readLegacyEnv() legacyEnv {
	return legacyEnv{
		addr:           os.Getenv("GPUWORKS_ADDR"),
		dataDir:        os.Getenv("GPUWORKS_DATA_DIR"),
		trustedProxies: os.Getenv("GPUWORKS_TRUSTED_PROXIES"),
	}
}

// applyLegacyEnv fills what neither the command line nor server.yaml provided
// from the legacy environment. The file and the flags stay the sources of
// truth: the environment only keeps a pre-server.yaml installation running
// until `gpuworks install` adopts it, and the unit install rewrites carries
// none of these variables. file is nil when serve runs without --config.
func applyLegacyEnv(cfg *app.Config, e legacyEnv, given map[string]bool, file *serverconfig.File) {
	e.addr, e.dataDir, e.trustedProxies = strings.TrimSpace(e.addr), strings.TrimSpace(e.dataDir), strings.TrimSpace(e.trustedProxies)
	if e.addr != "" && !given["addr"] && cfg.Listen.Empty() {
		cfg.Addr = e.addr
	}
	if e.dataDir != "" && !given["data"] && (file == nil || file.DataDir == "") {
		cfg.DataDir = e.dataDir
	}
	if e.trustedProxies != "" && !given["trusted-proxies"] && (file == nil || len(file.TrustedProxies) == 0) {
		cfg.TrustedProxies = e.trustedProxies
	}
}
