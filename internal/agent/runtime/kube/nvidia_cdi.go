package kube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// NVIDIA's container toolkit in CDI mode (the GPU Operator's default) reads
// NVIDIA_VISIBLE_DEVICES as CDI device names, and an unqualified one (a card
// index or UUID) as a device of its default kind, management.nvidia.com/gpu,
// whose spec holds only "all": a pod asking for one card by index fails to
// start ("unresolvable CDI devices management.nvidia.com/gpu=0"). NVIDIA's
// device plugin, run with CDI, writes a spec with one device per card UUID
// when it starts; naming the card by that fully qualified name gives the pod
// that card alone, with no device-plugin resource of its own.

// nvidiaCDIDirs are where CDI specs live on a node (containerd's default
// cdi_spec_dirs).
var nvidiaCDIDirs = []string{"/etc/cdi", "/var/run/cdi"}

const nvidiaCDIManagementKind = "management.nvidia.com/gpu"

// nvidiaRuntimeConfigs are the container toolkit's configuration files: the
// GPU Operator's install, then a toolkit installed on the host. Their
// [nvidia-container-runtime] mode says how NVIDIA_VISIBLE_DEVICES is read;
// the management spec stays behind when an operator switches the toolkit
// back to legacy mode, so the spec alone does not decide.
var nvidiaRuntimeConfigs = []string{
	"/usr/local/nvidia/toolkit/.config/nvidia-container-runtime/config.toml",
	"/etc/nvidia-container-runtime/config.toml",
}

// nvidiaRuntimeMode is the first toolkit configuration's runtime mode under
// root ("cdi", "legacy", "auto", or "" when none says).
func nvidiaRuntimeMode(root string) string {
	for _, path := range nvidiaRuntimeConfigs {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue
		}
		section := ""
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				section = strings.Trim(line, "[] ")
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if ok && section == "nvidia-container-runtime" && strings.TrimSpace(k) == "mode" {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		return ""
	}
	return ""
}

// nvidiaCDIKinds are the spec kinds whose devices are single NVIDIA cards by
// UUID, preferred in this order: the device plugin's, then the toolkit's
// generated per-device spec.
var nvidiaCDIKinds = []string{"k8s.device-plugin.nvidia.com/gpu", "nvidia.com/gpu"}

// nvidiaCDI is what a node's CDI specs say about its NVIDIA cards.
type nvidiaCDI struct {
	// Toolkit: the container toolkit resolves NVIDIA_VISIBLE_DEVICES as CDI
	// names: its management spec is there and its configuration does not
	// say legacy mode.
	Toolkit bool
	// Names maps a card UUID to its fully qualified CDI device name.
	Names map[string]string
}

type cdiSpec struct {
	Kind    string `yaml:"kind"`
	Devices []struct {
		Name string `yaml:"name"`
	} `yaml:"devices"`
}

// readNVIDIACDI reads the CDI specs under root (the host's file system). An
// unreadable or malformed file is skipped: it is someone else's spec.
func readNVIDIACDI(root string) nvidiaCDI {
	out := nvidiaCDI{Names: map[string]string{}}
	rank := map[string]int{}
	for i, kind := range nvidiaCDIKinds {
		rank[kind] = i + 1
	}
	best := map[string]int{}
	for _, dir := range nvidiaCDIDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			ext := filepath.Ext(e.Name())
			if e.IsDir() || (ext != ".json" && ext != ".yaml" && ext != ".yml") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, dir, e.Name()))
			if err != nil {
				continue
			}
			var spec cdiSpec
			if yaml.Unmarshal(raw, &spec) != nil { // JSON is YAML
				continue
			}
			if spec.Kind == nvidiaCDIManagementKind {
				out.Toolkit = true
				continue
			}
			r := rank[spec.Kind]
			if r == 0 {
				continue
			}
			for _, d := range spec.Devices {
				if !strings.HasPrefix(d.Name, "GPU-") {
					continue // an index, "all" or a MIG device: not a card by UUID
				}
				if b, ok := best[d.Name]; !ok || r < b {
					best[d.Name] = r
					out.Names[d.Name] = spec.Kind + "=" + d.Name
				}
			}
		}
	}
	if out.Toolkit && nvidiaRuntimeMode(root) == "legacy" {
		out.Toolkit = false
	}
	return out
}

// qualify rewrites an NVIDIA_VISIBLE_DEVICES value of card indexes or UUIDs
// to CDI device names when the toolkit needs them; cards maps index to UUID.
// "all", "none" and "void" stay: the management spec defines "all".
func (c nvidiaCDI) qualify(value string, cards map[int]string) (string, error) {
	if !c.Toolkit || value == "" || value == "all" || value == "none" || value == "void" {
		return value, nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		uuid := p
		if i, err := strconv.Atoi(p); err == nil {
			uuid = cards[i]
		}
		name, ok := c.Names[uuid]
		if !ok {
			known := make([]string, 0, len(c.Names))
			for u := range c.Names {
				known = append(known, u)
			}
			sort.Strings(known)
			return "", fmt.Errorf("NVIDIA's container toolkit runs in CDI mode, and no CDI spec on the node names card %s (UUID %q; specs name %v): "+
				"run NVIDIA's device plugin with CDI (the GPU Operator's default), or set the toolkit to legacy mode (NVIDIA_CONTAINER_RUNTIME_MODE=legacy)", p, uuid, known)
		}
		out = append(out, name)
	}
	return strings.Join(out, ","), nil
}

// nvidiaVisible is the instance pods' NVIDIA_VISIBLE_DEVICES rewrite for
// this node, read when a pod is made: the GPU Operator writes its specs
// after the agent may have started. Nil when the toolkit takes plain
// indexes (legacy mode, or no toolkit spec), or without the host's root.
func (d *Driver) nvidiaVisible(ctx context.Context) func(string) (string, error) {
	if d.o.HostRoot == "" {
		return nil
	}
	cdi := readNVIDIACDI(d.o.HostRoot)
	if !cdi.Toolkit {
		return nil
	}
	return func(v string) (string, error) {
		cards := map[int]string{}
		if d.o.Cards != nil {
			if c, err := d.o.Cards(ctx); err == nil {
				cards = c
			}
		}
		return cdi.qualify(v, cards)
	}
}
