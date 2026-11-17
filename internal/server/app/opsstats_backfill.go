package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// The one-off fill of the token usage and service quality from before live
// accumulation began, out of the request log (ux.db): the days it still
// holds, newest first, each day in one transaction per store together with
// how far back the fill reaches, so a fill cut short resumes where it stopped
// and adds nothing twice. Rows the request log sampled stand for their
// weight; requests the gateway did not read the usage of then (/v1/messages,
// /v1/responses before it learnt them) stay without tokens. GPU utilization
// and card time are not filled: their raw samples go back only 72 hours and
// reading them all once would cost a fleet more than it is worth.

// backfillPause spaces the days of the fill, so it never competes with the
// live writers for long.
const backfillPause = 200 * time.Millisecond

// backfill runs the fill until done or ctx ends.
func (o *opsStats) backfill(ctx context.Context) {
	if o.obs == nil || o.obs.UX() == nil {
		return
	}
	state, err := o.store.UsageRollupState(o.now())
	if err != nil {
		o.log.Warn("usage fill: read state", "err", err)
		return
	}
	quality, known, err := o.stats.BackfillState(ctx, statsdb.KindQuality)
	if err != nil {
		o.log.Warn("usage fill: read stats state", "err", err)
		return
	}
	if !known {
		// A stats.db made after live accumulation began fills up to it.
		quality = statsdb.Backfill{ThroughMS: o.liveSinceMS}
	}
	usageThrough, usageDone := state.BackfillThroughMS, state.BackfillDone
	if usageDone && quality.Done {
		return
	}
	days, err := o.obs.UX().RequestDays()
	if err != nil {
		o.log.Warn("usage fill: list days", "err", err)
		return
	}
	loc := o.location()
	filled := 0
	for i := len(days) - 1; i >= 0; i-- {
		day := days[i]
		// A day past a store's hour retention would age out before it could
		// roll up; the fill stops there.
		r := o.Retention()
		usageKept := day.StartMS >= o.now().Add(-time.Duration(r.UsageHourlyDays)*24*time.Hour).UnixMilli()
		statsKept := day.StartMS >= o.now().Add(-time.Duration(r.StatsHourlyDays)*24*time.Hour).UnixMilli()
		needUsage := !usageDone && usageKept && day.StartMS < usageThrough
		needQuality := !quality.Done && statsKept && day.StartMS < quality.ThroughMS
		if !needUsage && !needQuality {
			continue
		}
		acc := rollup.NewRequests()
		before := max(usageThrough, quality.ThroughMS)
		err := o.obs.UX().ScanDay(ctx, day.Name, before, func(r uxrecord.Request) error {
			end := r.ReceivedMS + int64(max(r.EndMS, 0))
			if end >= o.liveSinceMS {
				return nil // counted live
			}
			acc.ObserveWeighted(r, int64(r.Weight))
			return nil
		})
		if err != nil {
			o.log.Warn("usage fill: scan", "day", day.Name, "err", err)
			return
		}
		last := i == 0
		usage, qualityDelta := acc.TakeUsage(), acc.TakeQuality()
		touched := touchedDays(usage, qualityDelta, loc)
		if needUsage {
			if err := o.store.AddUsageBackfill(ctx, usage, day.StartMS, last); err != nil {
				o.log.Warn("usage fill: write", "day", day.Name, "err", err)
				return
			}
			usageThrough, usageDone = day.StartMS, last
		}
		if needQuality {
			quality = statsdb.Backfill{ThroughMS: day.StartMS, Done: last}
			if err := o.stats.AddQualityBackfill(ctx, qualityDelta, quality); err != nil {
				o.log.Warn("usage fill: write quality", "day", day.Name, "err", err)
				return
			}
		}
		today := rollup.DayOf(o.now(), loc)
		for d := range touched {
			if d < today {
				if err := o.rollupDay(ctx, d, loc, true); err != nil {
					o.log.Warn("usage fill: roll up", "day", d, "err", err)
				}
			}
		}
		filled++
		select {
		case <-ctx.Done():
			return
		case <-time.After(backfillPause):
		}
	}
	if !usageDone {
		_ = o.store.AddUsageBackfill(ctx, nil, usageThrough, true)
	}
	if !quality.Done {
		_ = o.stats.SetBackfillState(ctx, statsdb.KindQuality, statsdb.Backfill{ThroughMS: quality.ThroughMS, Done: true})
	}
	if filled > 0 {
		o.log.Info("filled token usage and service quality from the request log", "days", filled)
	}
}

// touchedDays are the days of loc the hours of a fill fall in.
func touchedDays(usage map[rollup.UsageKey]rollup.Usage, quality map[rollup.QualityKey]rollup.Quality, loc *time.Location) map[rollup.Day]bool {
	out := map[rollup.Day]bool{}
	for k := range usage {
		out[rollup.DayOf(time.UnixMilli(k.HourMS), loc)] = true
	}
	for k := range quality {
		out[rollup.DayOf(time.UnixMilli(k.HourMS), loc)] = true
	}
	return out
}
