// Package analytics answers usage analytics' queries (ADR 0055): token
// usage, service quality, GPU utilization and card time over any range the
// retention still holds, by hour or by day, shaped for the API.
//
// A day range reads the day rows of the days rolled up and, for a day not
// rolled up yet (today, or one the server was down at the end of), that
// day's hour rows summed into it; an hour range reads hour rows. Days are the
// statistics time zone's.
package analytics

import (
	"context"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Service answers the queries.
type Service struct {
	Store *store.Store
	Stats *statsdb.Store
	// Retention is the data retention in effect (its time zone and hour
	// retention shape every answer).
	Retention   func() store.DataRetention
	LiveSinceMS int64
	Now         func() time.Time
}

// Granularities.
const (
	Hour = "hour"
	Day  = "day"
)

// Bounds of a query: an hour range is at most a week of bars, a day range at
// most the longest day retention.
const (
	maxHourSpan = 7 * 24 * time.Hour
	maxDaySpan  = 1825 * 24 * time.Hour
	// autoHourSpan is the longest range answered by hour when the query
	// names no granularity.
	autoHourSpan = 2 * 24 * time.Hour
)

// queryError is a query the service refuses, a product error the API
// answers 400 with; params name the field and say why.
func queryError(code, field, detail string) error {
	return messages.NewProductError(code, field+": "+detail, map[string]any{"field": field, "detail": detail})
}

// Query is a range and its granularity ("" picks one by the span).
type Query struct {
	FromMS, ToMS int64
	Granularity  string
}

// span is a resolved range.
type span struct {
	daily  bool
	fromMS int64
	toMS   int64
	loc    *time.Location
	// days, for a day range, in order; their starts are the buckets.
	days    []rollup.Day
	buckets []int64
	index   map[int64]int // bucket key (hour_ms or day) -> position
}

func (s *Service) resolve(q Query) (*span, error) {
	if q.FromMS <= 0 || q.ToMS <= q.FromMS {
		return nil, queryError(messages.ErrorAnalyticsRangeInvalid, "from", "from and to are Unix milliseconds, from before to")
	}
	length := time.Duration(q.ToMS-q.FromMS) * time.Millisecond
	g := q.Granularity
	if g == "" {
		g = Hour
		if length > autoHourSpan {
			g = Day
		}
	}
	loc, err := s.Retention().Location()
	if err != nil {
		loc = time.UTC
	}
	sp := &span{loc: loc, index: map[int64]int{}}
	switch g {
	case Hour:
		if length > maxHourSpan {
			return nil, queryError(messages.ErrorAnalyticsRangeTooLong, "granularity", fmt.Sprintf("an hourly range spans at most %d days", int(maxHourSpan.Hours()/24)))
		}
		sp.fromMS = rollup.HourOfMS(q.FromMS)
		sp.toMS = rollup.HourOfMS(q.ToMS-1) + hourMS
		for h := sp.fromMS; h < sp.toMS; h += hourMS {
			sp.index[h] = len(sp.buckets)
			sp.buckets = append(sp.buckets, h)
		}
	case Day:
		if length > maxDaySpan {
			return nil, queryError(messages.ErrorAnalyticsRangeTooLong, "from", "a daily range spans at most 1825 days")
		}
		sp.daily = true
		first := rollup.DayOf(time.UnixMilli(q.FromMS), loc)
		last := rollup.DayOf(time.UnixMilli(q.ToMS-1), loc)
		for d := first; d <= last; d = d.Next() {
			start, _ := d.Bounds(loc)
			sp.index[int64(d)] = len(sp.buckets)
			sp.days = append(sp.days, d)
			sp.buckets = append(sp.buckets, start)
		}
		sp.fromMS, _ = first.Bounds(loc)
		_, sp.toMS = last.Bounds(loc)
	default:
		return nil, queryError(messages.ErrorAnalyticsValueInvalid, "granularity", "hour or day")
	}
	return sp, nil
}

const hourMS = int64(time.Hour / time.Millisecond)

// lastDay is the day after the span's days, the exclusive end of a day range.
func (sp *span) lastDay() rollup.Day { return sp.days[len(sp.days)-1].Next() }

// unrolled are the span's days not in rolled, that the hour rows can still
// stand in for (not after now).
func (sp *span) unrolled(rolled map[rollup.Day]bool, now time.Time) []rollup.Day {
	today := rollup.DayOf(now, sp.loc)
	var out []rollup.Day
	for _, d := range sp.days {
		if !rolled[d] && d <= today {
			out = append(out, d)
		}
	}
	return out
}

// position is where a bucket key lands, or -1 outside the span.
func (sp *span) position(key int64) int {
	if i, ok := sp.index[key]; ok {
		return i
	}
	return -1
}

func (sp *span) rangeDTO(zone string, earliestMS, liveSinceMS int64) dto.AnalyticsRange {
	g := Hour
	if sp.daily {
		g = Day
	}
	return dto.AnalyticsRange{FromMS: sp.fromMS, ToMS: sp.toMS, Granularity: g, TimeZone: zone,
		EarliestMS: earliestMS, LiveSinceMS: liveSinceMS, Buckets: sp.buckets}
}

// hoursOf is a day's hour range in the span's zone.
func (sp *span) hoursOf(d rollup.Day) (int64, int64) { return d.HoursOf(sp.loc) }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// statsRolled are the span's days rolled up in stats.db.
func (s *Service) statsRolled(ctx context.Context, sp *span) (map[rollup.Day]bool, error) {
	return s.Stats.RolledDays(ctx, sp.days[0], sp.lastDay())
}

// earliest is the oldest bucket start a kind holds at the span's
// granularity (a day range also has the hours of days not rolled up).
func (s *Service) earliest(ctx context.Context, sp *span, kind statsdb.Kind) (int64, error) {
	hour, day, err := s.Stats.Earliest(ctx, kind)
	if err != nil || !sp.daily || day == 0 {
		return hour, err
	}
	start, _ := day.Bounds(sp.loc)
	if hour == 0 || start < hour {
		return start, nil
	}
	return hour, nil
}

// deploymentNames names the deployments of ids.
func (s *Service) deploymentNames(ids map[uint]bool) (map[uint]store.DeploymentName, error) {
	list := make([]uint, 0, len(ids))
	for id := range ids {
		if id != 0 {
			list = append(list, id)
		}
	}
	return s.Store.DeploymentNames(list)
}

func ptr(v float64) *float64 { return &v }
