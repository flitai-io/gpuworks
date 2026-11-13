package api

import (
	"math"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) dashboardCurrentMetrics(nodes []store.Node) dto.DashboardCurrentMetrics {
	var out dto.DashboardCurrentMetrics
	if a.d.MetricsLatest == nil {
		return out
	}
	online := make(map[string]bool)
	cards := make(map[string]bool)
	for i := range nodes {
		n := &nodes[i]
		if n.ConnectionState() != store.NodeStateOnline {
			continue
		}
		online[n.UUID] = true
		for _, gpu := range n.OwnedGPUs() {
			cards[metricKey(n.UUID, strconv.Itoa(gpu.Index), "")] = true
		}
	}
	if len(online) == 0 {
		return out
	}
	// Match the GPU inventory's two-minute collection freshness window. Chart
	// range and downsampling must never change a current measurement.
	now := time.Now().UTC().UnixMilli()
	since := now - int64(2*time.Minute/time.Millisecond)
	// VRAM used and total are kept per card and summed together afterwards,
	// over the cards with a fresh sample of both, so a card missing one of
	// the two cannot skew their ratio.
	var vramUsed, vramTotal map[string]MetricLatest
	for _, metric := range []struct {
		name    string
		target  **float64
		keep    *map[string]MetricLatest
		node    bool
		average bool
		maximum bool
	}{
		{name: "gpuworks_gpu_utilization", target: &out.UtilizationPct, average: true},
		{name: "gpuworks_gpu_temperature_celsius", target: &out.TemperatureC, maximum: true},
		{name: "gpuworks_gpu_power_watts", target: &out.PowerWatts},
		{name: "gpuworks_node_mem_used_bytes", target: &out.MemoryUsedBytes, node: true},
		{name: "gpuworks_gpu_memory_used_bytes", keep: &vramUsed},
		{name: "gpuworks_gpu_memory_total_bytes", keep: &vramTotal},
	} {
		points, err := a.d.MetricsLatest(metric.name, nil, since, now)
		if err != nil {
			if a.d.Log != nil {
				a.d.Log.Warn("dashboard current metric unavailable", "metric", metric.name, "error", err)
			}
			continue
		}
		// A label change can leave several series for the same resource. Count
		// only that resource's newest observation, not every historical label set.
		latest := make(map[string]MetricLatest)
		for _, p := range points {
			node := p.Labels["node"]
			if !online[node] || p.TimeMS < since || p.TimeMS > now || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				continue
			}
			key := node
			if !metric.node {
				key = metricKey(node, p.Labels["gpu"], "")
				if p.Labels["instance"] != "" || !cards[key] {
					continue
				}
			}
			if old, ok := latest[key]; !ok || p.TimeMS > old.TimeMS {
				latest[key] = p
			}
		}
		if metric.keep != nil {
			*metric.keep = latest
			continue
		}
		if len(latest) == 0 {
			continue
		}
		value := 0.0
		if metric.maximum {
			value = math.Inf(-1)
		}
		for _, p := range latest {
			if metric.maximum {
				value = math.Max(value, p.Value)
			} else {
				value += p.Value
			}
		}
		if metric.average {
			value /= float64(len(latest))
		}
		*metric.target = &value
	}
	var used, total float64
	paired := false
	for key, u := range vramUsed {
		if t, ok := vramTotal[key]; ok {
			used += u.Value
			total += t.Value
			paired = true
		}
	}
	if paired {
		out.VRAMUsedBytes, out.VRAMTotalBytes = &used, &total
	}
	return out
}
