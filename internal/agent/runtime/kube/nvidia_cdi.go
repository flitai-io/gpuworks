package kube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/proto"
)

// NVIDIA's container toolkit in CDI mode (the GPU Operator's default) reads
// NVIDIA_VISIBLE_DEVICES as CDI device names, and an unqualified one (a card
// index or UUID) as a device of its default kind, management.nvidia.com/gpu,
// whose spec holds only "all": a pod asking for one card by index fails to
// start ("unresolvable CDI devices management.nvidia.com/gpu=0"). NVIDIA's
// device plugin, run with CDI, writes a spec with one device per card UUID
// when it starts; naming the card by that fully qualified name gives the pod
// that card alone, with no device-plugin resource of its own.
//
// When no spec names the card (HAMi, or another device plugin, replaced
// NVIDIA's, so nobody wrote the per-card names) the pod is given every card
// through the management spec's "all" and CUDA_VISIBLE_DEVICES limits it to
// the card it was assigned: every /dev/nvidiaN is visible in the container,
// only CUDA is confined. The node reports that as soft isolation
// (Driver.NVIDIAIsolation) so the operator can pick strict isolation back.

// nvidiaCDIDirs are where CDI specs live on a node: containerd's default
// cdi_spec_dirs, and /run/cdi for /var/run/cdi. On most distributions
// /var/run is an absolute symlink to /run, which the kernel resolves
// against the agent's own root, not the host's, when the path is read
// through the host's root: only /run/cdi reaches the host's specs then.
var nvidiaCDIDirs = []string{"/etc/cdi", "/run/cdi", "/var/run/cdi"}

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
// root ("cdi", "legacy", "auto", or "" when none says) and whose toolkit that
// configuration is (proto.NVIDIAToolkitGPUOperator or
// proto.NVIDIAToolkitHost; "" when there is no configuration at all).
func nvidiaRuntimeMode(root string) (mode, toolkit string) {
	for i, path := range nvidiaRuntimeConfigs {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue
		}
		toolkit = proto.NVIDIAToolkitGPUOperator
		if i > 0 {
			toolkit = proto.NVIDIAToolkitHost
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
				return strings.Trim(strings.TrimSpace(v), `"'`), toolkit
			}
		}
		return "", toolkit
	}
	return "", ""
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
	// ToolkitKind is whose toolkit configuration was read
	// (proto.NVIDIAToolkit*), empty when there was none.
	ToolkitKind string
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
	mode, kind := nvidiaRuntimeMode(root)
	out.ToolkitKind = kind
	if out.Toolkit && mode == "legacy" {
		out.Toolkit = false
	}
	return out
}

// nvidiaVisibility is what an instance pod is given to see of its cards.
type nvidiaVisibility struct {
	// Devices is the NVIDIA_VISIBLE_DEVICES value.
	Devices string
	// CUDAVisible, when set, is the CUDA_VISIBLE_DEVICES value: Devices is
	// then every card, and CUDA is what keeps the pod to its own (soft
	// isolation).
	CUDAVisible string
}

// qualify rewrites an NVIDIA_VISIBLE_DEVICES value of card indexes or UUIDs
// to CDI device names when the toolkit needs them; cards maps index to UUID.
// "all", "none" and "void" stay: the management spec defines "all". When any
// card is named by no spec, the pod is given every card through the
// management spec and CUDA_VISIBLE_DEVICES is the host index of each card
// asked for (with every card visible the container's CUDA order is the
// host's).
func (c nvidiaCDI) qualify(value string, cards map[int]string) (nvidiaVisibility, error) {
	if !c.Toolkit || value == "" || value == "all" || value == "none" || value == "void" {
		return nvidiaVisibility{Devices: value}, nil
	}
	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	indexes := make([]string, 0, len(parts))
	named := true
	for _, p := range parts {
		p = strings.TrimSpace(p)
		uuid, index := p, ""
		if i, err := strconv.Atoi(p); err == nil {
			uuid, index = cards[i], p
		} else {
			for i, u := range cards {
				if u == p {
					index = strconv.Itoa(i)
				}
			}
		}
		if index == "" {
			return nvidiaVisibility{}, fmt.Errorf("NVIDIA's container toolkit runs in CDI mode and card %q is not one of the node's cards, so it can be neither named nor confined by CUDA_VISIBLE_DEVICES", p)
		}
		name, ok := c.Names[uuid]
		named = named && ok
		names = append(names, name)
		indexes = append(indexes, index)
	}
	if named {
		return nvidiaVisibility{Devices: strings.Join(names, ",")}, nil
	}
	return nvidiaVisibility{Devices: nvidiaCDIManagementKind + "=all", CUDAVisible: strings.Join(indexes, ",")}, nil
}

// nvidiaVisible is the instance pods' NVIDIA_VISIBLE_DEVICES rewrite for
// this node, read when a pod is made: the GPU Operator writes its specs
// after the agent may have started. Nil when the toolkit takes plain
// indexes (legacy mode, or no toolkit spec), or without the host's root.
func (d *Driver) nvidiaVisible(ctx context.Context) func(string) (nvidiaVisibility, error) {
	if d.o.HostRoot == "" {
		return nil
	}
	cdi := readNVIDIACDI(d.o.HostRoot)
	if !cdi.Toolkit {
		return nil
	}
	return func(v string) (nvidiaVisibility, error) {
		return cdi.qualify(v, d.nvidiaCards(ctx))
	}
}

// nvidiaCards is the node's cards by index and UUID; empty when the probe
// cannot say.
func (d *Driver) nvidiaCards(ctx context.Context) map[int]string {
	if d.o.Cards == nil {
		return map[int]string{}
	}
	cards, err := d.o.Cards(ctx)
	if err != nil || cards == nil {
		return map[int]string{}
	}
	return cards
}

// NVIDIAIsolation is what the node reports about how strictly an instance
// pod is kept to its own NVIDIA card: nil while it is strict (the toolkit
// takes plain indexes, or a CDI spec names every card) or the node has no
// NVIDIA card; soft when the toolkit resolves CDI names, no spec names some
// card, and pods therefore see every card and are held to theirs by CUDA only.
func (d *Driver) NVIDIAIsolation(ctx context.Context) *proto.NVIDIAIsolation {
	if d.o.HostRoot == "" {
		return nil
	}
	cdi := readNVIDIACDI(d.o.HostRoot)
	if !cdi.Toolkit {
		return nil
	}
	unnamed := 0
	for _, uuid := range d.nvidiaCards(ctx) {
		if !strings.HasPrefix(uuid, "GPU-") {
			continue // not an NVIDIA card by UUID (another vendor's, or a MIG device)
		}
		if _, ok := cdi.Names[uuid]; !ok {
			unnamed++
		}
	}
	if unnamed == 0 {
		return nil
	}
	return &proto.NVIDIAIsolation{Level: proto.NVIDIAIsolationSoft, Reason: proto.NVIDIAIsolationCDINamesMissing, Toolkit: cdi.ToolkitKind, Cards: unnamed}
}
