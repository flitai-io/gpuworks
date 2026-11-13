package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The dashboard's card counts are the scheduling overview's own summary, so
// the two pages can never disagree about how many cards are free.
func TestDashboardSummaryCardsAreTheSchedulingOverviewSummary(t *testing.T) {
	n := fleetFixture(t)
	var summary dto.DashboardSummary
	if err := json.Unmarshal(n.memberGet("/api/v1/dashboard/summary"), &summary); err != nil {
		t.Fatal(err)
	}
	var overview dto.SchedulingOverview
	if err := json.Unmarshal(n.memberGet("/api/v1/scheduling/overview"), &overview); err != nil {
		t.Fatal(err)
	}
	if summary.Cards == nil {
		t.Fatal("cards = null, want the overview summary")
	}
	if !reflect.DeepEqual(summary.Cards.OverviewSummary, overview.Summary) || summary.Cards.Allocated != overview.Summary.Awake+overview.Summary.Asleep {
		t.Fatalf("dashboard cards = %+v, scheduling overview summary = %+v", *summary.Cards, overview.Summary)
	}
	if summary.Cards.Cards == 0 {
		t.Fatal("fixture has cards, the summary counted none")
	}
}

// recordingBatches answers CountByStatus from per-status totals and records
// what was asked; every other method is unused by the dashboard.
type recordingBatches struct {
	BatchService
	totals map[string]int
	fail   bool
	asked  [][]string
}

func (b *recordingBatches) CountByStatus(_ BatchCaller, states []string) (map[string]int, error) {
	b.asked = append(b.asked, states)
	if b.fail {
		return nil, errors.New("batch store unavailable")
	}
	out := map[string]int{}
	for _, s := range states {
		if n := b.totals[s]; n > 0 {
			out[s] = n
		}
	}
	return out, nil
}

func TestDashboardTasksCountBatchesByStatusAndRunningBenchmarks(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, state := range []string{"pending", "running", "running", "done", "failed", "canceled"} {
		if err := st.DB().Create(&store.BenchmarkRun{Name: "run-" + state, State: state}).Error; err != nil {
			t.Fatal(err)
		}
	}
	batches := &recordingBatches{totals: map[string]int{"validating": 3, "in_progress": 2, "finalizing": 1, "completed": 9, "failed": 4}}
	h := apiHandlers{d: Deps{Store: st, Batches: batches}}
	got := h.dashboardTasks(httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/summary", nil))
	want := dto.DashboardTasks{Batches: &dto.DashboardBatches{Active: 3, Waiting: 3}, Benchmarks: 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tasks = %+v (batches %+v), want %+v (batches %+v)", got, got.Batches, want, want.Batches)
	}
	if wantAsked := [][]string{{"validating", "in_progress", "finalizing"}}; !reflect.DeepEqual(batches.asked, wantAsked) {
		t.Fatalf("batch counts asked %v, want %v", batches.asked, wantAsked)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const wire = `{"batches":{"active":3,"waiting":3},"benchmarks":3}`
	if string(raw) != wire {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", raw, wire)
	}
}

// Without the Batch API, or when its count fails, the dashboard says it does
// not know rather than showing zero batches.
func TestDashboardTasksLeaveBatchesUnknownWhenTheyCannotBeCounted(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/summary", nil)
	if got := (&apiHandlers{d: Deps{Store: st}}).dashboardTasks(req); got.Batches != nil {
		t.Fatalf("no batch service: batches = %+v, want null", got.Batches)
	}
	if got := (&apiHandlers{d: Deps{Store: st, Batches: &recordingBatches{fail: true}}}).dashboardTasks(req); got.Batches != nil {
		t.Fatalf("failing batch count: batches = %+v, want null", got.Batches)
	}
}
