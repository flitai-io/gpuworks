package analytics

import (
	"context"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
)

func qualityFigures(q rollup.Quality) dto.QualityFigures {
	out := dto.QualityFigures{Requests: q.Requests, Completed: q.Completed, Length: q.Length,
		ClientClosed: q.ClientClosed, Interrupted: q.Interrupted, Error: q.Error}
	if v, ok := q.TTFT.Quantile(0.5); ok {
		out.TTFTP50MS = ptr(v)
	}
	if v, ok := q.TTFT.Quantile(0.95); ok {
		out.TTFTP95MS = ptr(v)
	}
	if v, ok := q.EndToEnd.Quantile(0.5); ok {
		out.E2EP50MS = ptr(v)
	}
	if v, ok := q.EndToEnd.Quantile(0.95); ok {
		out.E2EP95MS = ptr(v)
	}
	if q.DecodeMS > 0 {
		out.OutputTokensPerSec = ptr(float64(q.OutputTokens) * 1000 / float64(q.DecodeMS))
	}
	return out
}

// statsParts are the reads a span takes from stats.db: the whole range of
// one table, or the rolled-up days plus each unrolled day's hours.
func (s *Service) statsParts(ctx context.Context, sp *span) ([]statsdb.Range, []int64, error) {
	if !sp.daily {
		return []statsdb.Range{{From: sp.fromMS, To: sp.toMS}}, []int64{-1}, nil
	}
	parts := []statsdb.Range{{Daily: true, From: int64(sp.days[0]), To: int64(sp.lastDay())}}
	// bucket -1: the rows carry their own bucket key; otherwise every row of
	// the part lands in that day.
	buckets := []int64{-1}
	rolled, err := s.statsRolled(ctx, sp)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range sp.unrolled(rolled, s.now()) {
		from, to := sp.hoursOf(d)
		parts = append(parts, statsdb.Range{From: from, To: to})
		buckets = append(buckets, int64(d))
	}
	return parts, buckets, nil
}

// Quality answers the service quality over a range, of one deployment when
// deploymentID is not 0.
func (s *Service) Quality(ctx context.Context, q Query, deploymentID uint) (dto.QualityAnalytics, error) {
	sp, err := s.resolve(q)
	if err != nil {
		return dto.QualityAnalytics{}, err
	}
	parts, keys, err := s.statsParts(ctx, sp)
	if err != nil {
		return dto.QualityAnalytics{}, err
	}
	series := make([]rollup.Quality, len(sp.buckets))
	byDep := map[uint]*rollup.Quality{}
	byClass := map[string]*rollup.Quality{}
	var total rollup.Quality
	for i, part := range parts {
		rows, err := s.Stats.Quality(ctx, part, deploymentID)
		if err != nil {
			return dto.QualityAnalytics{}, err
		}
		for _, row := range rows {
			key := row.Bucket
			if keys[i] >= 0 {
				key = keys[i]
			}
			if at := sp.position(key); at >= 0 {
				series[at].Add(row.Quality)
			}
			cur := byDep[row.DeploymentID]
			if cur == nil {
				cur = &rollup.Quality{}
				byDep[row.DeploymentID] = cur
			}
			cur.Add(row.Quality)
			class := byClass[row.PriorityClass]
			if class == nil {
				class = &rollup.Quality{}
				byClass[row.PriorityClass] = class
			}
			class.Add(row.Quality)
			total.Add(row.Quality)
		}
	}
	out := dto.QualityAnalytics{Totals: qualityFigures(total), Series: make([]dto.QualityFigures, len(series)), Rows: []dto.QualityRow{}, Classes: []dto.QualityClassRow{}}
	for name, q := range byClass {
		out.Classes = append(out.Classes, dto.QualityClassRow{PriorityClass: name, QualityFigures: qualityFigures(*q)})
	}
	sort.Slice(out.Classes, func(i, j int) bool {
		if out.Classes[i].Requests != out.Classes[j].Requests {
			return out.Classes[i].Requests > out.Classes[j].Requests
		}
		return out.Classes[i].PriorityClass < out.Classes[j].PriorityClass
	})
	for i, b := range series {
		out.Series[i] = qualityFigures(b)
	}
	ids := map[uint]bool{}
	for id := range byDep {
		ids[id] = true
	}
	names, err := s.deploymentNames(ids)
	if err != nil {
		return out, err
	}
	for id, qd := range byDep {
		row := dto.QualityRow{DeploymentID: id, QualityFigures: qualityFigures(*qd)}
		if n, ok := names[id]; ok {
			row.DeploymentName, row.ServedName, row.DeploymentDeleted = n.Name, n.ServedName, n.Deleted
		}
		out.Rows = append(out.Rows, row)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].Requests != out.Rows[j].Requests {
			return out.Rows[i].Requests > out.Rows[j].Requests
		}
		return out.Rows[i].DeploymentID < out.Rows[j].DeploymentID
	})
	earliest, err := s.earliest(ctx, sp, statsdb.KindQuality)
	if err != nil {
		return out, err
	}
	out.Range = sp.rangeDTO(s.Retention().TimeZone, earliest, s.LiveSinceMS)
	return out, nil
}
