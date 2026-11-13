package deployment

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A stored order is read as the automation runs it: in its zone, with the
// last run it recorded; one in a zone the server lost since is an error, so
// the automation skips it rather than run it in another zone.
func TestAStoredOrderReadsAsTheAutomationRunsIt(t *testing.T) {
	fired := time.Date(2026, 10, 29, 17, 0, 0, 0, time.UTC)
	row := store.LifecycleSchedule{ID: 4, DeploymentID: 9, Action: store.ScheduleActionReplicas, Weekdays: 1<<7 - 1, Minute: 60, Timezone: "Asia/Shanghai", Replicas: 2, Role: "decode", SleepLevel: 0, LastFiredAt: &fired, Enabled: true}
	s, err := AutomationSchedule(row)
	if err != nil {
		t.Fatal(err)
	}
	if s.Location.String() != "Asia/Shanghai" || s.ID != 4 || s.DeploymentID != 9 || s.Minute != 60 || s.Action != store.ScheduleActionReplicas ||
		s.Weekdays != 1<<7-1 || s.Replicas != 2 || s.Role != "decode" || !s.LastFiredAt.Equal(fired) {
		t.Fatalf("schedule = %+v", s)
	}
	row.LastFiredAt = nil
	if s, err := AutomationSchedule(row); err != nil || !s.LastFiredAt.IsZero() {
		t.Fatalf("an order never run = %+v err=%v, want a zero last run", s, err)
	}
	row.Timezone = "Nowhere/Never"
	if _, err := AutomationSchedule(row); err == nil {
		t.Fatal("an order in an unknown zone reads as runnable")
	}
}
