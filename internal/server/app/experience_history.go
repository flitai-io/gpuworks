package app

import (
	"context"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// The experience environment opens on a platform that has been in use for a
// month: these let it write that history in the shapes the live paths would
// have written it, and read the cards and series it writes it for.

// History is past activity, in the live paths' own shapes.
type History struct {
	// Requests are past requests, each standing for Weights[i] like it.
	Requests []uxrecord.Request
	Weights  []int64
	// GPUs are cards' hourly samples, named by GPUModels; GPUTime and
	// Capacity the hourly card time of deployments and of the cluster.
	GPUs      map[rollup.GPUKey]rollup.GPU
	GPUModels map[rollup.GPUKey]string
	GPUTime   map[rollup.GPUTimeKey]rollup.GPUTime
	Capacity  map[rollup.CapacityKey]rollup.Capacity
}

// SeedHistory adds h to the usage and statistics stores and rolls up the
// days it falls on. Each call adds: seeding the same history twice counts it
// twice.
func (a *App) SeedHistory(ctx context.Context, h History) error {
	if a.ops == nil || a.ops.stats == nil || len(h.Requests) != len(h.Weights) {
		return errors.New("statistics are not available")
	}
	loc := a.ops.location()
	today := rollup.DayOf(time.Now(), loc)
	days := map[rollup.Day]bool{}
	mark := func(ms int64) {
		if day := rollup.DayOf(time.UnixMilli(ms), loc); day < today {
			days[day] = true
		}
	}
	acc := rollup.NewRequests()
	for i, rec := range h.Requests {
		acc.ObserveWeighted(rec, h.Weights[i])
		mark(rec.ReceivedMS + int64(rec.EndMS))
	}
	for k := range h.GPUs {
		mark(k.HourMS)
	}
	for k := range h.GPUTime {
		mark(k.HourMS)
	}
	if err := a.store.AddUsage(ctx, acc.TakeUsage()); err != nil {
		return err
	}
	if err := a.ops.stats.AddQuality(ctx, acc.TakeQuality()); err != nil {
		return err
	}
	if err := a.ops.stats.AddGPUs(ctx, h.GPUs, h.GPUModels); err != nil {
		return err
	}
	if err := a.ops.stats.AddGPUTime(ctx, h.GPUTime, h.Capacity); err != nil {
		return err
	}
	for day := range days {
		if err := a.ops.rollupDay(ctx, day, loc, true); err != nil {
			return err
		}
	}
	return nil
}

// StatisticsCard is a card of an online node as the statistics name it.
type StatisticsCard struct {
	Key         rollup.GPUKey
	Model       string
	MemoryTotal float64
	// Holders are the deployments holding the card, with the share of it
	// each holds (a dGPU slice holds part).
	Holders map[uint]float64
}

// StatisticsCards are the cards of the online nodes and who holds them now.
func (a *App) StatisticsCards() ([]StatisticsCard, error) {
	snap, err := a.store.GPUTimeSnapshot()
	if err != nil {
		return nil, err
	}
	type cardKey struct {
		node  uint
		ident string
	}
	index := map[cardKey]int{}
	out := make([]StatisticsCard, 0, len(snap.Cards))
	for _, c := range snap.Cards {
		node, ok := snap.NodeUUIDs[c.NodeID]
		if !ok || c.Ownership != "" {
			continue
		}
		index[cardKey{c.NodeID, c.Ident}] = len(out)
		out = append(out, StatisticsCard{Key: rollup.GPUKey{NodeUUID: node, GPUIndex: c.Index, GPUUUID: c.UUID},
			Model: c.Name, MemoryTotal: float64(c.MemoryTotal), Holders: map[uint]float64{}})
	}
	for _, h := range snap.Holdings {
		for _, ident := range h.Idents {
			i, ok := index[cardKey{h.NodeID, ident}]
			if !ok {
				continue
			}
			share := 1.0
			if h.VRAMLimitBytes > 0 && out[i].MemoryTotal > 0 {
				share = min(1, float64(h.VRAMLimitBytes)/out[i].MemoryTotal)
			}
			out[i].Holders[h.DeploymentID] += share
		}
	}
	return out, nil
}

// MetricSeries are metric's raw series since sinceMS.
func (a *App) MetricSeries(metric string, sinceMS int64) ([]tsdb.Series, error) {
	if a.obs == nil {
		return nil, errors.New("metrics are not available")
	}
	return a.obs.TSDB().QueryRange(metric, nil, sinceMS, time.Now().UnixMilli(), 1000)
}

// SeedMetrics writes past raw samples, bypassing the live statistics that
// count arriving samples (SeedHistory writes those).
func (a *App) SeedMetrics(samples []tsdb.Sample) error {
	if a.obs == nil {
		return errors.New("metrics are not available")
	}
	return a.obs.TSDB().WriteBatch(samples)
}
