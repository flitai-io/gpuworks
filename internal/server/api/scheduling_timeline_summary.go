package api

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	// timelineSummaryBuckets is the most summaries one card gets for the
	// time its raw segments no longer cover. With the 200 raw segments a
	// card lists at most 250 elements (summaries plus segments), whatever
	// the window.
	timelineSummaryBuckets = 50
	// timelineSummaryMinBucket is the shortest slice a summary covers: a
	// span shorter than 50 of them gets fewer, wider slices rather than
	// slivers. A minute is a slice's worth of swaps at the pace that
	// overflowed the cap (one every ~18 s) and, on the 15-minute window, one
	// fifteenth of the chart; shorter slices would say little more and
	// need rounding finer than the whole seconds the answer carries.
	timelineSummaryMinBucket = time.Minute
)

// summarizedCard is a card's segments after the cap: the newest raw, the
// time before them summarized.
type summarizedCard struct {
	segments []dto.TimelineSegment
	summary  []dto.TimelineSummary
	// before is where the summaries end (nil: nothing was summarized).
	before *time.Time
	// truncated says some history is not listed raw.
	truncated bool
}

// stateIndex orders the four states in a depAcc.
func stateIndex(state string) int {
	switch state {
	case string(store.ResidencyAwake):
		return 0
	case string(store.ResidencyAsleep):
		return 1
	case string(store.ResidencySwitching):
		return 2
	}
	return 3
}

// depAcc is what one deployment did in one slice: per state, the milliseconds
// covered and where the last stretch counted ended (the segments come in
// order of their start, so a stretch that overlaps the one before adds only
// what runs past it).
type depAcc struct {
	name    string
	deleted bool
	ms      [4]int64
	end     [4]int64
	// asleep splits the asleep time by the sleep level the segments carry
	// (those with none known are left out), counted like ms.
	asleep map[int]*levelAcc
}

// levelAcc is the asleep time of one sleep level in a slice.
type levelAcc struct{ ms, end int64 }

// stateTieOrder is the order states win a tie of seconds in, as indexes of
// depAcc: awake first (it is what the card is for), then the transitions
// switching and starting (short by nature, so a slice that spent as long in
// one as asleep is worth showing as the change it was), asleep last (the
// resting state, which says the least).
var stateTieOrder = [4]int{0, 2, 3, 1}

// dominantState is the state the deployment spent the most time in during the
// slice (counting milliseconds, before the wire rounds them to seconds), the
// first of stateTieOrder on a tie, and, for asleep, the sleep level with most
// asleep time among those the segments name (the shallower one on a tie), 0
// when none is known. A deployment listed in a slice has time in some state.
func (d *depAcc) dominantState() (string, int) {
	best := stateTieOrder[0]
	for _, i := range stateTieOrder[1:] {
		if d.ms[i] > d.ms[best] {
			best = i
		}
	}
	state := [4]store.ResidencyState{store.ResidencyAwake, store.ResidencyAsleep, store.ResidencySwitching, store.ResidencyStarting}[best]
	if best != 1 {
		return string(state), 0
	}
	level := 0
	for l, acc := range d.asleep {
		if level == 0 || acc.ms > d.asleep[level].ms || (acc.ms == d.asleep[level].ms && l < level) {
			level = l
		}
	}
	return string(state), level
}

// bucketAcc is one slice being filled in.
type bucketAcc struct {
	deps     map[uint]*depAcc
	switches int
}

func (b *bucketAcc) add(seg dto.TimelineSegment, from, to int64) {
	d := b.deps[seg.DeploymentID]
	if d == nil {
		d = &depAcc{name: seg.Deployment, deleted: seg.DeploymentDeleted}
		b.deps[seg.DeploymentID] = d
	}
	i := stateIndex(seg.State)
	if start := max(from, d.end[i]); to > start {
		d.ms[i] += to - start
	}
	d.end[i] = max(d.end[i], to)
	if i == 1 && seg.Level > 0 {
		if d.asleep == nil {
			d.asleep = map[int]*levelAcc{}
		}
		l := d.asleep[seg.Level]
		if l == nil {
			l = &levelAcc{}
			d.asleep[seg.Level] = l
		}
		if start := max(from, l.end); to > start {
			l.ms += to - start
		}
		l.end = max(l.end, to)
	}
}

// summarizeSegments keeps a card's newest timelineSegmentCap segments raw
// and summarizes the rest of the window [from, ...) in at most
// timelineSummaryBuckets slices of equal length (fewer when the span is
// short). segs are in order of their start. A card with no more segments than
// the cap is returned as it is.
//
// The boundary is the start of the first segment kept raw. A segment from
// before it that runs past it is split: its part before the boundary is
// summarized, its part after stays raw as a segment of its own, so the raw
// list may exceed the cap by those parts; the boundary moves forward until it
// does not. One pass over the segments and a few over the (few) segments that
// run past the boundary, no store read.
//
// Switches follow the awake stretches in order of their start across the
// whole summarized part: each one whose deployment is not the previous one's
// is a switch, counted in the slice it begins in. This is the card's hand
// changes, not the deployments' yields and wakes of the sharing stats: one
// swap of A for B is one switch here and a yield of A plus a wake of B
// there.
func summarizeSegments(segs []dto.TimelineSegment, from time.Time) summarizedCard {
	n := len(segs)
	if n <= timelineSegmentCap {
		return summarizedCard{segments: segs}
	}
	first := n - timelineSegmentCap
	var tails []int
	for round := 0; ; round++ {
		boundary := segs[first].From
		tails = tails[:0]
		for j := 0; j < first; j++ {
			if segs[j].To.After(boundary) {
				tails = append(tails, j)
			}
		}
		excess := (n - first) + len(tails) - timelineSegmentCap
		if excess <= 0 || first >= n-1 || round >= 3 {
			break
		}
		first = min(first+excess, n-1)
	}
	boundary := segs[first].From
	raw := make([]dto.TimelineSegment, 0, n-first+len(tails))
	for _, j := range tails {
		tail := segs[j]
		tail.From = boundary
		raw = append(raw, tail)
	}
	raw = append(raw, segs[first:]...)
	sort.SliceStable(raw, func(i, j int) bool {
		if !raw[i].From.Equal(raw[j].From) {
			return raw[i].From.Before(raw[j].From)
		}
		return raw[i].Instance < raw[j].Instance
	})
	out := summarizedCard{segments: raw, truncated: true}
	if len(raw) > timelineSegmentCap {
		// Only a card with hundreds of segments open at once gets here.
		out.segments = raw[len(raw)-timelineSegmentCap:]
	}
	fromMS, toMS := from.UnixMilli(), boundary.UnixMilli()
	if toMS <= fromMS {
		return out
	}
	out.summary = summarizeBefore(segs[:first], fromMS, toMS)
	if len(out.summary) > 0 {
		b := boundary.UTC()
		out.before = &b
	}
	return out
}

// summarizeBefore is the summaries of segs over [fromMS, toMS).
func summarizeBefore(segs []dto.TimelineSegment, fromMS, toMS int64) []dto.TimelineSummary {
	span := toMS - fromMS
	count := int(min(max(span/timelineSummaryMinBucket.Milliseconds(), 1), timelineSummaryBuckets))
	edge := func(k int) int64 { return fromMS + span*int64(k)/int64(count) }
	bucketOf := func(ms int64) int { return min(int((ms-fromMS)*int64(count)/span), count-1) }
	buckets := make([]bucketAcc, count)
	var lastAwake uint
	for _, seg := range segs {
		start, end := max(seg.From.UnixMilli(), fromMS), min(seg.To.UnixMilli(), toMS)
		if end <= start {
			continue
		}
		first := bucketOf(start)
		if seg.State == string(store.ResidencyAwake) {
			if lastAwake != 0 && lastAwake != seg.DeploymentID {
				buckets[first].switches++
			}
			lastAwake = seg.DeploymentID
		}
		for k := first; k < count && edge(k) < end; k++ {
			b := &buckets[k]
			if b.deps == nil {
				b.deps = map[uint]*depAcc{}
			}
			b.add(seg, max(start, edge(k)), min(end, edge(k+1)))
		}
	}
	var out []dto.TimelineSummary
	for k := range buckets {
		if len(buckets[k].deps) == 0 {
			continue
		}
		out = append(out, buckets[k].wire(time.UnixMilli(edge(k)).UTC(), time.UnixMilli(edge(k+1)).UTC()))
	}
	return out
}

// wire is the slice as the answer lists it: the deployments by the time they
// were awake, longest first (the lower id first on a tie), the first of
// which that was awake at all is the dominant one.
func (b *bucketAcc) wire(from, to time.Time) dto.TimelineSummary {
	ids := make([]uint, 0, len(b.deps))
	for id := range b.deps {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if a, c := b.deps[ids[i]].ms[0], b.deps[ids[j]].ms[0]; a != c {
			return a > c
		}
		return ids[i] < ids[j]
	})
	out := dto.TimelineSummary{Kind: dto.TimelineSummaryKind, From: from, To: to, Switches: b.switches, Deployments: make([]dto.TimelineSummaryDeployment, len(ids))}
	for i, id := range ids {
		d := b.deps[id]
		state, level := d.dominantState()
		out.Deployments[i] = dto.TimelineSummaryDeployment{
			DeploymentID: id, Deployment: d.name, DeploymentDeleted: d.deleted, State: state, Level: level,
			AwakeSeconds: roundedSeconds(d.ms[0]), AsleepSeconds: roundedSeconds(d.ms[1]),
			SwitchingSeconds: roundedSeconds(d.ms[2]), StartingSeconds: roundedSeconds(d.ms[3]),
		}
	}
	if top := b.deps[ids[0]]; top.ms[0] > 0 {
		out.DominantDeploymentID, out.Dominant = ids[0], top.name
	}
	return out
}
