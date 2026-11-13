package api

import (
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestScheduleRoutesRequireAuthAndFindTheirTarget(t *testing.T) {
	rig := newScheduleRig(t)
	own := &store.LifecycleSchedule{DeploymentID: rig.sleepable.ID, Action: store.ScheduleActionWake, Weekdays: 1 << 1, Minute: 8 * 60, Enabled: true}
	other := &store.LifecycleSchedule{DeploymentID: rig.plain.ID, Action: store.ScheduleActionWake, Weekdays: 1 << 1, Minute: 9 * 60, Enabled: true}
	for _, row := range []*store.LifecycleSchedule{own, other} {
		if err := rig.st.CreateLifecycleSchedule(row); err != nil {
			t.Fatal(err)
		}
	}
	const wakeBody = `{"action":"wake","weekdays":[1],"time":"07:00"}`
	routes := []struct {
		method, tail, body string
	}{
		{http.MethodGet, "schedule", ""},
		{http.MethodPost, "schedule", wakeBody},
		{http.MethodPut, fmt.Sprintf("schedule/%d", own.ID), wakeBody},
		{http.MethodDelete, fmt.Sprintf("schedule/%d", own.ID), ""},
		{http.MethodPut, "node-replacement", `{"after_sec":600}`},
		{http.MethodDelete, "wake-waiting", ""},
	}

	t.Run("no token is 401 and changes nothing", func(t *testing.T) {
		for _, route := range routes {
			rec := rig.do("", route.method, schedPath(rig.sleepable, route.tail), route.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d %s, want 401", route.method, route.tail, rec.Code, rec.Body)
			}
		}
		if rows, _ := rig.st.LifecycleSchedules(rig.sleepable.ID); len(rows) != 1 {
			t.Fatalf("an unauthenticated request changed the schedule: %+v", rows)
		}
		if _, err := rig.st.LifecycleSchedule(rig.sleepable.ID, own.ID); err != nil {
			t.Fatalf("unauthenticated delete removed the order: %v", err)
		}
	})

	t.Run("a read token reads but cannot change", func(t *testing.T) {
		read := rig.mint("read")
		for _, route := range routes {
			rec := rig.do(read, route.method, schedPath(rig.sleepable, route.tail), route.body)
			if route.method == http.MethodGet {
				if rec.Code != http.StatusOK {
					t.Fatalf("read token GET %s = %d %s", route.tail, rec.Code, rec.Body)
				}
				continue
			}
			if rec.Code != http.StatusForbidden || decodeBody(t, rec)["code"] != "PLATFORM_TOKEN_READ_ONLY" {
				t.Fatalf("read token %s %s = %d %s, want 403 PLATFORM_TOKEN_READ_ONLY", route.method, route.tail, rec.Code, rec.Body)
			}
		}
		if rows, _ := rig.st.LifecycleSchedules(rig.sleepable.ID); len(rows) != 1 || rows[0].Minute != 8*60 {
			t.Fatalf("a refused read-token request changed the schedule: %+v", rows)
		}
		if dep, _ := rig.st.GetDeployment(rig.sleepable.ID); dep.NodeReplaceAfterSec != 600 {
			t.Fatalf("a refused read-token request changed the settings: replace=%d", dep.NodeReplaceAfterSec)
		}
	})

	t.Run("a write token manages the schedule", func(t *testing.T) {
		write := rig.mint("write")
		rec := rig.do(write, http.MethodPost, schedPath(rig.sleepable, "schedule"), wakeBody)
		if rec.Code != http.StatusCreated {
			t.Fatalf("write token POST = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("unknown deployment or order is 404", func(t *testing.T) {
		for _, route := range routes {
			rec := rig.do(rig.session, route.method, "/api/v1/deployments/9999/"+route.tail, route.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("unknown deployment: %s %s = %d %s", route.method, route.tail, rec.Code, rec.Body)
			}
		}
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			rec := rig.do(rig.session, method, schedPath(rig.sleepable, "schedule/9999"), wakeBody)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("unknown order: %s = %d %s", method, rec.Code, rec.Body)
			}
			// An order of another deployment is not this deployment's.
			rec = rig.do(rig.session, method, schedPath(rig.sleepable, fmt.Sprintf("schedule/%d", other.ID)), wakeBody)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("another deployment's order: %s = %d %s", method, rec.Code, rec.Body)
			}
		}
		if got, err := rig.st.LifecycleSchedule(rig.plain.ID, other.ID); err != nil || got.Minute != 9*60 || got.Action != store.ScheduleActionWake {
			t.Fatalf("another deployment's order was touched: %+v %v", got, err)
		}
		if rec := rig.do(rig.session, http.MethodPut, schedPath(rig.sleepable, "schedule/abc"), wakeBody); rec.Code != http.StatusNotFound {
			t.Fatalf("non-numeric order id = %d %s", rec.Code, rec.Body)
		}
		// A listing never shows another deployment's orders.
		rec := rig.do(rig.session, http.MethodGet, schedPath(rig.plain, "schedule"), "")
		if body := decodeBody(t, rec); body["total"] != float64(1) {
			t.Fatalf("plain deployment's list = %s", rec.Body)
		}
	})

	t.Run("a method a route does not take is 405", func(t *testing.T) {
		for _, tc := range []struct{ method, tail string }{
			{http.MethodGet, fmt.Sprintf("schedule/%d", own.ID)},
			{http.MethodPost, fmt.Sprintf("schedule/%d", own.ID)},
			{http.MethodPut, "schedule"},
			{http.MethodDelete, "schedule"},
			{http.MethodGet, "node-replacement"},
			{http.MethodPost, "node-replacement"},
			{http.MethodGet, "wake-waiting"},
			{http.MethodPut, "wake-waiting"},
		} {
			for _, token := range []string{rig.session, rig.mint("read")} {
				// Routing answers before the scope check, for either token.
				want := http.StatusMethodNotAllowed
				if rec := rig.do(token, tc.method, schedPath(rig.sleepable, tc.tail), "{}"); rec.Code != want {
					t.Fatalf("%s %s = %d %s, want %d", tc.method, tc.tail, rec.Code, rec.Body, want)
				}
			}
		}
	})
}
