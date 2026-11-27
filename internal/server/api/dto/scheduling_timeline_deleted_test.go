package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTimelineDeletedDeploymentWire(t *testing.T) {
	from := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	for _, deleted := range []bool{false, true} {
		flag := ""
		if deleted {
			flag = `,"deployment_deleted":true`
		}
		segment := TimelineSegment{DeploymentID: 21, Deployment: "quiet-deleted", DeploymentDeleted: deleted, Instance: "gone", State: "awake", From: from, To: from.Add(time.Hour)}
		segmentWire := `{"deployment_id":21,"deployment":"quiet-deleted"` + flag + `,"instance":"gone","state":"awake","from":"2026-11-25T06:00:00Z","to":"2026-11-25T07:00:00Z"}`
		summary := TimelineSummaryDeployment{DeploymentID: 21, Deployment: "quiet-deleted", DeploymentDeleted: deleted, AwakeSeconds: 3600, State: "awake"}
		summaryWire := `{"deployment_id":21,"deployment":"quiet-deleted"` + flag + `,"awake_seconds":3600,"asleep_seconds":0,"switching_seconds":0,"starting_seconds":0,"state":"awake"}`
		for _, tc := range []struct {
			name     string
			in, back any
			wire     string
		}{
			{"segment", segment, &TimelineSegment{}, segmentWire},
			{"summary", summary, &TimelineSummaryDeployment{}, summaryWire},
		} {
			t.Run(tc.name+flag, func(t *testing.T) {
				if got := marshalString(t, tc.in); got != tc.wire {
					t.Fatalf("marshal = %s, want %s", got, tc.wire)
				}
				decoder := json.NewDecoder(strings.NewReader(tc.wire))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(tc.back); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(reflect.ValueOf(tc.back).Elem().Interface(), tc.in) {
					t.Fatalf("round trip = %+v, want %+v", tc.back, tc.in)
				}
			})
		}
	}
}
