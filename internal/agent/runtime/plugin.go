package runtime

// This file contains the small, read-only runtime-plugin probe shared by the
// node status report and Doctor. A plugin is identified by the accelerator
// vendor and the selected container runtime: Docker and containerd do not
// share registration/configuration even when they use the same vendor tools.

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// ErrNoGPUSupport is a daemon's answer that it cannot give a container the
// node's accelerators. A checker wraps it only when the daemon was reached
// and said so; any other error means the question went unanswered, which
// says nothing about the node either way.
var ErrNoGPUSupport = errors.New("container runtime has no GPU support configured")

// PluginInfo is deliberately a display-only fact. Empty Name means that the
// selected runtime uses explicit device injection and has no dedicated vendor
// runtime plugin (for example Ascend+nerdctl, Hygon, and PPU).
type PluginInfo struct {
	Name      string `json:"name,omitempty"`
	Version   string `json:"version,omitempty"`
	Installed bool   `json:"installed"`
}

// PluginDetector is implemented by GPU probers whose vendor has a dedicated
// container runtime plugin. Each vendor's prober owns its detection commands;
// vendors without a plugin simply do not implement this interface.
type PluginDetector interface {
	// DetectRuntimePlugin reports the vendor plugin for the selected container
	// runtime, which may constrain what is worth reporting (a Docker-only
	// plugin under nerdctl reports the empty PluginInfo).
	DetectRuntimePlugin(ctx context.Context, driver string) PluginInfo
}

// DetectPlugin returns the installed plugin name/version for the prober's
// vendor and the selected container runtime. It intentionally does not
// require an image: image-backed behavioural smoke tests remain a separate
// Doctor check.
func DetectPlugin(ctx context.Context, prober interface{ Vendor() string }, driver string) PluginInfo {
	driver = strings.ToLower(strings.TrimSpace(driver))
	if driver == DriverUdocker || driver == "" {
		return PluginInfo{}
	}
	detector, ok := prober.(PluginDetector)
	if !ok {
		return PluginInfo{}
	}
	return detector.DetectRuntimePlugin(ctx, driver)
}

var versionRE = regexp.MustCompile(`(?i)([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[-+._][0-9A-Za-z.-]+)?)`)

// FirstVersion extracts the first version-looking token from tool output.
func FirstVersion(text string) string {
	match := versionRE.FindStringSubmatch(strings.TrimSpace(text))
	if len(match) > 1 {
		return match[1]
	}
	return ""
}
