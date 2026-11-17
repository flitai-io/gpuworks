package uxstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Pages of a deployment's log across three day partitions, with timestamps
// shared by several rows and another deployment interleaved, come back in
// (ts, insertion) order newest first with no duplicate and no gap, and rows
// arriving between pages do not shift later pages.
func TestCursorPaginationAcrossPartitions(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	type obs struct {
		id  string
		ts  int64
		seq int // observe order
	}
	var want []obs
	n := 0
	for d := 2; d >= 0; d-- {
		base := ms(day0.AddDate(0, 0, -d).Add(-6 * time.Hour))
		for i := range 37 {
			ts := base + int64(i/3)*1000 // three rows per timestamp
			id := fmt.Sprintf("d%d-%02d", d, i)
			r := okReq(id, 1, ts)
			r.APIKeyID = uint(1 + i%2)
			s.Observe(r)
			want = append(want, obs{id, ts, n})
			n++
			s.Observe(okReq("other-"+id, 2, ts))
		}
		mustSync(t, s, false) // one flush per day: several transactions
	}
	slices.SortFunc(want, func(a, b obs) int {
		if a.ts != b.ts {
			return int(b.ts - a.ts)
		}
		return b.seq - a.seq
	})

	ctx := context.Background()
	q := RequestQuery{DeploymentID: 1, Limit: 10}
	var got []string
	for page := 0; ; page++ {
		rows, next, err := s.Requests(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			got = append(got, r.ID)
		}
		if next == "" {
			if len(rows) == 0 || len(rows) > 10 {
				t.Fatalf("last page has %d rows", len(rows))
			}
			break
		}
		if len(rows) != 10 {
			t.Fatalf("page %d has %d rows with a next cursor", page, len(rows))
		}
		if page == 2 { // newer rows land mid-walk
			s.Observe(okReq("new", 1, ms(day0)))
			mustSync(t, s, false)
		}
		q.Cursor = next
	}
	wantIDs := make([]string, len(want))
	for i, o := range want {
		wantIDs[i] = o.id
	}
	if !slices.Equal(got, wantIDs) {
		t.Fatalf("paged ids:\n got %v\nwant %v", got, wantIDs)
	}

	// The time range and the key filter combine with paging.
	from := ms(day0.AddDate(0, 0, -1).Add(-6 * time.Hour))
	to := from + 5000
	q = RequestQuery{DeploymentID: 1, APIKeyID: 2, FromMS: from, ToMS: to, Limit: 2}
	var ranged []string
	for _, r := range allRequests(t, s, q) {
		if r.ReceivedMS < from || r.ReceivedMS > to || r.APIKeyID != 2 {
			t.Fatalf("row outside the filter: %+v", r)
		}
		ranged = append(ranged, r.ID)
	}
	// d1 rows 0..17 are within 5 s (ts = base + i/3 s); key 2 is the odd i.
	if want := []string{"d1-17", "d1-15", "d1-13", "d1-11", "d1-09", "d1-07", "d1-05", "d1-03", "d1-01"}; !slices.Equal(ranged, want) {
		t.Fatalf("ranged = %v, want %v", ranged, want)
	}
}

func TestRequestLimitDefaultsAndCap(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 10000
	s, _ := openTest(t, opts)
	for i := range 600 {
		s.Observe(okReq(fmt.Sprint(i), 1, ms(day0)+int64(i)))
	}
	mustSync(t, s, false)
	ctx := context.Background()
	for _, tc := range []struct{ limit, want int }{{0, DefaultRequestLimit}, {-3, DefaultRequestLimit}, {7, 7}, {10000, MaxRequestLimit}} {
		rows, next, err := s.Requests(ctx, RequestQuery{DeploymentID: 1, Limit: tc.limit})
		if err != nil || len(rows) != tc.want || next == "" {
			t.Errorf("limit %d: %d rows next %q err %v, want %d", tc.limit, len(rows), next, err, tc.want)
		}
	}
}

// The result filter in SQL selects exactly the records uxrecord.ResultOf
// gives that result, for each result alone and for a combination.
func TestResultFilterMatchesGoRule(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 1000
	s, _ := openTest(t, opts)
	muts := map[string]func(r *uxrecord.Request){
		"stop":              func(*uxrecord.Request) {},
		"no-finish":         func(r *uxrecord.Request) { r.FinishReason = "" },
		"length":            func(r *uxrecord.Request) { r.FinishReason = "length" },
		"status-201":        func(r *uxrecord.Request) { r.Status = 201 },
		"status-429":        func(r *uxrecord.Request) { r.Status = 429 },
		"status-500-closed": func(r *uxrecord.Request) { r.Status, r.Outcome = 500, uxrecord.OutcomeClientClosed },
		"upstream-err":      func(r *uxrecord.Request) { r.Status, r.Outcome = 502, uxrecord.OutcomeUpstreamError },
		"closed":            func(r *uxrecord.Request) { r.Outcome = uxrecord.OutcomeClientClosed },
		"closed-at-length":  func(r *uxrecord.Request) { r.Outcome, r.FinishReason = uxrecord.OutcomeClientClosed, "length" },
		"truncated":         func(r *uxrecord.Request) { r.Outcome = uxrecord.OutcomeTruncated },
	}
	byResult := map[uxrecord.Result][]string{}
	i := 0
	for id, mut := range muts {
		r := okReq(id, 1, ms(day0)+int64(i))
		i++
		mut(&r)
		s.Observe(r)
		byResult[uxrecord.ResultOf(r)] = append(byResult[uxrecord.ResultOf(r)], id)
	}
	mustSync(t, s, false)
	ids := func(q RequestQuery) []string {
		var out []string
		for _, r := range allRequests(t, s, q) {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	for _, result := range uxrecord.Results() {
		want := append([]string(nil), byResult[result]...)
		slices.Sort(want)
		if got := ids(RequestQuery{DeploymentID: 1, Results: []uxrecord.Result{result}}); !slices.Equal(got, want) {
			t.Errorf("%s: SQL %v, Go rule %v", result, got, want)
		}
	}
	want := append(append([]string(nil), byResult[uxrecord.ResultClientClosed]...), byResult[uxrecord.ResultError]...)
	slices.Sort(want)
	if got := ids(RequestQuery{DeploymentID: 1, Results: []uxrecord.Result{uxrecord.ResultClientClosed, uxrecord.ResultError}}); !slices.Equal(got, want) {
		t.Errorf("closed or error: SQL %v, Go rule %v", got, want)
	}
	for _, result := range uxrecord.Results() {
		if len(byResult[result]) == 0 {
			t.Fatalf("fixture drifted: no %s record", result)
		}
	}
	if _, _, err := s.Requests(context.Background(), RequestQuery{DeploymentID: 1, Results: []uxrecord.Result{"slow"}}); !errors.Is(err, ErrBadQuery) {
		t.Fatalf("unknown result: err %v, want ErrBadQuery", err)
	}
}

// The end-to-end range and the replica narrow the page; an unknown end time
// never falls inside a closed range.
func TestRequestsFilterByEndToEndTimeAndReplica(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 1000
	s, _ := openTest(t, opts)
	for i, tc := range []struct {
		id       string
		endMS    int32
		instance string
	}{
		{"fast-a", 300, "inst-a"}, {"mid-a", 2000, "inst-a"}, {"slow-b", 9000, "inst-b"}, {"unknown-b", uxrecord.Unknown, "inst-b"},
	} {
		r := okReq(tc.id, 1, ms(day0)+int64(i))
		r.EndMS, r.InstanceUUID = tc.endMS, tc.instance
		s.Observe(r)
	}
	mustSync(t, s, false)
	ids := func(q RequestQuery) []string {
		var out []string
		for _, r := range allRequests(t, s, q) {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		q    RequestQuery
		want []string
	}{
		{RequestQuery{DeploymentID: 1, MinEndMS: 1000}, []string{"mid-a", "slow-b"}},
		{RequestQuery{DeploymentID: 1, MaxEndMS: 2000}, []string{"fast-a", "mid-a"}},
		{RequestQuery{DeploymentID: 1, MinEndMS: 1000, MaxEndMS: 5000}, []string{"mid-a"}},
		{RequestQuery{DeploymentID: 1, InstanceUUID: "inst-b"}, []string{"slow-b", "unknown-b"}},
		{RequestQuery{DeploymentID: 1, InstanceUUID: "inst-b", MinEndMS: 1}, []string{"slow-b"}},
	} {
		if got := ids(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.q, got, tc.want)
		}
	}
}

// The TTFT range keeps streamed requests whose first token, reasoning or
// answer, falls inside it; a reply that does not stream, or one that never
// produced a token, has none and never matches.
func TestRequestsFilterByTTFT(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.RowsPerSecond = 1000
	s, _ := openTest(t, opts)
	for i, tc := range []struct {
		id            string
		stream        bool
		think, answer int32
	}{
		{"fast", true, uxrecord.Unknown, 200},
		{"thinks-first", true, 800, 3000},
		{"answers-late", true, uxrecord.Unknown, 3000},
		{"no-stream", false, uxrecord.Unknown, uxrecord.Unknown},
		{"no-token", true, uxrecord.Unknown, uxrecord.Unknown},
	} {
		r := okReq(tc.id, 1, ms(day0)+int64(i))
		r.Stream, r.FirstThinkMS, r.FirstAnswerMS, r.EndMS = tc.stream, tc.think, tc.answer, 5000
		s.Observe(r)
	}
	mustSync(t, s, false)
	ids := func(q RequestQuery) []string {
		var out []string
		for _, r := range allRequests(t, s, q) {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	for _, tc := range []struct {
		q    RequestQuery
		want []string
	}{
		{RequestQuery{DeploymentID: 1, MinTTFTMS: 500}, []string{"answers-late", "thinks-first"}},
		{RequestQuery{DeploymentID: 1, MaxTTFTMS: 1000}, []string{"fast", "thinks-first"}},
		{RequestQuery{DeploymentID: 1, MinTTFTMS: 500, MaxTTFTMS: 1000}, []string{"thinks-first"}},
		{RequestQuery{DeploymentID: 1, MinTTFTMS: 100, MaxEndMS: 4000}, nil},
	} {
		if got := ids(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %v, want %v", tc.q, got, tc.want)
		}
	}
}
