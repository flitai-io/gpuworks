package api

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

var summaryFrom = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

// seg is a segment between offsets from summaryFrom.
func seg(dep uint, state string, from, to time.Duration) dto.TimelineSegment {
	return dto.TimelineSegment{
		DeploymentID: dep, Deployment: fmt.Sprintf("dep-%d", dep), Instance: fmt.Sprintf("inst-%d", dep), State: state,
		From: summaryFrom.Add(from), To: summaryFrom.Add(to),
	}
}

// swaps is count awake stretches of step each, alternating deployments 1 and
// 2, the first beginning at offset first.
func swaps(first, step time.Duration, count int) []dto.TimelineSegment {
	out := make([]dto.TimelineSegment, count)
	for i := range out {
		at := first + time.Duration(i)*step
		out[i] = seg(uint(i%2)+1, "awake", at, at+step)
	}
	return out
}

func summedSeconds(s dto.TimelineSummary) (total int64) {
	for _, d := range s.Deployments {
		total += d.AwakeSeconds + d.AsleepSeconds + d.SwitchingSeconds + d.StartingSeconds
	}
	return total
}

// A card whose segments fit the cap is returned as it is.
func TestSummarizeLeavesAFittingCardAlone(t *testing.T) {
	segs := swaps(0, 18*time.Second, timelineSegmentCap)
	got := summarizeSegments(segs, summaryFrom)
	if len(got.segments) != timelineSegmentCap || got.summary != nil || got.before != nil || got.truncated {
		t.Fatalf("a card at the cap = %d segments, %d summaries, before %v, truncated %v", len(got.segments), len(got.summary), got.before, got.truncated)
	}
	if got := summarizeSegments(nil, summaryFrom); got.segments != nil || got.truncated {
		t.Fatalf("a card with no segments = %+v", got)
	}
}

// The time before the newest 200 segments is cut into 50 slices of equal
// length that follow each other without a gap, the last ending where the
// first raw segment begins.
func TestSummarizeSlicesTheEarlierTimeEqually(t *testing.T) {
	// 600 stretches of 30 s: 5 h. The newest 200 are the last 100 minutes,
	// the earlier 400 the first 3 h 20 m.
	segs := swaps(0, 30*time.Second, 600)
	got := summarizeSegments(segs, summaryFrom)
	if len(got.segments) != timelineSegmentCap || !got.truncated || got.before == nil {
		t.Fatalf("%d raw segments, truncated %v, before %v", len(got.segments), got.truncated, got.before)
	}
	boundary := summaryFrom.Add(400 * 30 * time.Second)
	if !got.before.Equal(boundary) || !got.segments[0].From.Equal(boundary) {
		t.Fatalf("summaries end at %v, the first raw segment begins %v, want both %v", got.before, got.segments[0].From, boundary)
	}
	if len(got.summary) != timelineSummaryBuckets {
		t.Fatalf("%d summaries, want %d", len(got.summary), timelineSummaryBuckets)
	}
	width := boundary.Sub(summaryFrom) / timelineSummaryBuckets
	for i, s := range got.summary {
		if s.Kind != dto.TimelineSummaryKind {
			t.Errorf("summary %d kind = %q", i, s.Kind)
		}
		if !s.From.Equal(summaryFrom.Add(time.Duration(i)*width)) || s.To.Sub(s.From) != width {
			t.Errorf("summary %d = %v..%v, want %v wide from %v", i, s.From, s.To, width, summaryFrom.Add(time.Duration(i)*width))
		}
	}
	if !got.summary[len(got.summary)-1].To.Equal(boundary) {
		t.Errorf("the last summary ends %v, want %v", got.summary[len(got.summary)-1].To, boundary)
	}
}

// A short span gets fewer slices, none shorter than a minute; under a minute
// it is one slice.
func TestSummarizeUsesFewerSlicesForAShortSpan(t *testing.T) {
	for _, tc := range []struct {
		name string
		step time.Duration
		want int
	}{
		// 400 stretches before the boundary: 400 * step is the span.
		{"ten minutes", 1500 * time.Millisecond, 10},
		{"three minutes", 450 * time.Millisecond, 3},
		{"under a minute", 100 * time.Millisecond, 1},
		{"long span", 30 * time.Second, timelineSummaryBuckets},
	} {
		got := summarizeSegments(swaps(0, tc.step, 600), summaryFrom)
		if len(got.summary) != tc.want {
			t.Errorf("%s: %d summaries, want %d", tc.name, len(got.summary), tc.want)
			continue
		}
		for _, s := range got.summary {
			if s.To.Sub(s.From) < min(timelineSummaryMinBucket, got.before.Sub(summaryFrom)) {
				t.Errorf("%s: a slice %v..%v is shorter than a minute", tc.name, s.From, s.To)
			}
		}
	}
}

// A segment that runs across the boundary is split there: its part before
// goes into the summaries, its part after is a raw segment of its own, and
// the raw list still fits the cap.
func TestSummarizeSplitsASegmentAcrossTheBoundary(t *testing.T) {
	// Deployment 3 sleeps on the card through the whole window while 1 and 2
	// swap on it.
	segs := append([]dto.TimelineSegment{seg(3, "asleep", 0, 5*time.Hour)}, swaps(0, 30*time.Second, 600)...)
	segs[0].Level = 1
	got := summarizeSegments(segs, summaryFrom)
	boundary := got.before
	if boundary == nil {
		t.Fatal("nothing was summarized")
	}
	if len(got.segments) > timelineSegmentCap {
		t.Fatalf("%d raw segments, the cap is %d", len(got.segments), timelineSegmentCap)
	}
	var tail *dto.TimelineSegment
	for i := range got.segments {
		if got.segments[i].DeploymentID == 3 {
			tail = &got.segments[i]
		}
	}
	if tail == nil || !tail.From.Equal(*boundary) || !tail.To.Equal(summaryFrom.Add(5*time.Hour)) || tail.State != "asleep" || tail.Level != 1 {
		t.Fatalf("the raw part of the long segment = %+v, want asleep L1 from the boundary %v to 5h", tail, boundary)
	}
	for i := 1; i < len(got.segments); i++ {
		if got.segments[i].From.Before(got.segments[i-1].From) {
			t.Fatalf("raw segments out of order at %d", i)
		}
	}
	// Its earlier part is in the summaries: the whole span before the
	// boundary, asleep.
	var asleep int64
	for _, s := range got.summary {
		for _, d := range s.Deployments {
			if d.DeploymentID == 3 {
				asleep += d.AsleepSeconds
			}
		}
	}
	// Each slice's seconds are rounded, so the sum is out by up to a second a slice.
	if want := int64(boundary.Sub(summaryFrom) / time.Second); asleep < want-int64(len(got.summary)) || asleep > want+int64(len(got.summary)) {
		t.Fatalf("deployment 3 asleep %d s in the summaries, want %d (give or take a second a slice)", asleep, want)
	}
}

// A card changes hands when the awake deployment on it becomes another one.
func TestSummarizeCountsHandChanges(t *testing.T) {
	m := time.Minute
	// Pad the card with the cap's worth of later stretches after minute 10,
	// so the ten minutes before them are summarized in slices of a minute.
	early := []dto.TimelineSegment{
		seg(1, "awake", 0, 2*m),
		seg(1, "asleep", 2*m, 3*m), // going to sleep and waking again is no change of hands
		seg(1, "awake", 3*m, 4*m),
		seg(2, "starting", 3*m, 4*m), // starting is not holding the card awake
		seg(2, "awake", 4*m, 5*m),    // change 1, in the slice minute 4 begins in
		seg(2, "switching", 5*m, 6*m),
		seg(3, "awake", 6*m, 7*m),  // change 2
		seg(1, "awake", 7*m, 8*m),  // change 3
		seg(1, "awake", 8*m, 10*m), // the same deployment's other stretch: none
	}
	segs := append(early, swaps(10*m, m, timelineSegmentCap)...)
	got := summarizeSegments(segs, summaryFrom)
	if got.before == nil || !got.before.Equal(summaryFrom.Add(10*m)) {
		t.Fatalf("summaries end at %v, want minute 10", got.before)
	}
	if len(got.summary) != 10 {
		t.Fatalf("%d summaries for ten minutes, want 10 of a minute", len(got.summary))
	}
	var perMinute []int
	total := 0
	for _, s := range got.summary {
		perMinute = append(perMinute, s.Switches)
		total += s.Switches
	}
	// The change to 2 begins in minute 4, to 3 in minute 6, back to 1 in minute 7.
	want := []int{0, 0, 0, 0, 1, 0, 1, 1, 0, 0}
	for i := range want {
		if perMinute[i] != want[i] {
			t.Fatalf("switches per minute = %v, want %v", perMinute, want)
		}
	}
	if total != 3 {
		t.Fatalf("%d switches, want 3", total)
	}
}

// Each deployment's seconds are those of its states in the slice, an
// instance's overlapping stretches of one state counted once; a slice with
// one holder and no gap adds up to its length.
func TestSummarizeAddsUpTheSecondsOfEachState(t *testing.T) {
	m := time.Minute
	early := []dto.TimelineSegment{
		seg(1, "awake", 0, 20*time.Second),
		seg(1, "switching", 20*time.Second, 30*time.Second),
		seg(1, "asleep", 30*time.Second, 50*time.Second),
		seg(1, "starting", 50*time.Second, m),
		// A second instance of deployment 1 awake while the first slept.
		{DeploymentID: 1, Deployment: "dep-1", Instance: "other", State: "awake", From: summaryFrom.Add(30 * time.Second), To: summaryFrom.Add(m)},
		{DeploymentID: 1, Deployment: "dep-1", Instance: "third", State: "awake", From: summaryFrom.Add(40 * time.Second), To: summaryFrom.Add(m)},
		// Minute 1 is held by deployment 2 alone, half of it.
		seg(2, "awake", m, m+30*time.Second),
	}
	segs := append(early, swaps(2*m, m, timelineSegmentCap)...)
	got := summarizeSegments(segs, summaryFrom)
	if len(got.summary) != 2 {
		t.Fatalf("%d summaries, want 2 (the minute where nothing differs is not skipped, the span is two minutes)", len(got.summary))
	}
	first, second := got.summary[0], got.summary[1]
	one := first.Deployments[0]
	if one.DeploymentID != 1 || one.AwakeSeconds != 20+30 || one.SwitchingSeconds != 10 || one.AsleepSeconds != 20 || one.StartingSeconds != 10 {
		t.Fatalf("deployment 1 in minute 0 = %+v, want awake 50 (20 + the other instances' 30 counted once), switching 10, asleep 20, starting 10", one)
	}
	// 20 awake + 10 + 20 + 10 of the first instance is a minute; the others'
	// awake seconds overlap its asleep and starting ones.
	if len(second.Deployments) != 1 || second.Deployments[0].DeploymentID != 2 || second.Deployments[0].AwakeSeconds != 30 || summedSeconds(second) != 30 {
		t.Fatalf("minute 1 = %+v, want deployment 2 awake 30 s and nothing else (the other 30 s are a gap)", second)
	}
	// One holder, no gap: the seconds are the slice's length.
	solo := append([]dto.TimelineSegment{seg(1, "awake", 0, 30*time.Second), seg(1, "asleep", 30*time.Second, m)}, swaps(m, m, timelineSegmentCap)...)
	one2 := summarizeSegments(solo, summaryFrom).summary
	if len(one2) != 1 || summedSeconds(one2[0]) != 60 {
		t.Fatalf("a held minute = %+v, want seconds adding up to 60", one2)
	}
}

// The deployment awake longest is the dominant one, the lower id on a tie,
// and a slice nobody was awake in names none.
func TestSummarizeNamesTheDominantDeployment(t *testing.T) {
	m := time.Minute
	early := []dto.TimelineSegment{
		seg(5, "awake", 0, 20*time.Second),
		seg(4, "awake", 20*time.Second, 60*time.Second), // longest in minute 0
		seg(5, "awake", m, m+30*time.Second),
		seg(4, "awake", m+30*time.Second, 2*m), // a tie in minute 1
		seg(6, "asleep", 2*m, 3*m),             // nobody awake in minute 2
	}
	segs := append(early, swaps(3*m, m, timelineSegmentCap)...)
	got := summarizeSegments(segs, summaryFrom)
	if len(got.summary) != 3 {
		t.Fatalf("%d summaries, want 3", len(got.summary))
	}
	for i, want := range []uint{4, 4, 0} {
		if got.summary[i].DominantDeploymentID != want {
			t.Errorf("minute %d dominant = %d, want %d", i, got.summary[i].DominantDeploymentID, want)
		}
	}
	if got.summary[0].Dominant != "dep-4" || got.summary[2].Dominant != "" {
		t.Errorf("dominant names = %q, %q", got.summary[0].Dominant, got.summary[2].Dominant)
	}
	// The deployments in a slice are listed by awake time, the longest first.
	if first := got.summary[0].Deployments; first[0].DeploymentID != 4 || first[1].DeploymentID != 5 {
		t.Errorf("minute 0 lists %+v, want 4 then 5", first)
	}
}

// Whatever the card did, it lists at most the cap of raw segments plus 50
// summaries, and only slices something held the card in.
func TestSummarizeBoundsTheOutput(t *testing.T) {
	for _, n := range []int{201, 1000, 20000} {
		got := summarizeSegments(swaps(0, 18*time.Second, n), summaryFrom)
		if len(got.segments) > timelineSegmentCap || len(got.summary) > timelineSummaryBuckets || len(got.segments)+len(got.summary) > timelineSegmentCap+timelineSummaryBuckets {
			t.Errorf("%d segments: %d raw + %d summaries", n, len(got.segments), len(got.summary))
		}
		if !got.truncated || got.before == nil {
			t.Errorf("%d segments: truncated %v, before %v", n, got.truncated, got.before)
		}
	}
	// A gap holds no slice: 300 stretches in the first minute and 300 after
	// an hour of nothing.
	segs := append(swaps(0, 100*time.Millisecond, 300), swaps(time.Hour, 100*time.Millisecond, 300)...)
	got := summarizeSegments(segs, summaryFrom)
	for _, s := range got.summary {
		if len(s.Deployments) == 0 {
			t.Fatalf("an empty slice was listed: %+v", s)
		}
	}
}

func TestTimelineSummaryPreservesDeletedDeployment(t *testing.T) {
	segments := swaps(0, 30*time.Second, 600)
	for i := range segments {
		segments[i].DeploymentDeleted = segments[i].DeploymentID == 1
	}
	got := summarizeSegments(segments, summaryFrom)
	if len(got.summary) == 0 {
		t.Fatal("expected summaries")
	}
	for _, slice := range got.summary {
		for _, dep := range slice.Deployments {
			wire, err := json.Marshal(dep)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if dep.DeploymentID == 1 && fields["deployment_deleted"] != true {
				t.Fatalf("deleted summary = %s", wire)
			}
			if dep.DeploymentID == 2 {
				if _, ok := fields["deployment_deleted"]; ok {
					t.Fatalf("live summary = %s", wire)
				}
			}
		}
	}
	for _, segment := range got.segments {
		if segment.DeploymentDeleted != (segment.DeploymentID == 1) {
			t.Fatalf("raw tail lost deletion flag: %+v", segment)
		}
	}
}
