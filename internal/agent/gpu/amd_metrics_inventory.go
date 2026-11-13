package gpu

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Only static inventory/KFD pool facts share this short lease. Device identity,
// capacity and dynamic telemetry are read on every scrape. Runtime injection
// always verifies fresh inventory and never trusts this telemetry cache.
// Span the normal ten-second collector interval so adjacent scrapes can
// reuse static facts; dynamic readings and identity checks never use this lease.
const amdMetricInventoryTTL = 15 * time.Second

// A snapshot is immutable after construction. AMDProber.mu protects its
// publication, not the sysfs I/O used to construct or verify it.
type amdMetricInventory struct {
	at        time.Time
	cards     map[int]proto.GPUInfo
	order     []int
	memory    map[int]amdMemoryFacts
	deviceIDs map[int]string
	vram      map[int]int64
}

func (p *AMDProber) metricTime() time.Time {
	if p.clockNow != nil {
		return p.clockNow()
	}
	return time.Now()
}

func (p *AMDProber) metricInventoryFrom(gpus []proto.GPUInfo) *amdMetricInventory {
	snapshot := &amdMetricInventory{at: p.metricTime(), cards: map[int]proto.GPUInfo{}, memory: map[int]amdMemoryFacts{}, deviceIDs: map[int]string{}, vram: map[int]int64{}}
	for _, g := range gpus {
		path := fmt.Sprintf("/sys/class/drm/card%d/device", g.Index)
		snapshot.cards[g.Index] = g
		snapshot.order = append(snapshot.order, g.Index)
		snapshot.deviceIDs[g.Index] = p.text(filepath.Join(path, "device"))
		total, err := p.number(filepath.Join(path, "mem_info_vram_total"))
		if err == nil && total > 0 {
			snapshot.vram[g.Index] = total
			snapshot.memory[g.Index] = p.memoryFacts(path, g.PCIeBDF, g.ArchID, total)
		}
	}
	return snapshot
}

func (p *AMDProber) metricInventory(ctx context.Context) (*amdMetricInventory, error) {
	p.mu.RLock()
	snapshot := p.telemetry
	p.mu.RUnlock()
	now := p.metricTime()
	if snapshot != nil && !now.Before(snapshot.at) && now.Sub(snapshot.at) < amdMetricInventoryTTL {
		return snapshot, nil
	}
	gpus, err := p.inventory(ctx)
	if err != nil {
		return nil, err
	}
	snapshot = p.metricInventoryFrom(gpus)
	p.mu.Lock()
	p.telemetry = snapshot
	p.mu.Unlock()
	return snapshot, nil
}

func sameAMDMetricIdentity(expected, current proto.GPUInfo) bool {
	return expected.UUID == current.UUID && expected.PCIeBDF == current.PCIeBDF
}

func (p *AMDProber) liveMetricIdentities(snapshot *amdMetricInventory) []proto.GPUInfo {
	out := make([]proto.GPUInfo, 0, len(snapshot.order))
	for _, index := range snapshot.order {
		if current, ok := p.liveMetricCard(snapshot, index); ok {
			out = append(out, current)
		}
	}
	return out
}

func (p *AMDProber) liveMetricCard(snapshot *amdMetricInventory, index int) (proto.GPUInfo, bool) {
	current := snapshot.cards[index]
	path := fmt.Sprintf("/sys/class/drm/card%d/device", index)
	if !strings.EqualFold(p.text(filepath.Join(path, "vendor")), accelerator.AMDPCIVendorHex) || p.text(filepath.Join(path, "device")) != snapshot.deviceIDs[index] {
		return current, false
	}
	current.PCIeBDF = p.deviceBDF(path)
	current.UUID = ""
	if current.PCIeBDF != "" {
		current.UUID = "amd-" + current.PCIeBDF
		if id := normalizeAMDUniqueID(p.text(filepath.Join(path, "unique_id"))); id != "" {
			current.UUID = "amd-" + id
		}
	}
	return current, true
}

func (p *AMDProber) metricCapacityMatches(path string, snapshot *amdMetricInventory, index int) bool {
	total, err := p.number(filepath.Join(path, "mem_info_vram_total"))
	if err != nil || total <= 0 || total != snapshot.vram[index] {
		return false
	}
	facts, ok := snapshot.memory[index]
	if !ok {
		return false
	}
	if facts.source == amdMemoryUMAPool {
		gtt, err := p.number(filepath.Join(path, "mem_info_gtt_total"))
		return err == nil && gtt == facts.totalBytes
	}
	return total == facts.totalBytes
}
