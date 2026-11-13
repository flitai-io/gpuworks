package deployment

import (
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// AutomationSchedule is a stored order as the automation runs it, in the
// zone it names (automation.ScheduleLocation). A zone the server does not
// know (validated on write, so only one the server lost since) is an error:
// the automation skips the order rather than run it in another zone.
func AutomationSchedule(row store.LifecycleSchedule) (automation.Schedule, error) {
	loc, err := automation.ScheduleLocation(row.Timezone)
	if err != nil {
		return automation.Schedule{}, err
	}
	s := automation.Schedule{ID: row.ID, DeploymentID: row.DeploymentID, Action: row.Action, Weekdays: row.Weekdays, Minute: row.Minute, Location: loc, SleepLevel: row.SleepLevel, Replicas: row.Replicas, Role: row.Role}
	if row.LastFiredAt != nil {
		s.LastFiredAt = *row.LastFiredAt
	}
	return s, nil
}
