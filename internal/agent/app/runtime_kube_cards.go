package app

import (
	"context"
	"log/slog"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube"
)

// What the kube runtime asks the node's prober about its cards: how the
// kubelet's device plugin names each (UUID, or PCI address), and the device
// nodes the agent's own plugin hands out for it.

// cardUUIDs maps the node's card indexes to their UUIDs, which is how the
// kube runtime matches a card to what the kubelet's device plugin calls it.
func cardUUIDs(prober gpu.Prober) func(context.Context) (map[int]string, error) {
	return func(ctx context.Context) (map[int]string, error) {
		if prober == nil {
			return map[int]string{}, nil
		}
		gpus, err := prober.Probe(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[int]string, len(gpus))
		for _, g := range gpus {
			if g.Index >= 0 && g.UUID != "" {
				out[g.Index] = g.UUID
			}
		}
		return out, nil
	}
}

// cardBDFs maps the node's card indexes to their PCI addresses: AMD's device
// plugin names its devices by them, so the kube runtime matches a card to
// the kubelet's allocations through them.
func cardBDFs(prober gpu.Prober) func(context.Context) (map[int]string, error) {
	return func(ctx context.Context) (map[int]string, error) {
		if prober == nil {
			return map[int]string{}, nil
		}
		gpus, err := prober.Probe(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[int]string, len(gpus))
		for _, g := range gpus {
			if g.Index >= 0 && g.PCIeBDF != "" {
				out[g.Index] = g.PCIeBDF
			}
		}
		return out, nil
	}
}

// cardDevices maps each card to the --device values the vendor's injection
// gives a workload on that card alone; the kube runtime's device plugin hands
// them out (runtime/kube/devices.go). A card whose injection fails (an
// unhealthy card, a device node that is gone) is left out and logged: it is
// not offered, and the node's healthy cards still are.
func cardDevices(prober gpu.Prober, log *slog.Logger) func(context.Context) (map[int][]string, error) {
	return func(ctx context.Context) (map[int][]string, error) {
		if prober == nil {
			return map[int][]string{}, nil
		}
		gpus, err := prober.Probe(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[int][]string, len(gpus))
		for _, g := range gpus {
			if g.Index < 0 {
				continue
			}
			var inj gpu.RuntimeInjection
			if aware, ok := prober.(gpu.DriverAwareInjector); ok {
				inj = aware.RuntimeInjectionForDriver([]int{g.Index}, kube.DriverName)
			} else {
				inj = prober.RuntimeInjection([]int{g.Index})
			}
			if inj.UnsupportedReason != "" {
				log.Warn("a card's devices cannot be injected; it is not offered", "card", g.Index, "reason", inj.UnsupportedReason)
				continue
			}
			for i := 0; i+1 < len(inj.DockerArgs); i++ {
				if inj.DockerArgs[i] == "--device" {
					out[g.Index] = append(out[g.Index], inj.DockerArgs[i+1])
					i++
				}
			}
		}
		return out, nil
	}
}
