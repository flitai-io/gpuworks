package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type scheduleRig struct {
	*tokenRig
	sleepable, plain, multi, multiAuto, pd, pdManual *store.Deployment
}

func newScheduleRig(t *testing.T) *scheduleRig {
	t.Helper()
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	create := func(dep *store.Deployment) *store.Deployment {
		dep.State = store.DeployActive
		created, err := rig.st.CreateDeployment(dep)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	return &scheduleRig{
		tokenRig:  rig,
		sleepable: create(&store.Deployment{Name: "sleepy", ServedName: "sleepy", Engine: "vllm", Sleepable: true, Replicas: 1}),
		plain:     create(&store.Deployment{Name: "plain", ServedName: "plain", Engine: "vllm", Replicas: 1}),
		multi:     create(&store.Deployment{Name: "multi", ServedName: "multi", Engine: "vllm", Sleepable: true, NNodes: 2, Replicas: 1}),
		multiAuto: create(&store.Deployment{Name: "multi-auto", ServedName: "multi-auto", Engine: "vllm", Sleepable: true, NNodes: 2, Replicas: 1, UnitsAuto: true}),
		pd: create(&store.Deployment{Name: "pd", ServedName: "pd", Engine: "vllm", Sleepable: true, Replicas: 1, UnitsAuto: true,
			Placement: `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"n1"}]},"decode":{"units":[{"node_uuid":"n2"}]}}}}`}),
		pdManual: create(&store.Deployment{Name: "pd-manual", ServedName: "pd-manual", Engine: "vllm", Sleepable: true, Replicas: 1,
			Placement: `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"n1"}]},"decode":{"units":[{"node_uuid":"n2"}]}}}}`}),
	}
}

const sleepOrder = `{"action":"sleep","weekdays":[1,2,3,4,5],"time":"21:00","timezone":"Asia/Shanghai","sleep_level":2}`

func TestScheduleCRUDWire(t *testing.T) {
	rig := newScheduleRig(t)
	path := schedPath(rig.sleepable, "schedule")

	rec := rig.do(rig.session, http.MethodGet, path, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[],"total":0}` {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body)
	}

	rec = rig.do(rig.session, http.MethodPost, path, sleepOrder)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created := decodeBody(t, rec)
	keys := make([]string, 0, len(created))
	for k := range created {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if got, want := strings.Join(keys, ","), "action,enabled,id,next_at,sleep_level,time,timezone,weekdays"; got != want {
		t.Fatalf("created keys = %s, want %s (replicas is omitted when 0): %s", got, want, rec.Body)
	}
	id, _ := created["id"].(float64)
	if id < 1 || created["action"] != "sleep" || created["time"] != "21:00" || created["timezone"] != "Asia/Shanghai" || created["sleep_level"] != float64(2) || created["enabled"] != true {
		t.Fatalf("created = %v", created)
	}
	if days, _ := json.Marshal(created["weekdays"]); string(days) != "[1,2,3,4,5]" {
		t.Fatalf("weekdays = %s", days)
	}
	next, ok := created["next_at"].(string)
	if !ok {
		t.Fatalf("an enabled order has next_at: %v", created)
	}
	at, err := time.Parse(time.RFC3339, next)
	if err != nil || !at.After(time.Now().Add(-time.Minute)) || at.After(time.Now().AddDate(0, 0, 8)) || !strings.HasSuffix(next, "Z") {
		t.Fatalf("next_at = %q (%v), want a UTC time within the coming week", next, err)
	}
	// 21:00 in Shanghai on a weekday is 13:00 UTC, whichever weekday it lands on.
	if at.UTC().Hour() != 13 || at.UTC().Minute() != 0 {
		t.Fatalf("next_at = %s, want 13:00Z (21:00 Asia/Shanghai)", next)
	}
	if wd := at.In(time.FixedZone("CST", 8*3600)).Weekday(); wd == time.Saturday || wd == time.Sunday {
		t.Fatalf("next_at falls on weekday %v", wd)
	}

	rec = rig.do(rig.session, http.MethodGet, path, "")
	var list struct {
		Items []dto.LifecycleSchedule `json:"items"`
		Total int                     `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || list.Total != 1 || len(list.Items) != 1 || list.Items[0].ID != uint(id) || list.Items[0].NextAt == nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if stored, err := rig.st.LifecycleSchedule(rig.sleepable.ID, uint(id)); err != nil || stored.Weekdays != 0b0111110 || stored.Minute != 21*60 || stored.SleepLevel != 2 || !stored.Enabled {
		t.Fatalf("stored = %+v %v (weekdays are a bitmask: Mon..Fri = 0b0111110)", stored, err)
	}

	// Update replaces the whole order and keeps its id; a disabled order has
	// no next_at.
	sid := fmt.Sprintf("schedule/%d", uint(id))
	rec = rig.do(rig.session, http.MethodPut, schedPath(rig.sleepable, sid), `{"action":"wake","weekdays":[6,0],"time":"07:05","timezone":"UTC","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}
	if want := fmt.Sprintf(`{"id":%d,"action":"wake","weekdays":[0,6],"time":"07:05","timezone":"UTC","enabled":false}`, uint(id)); strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("update body\n got %s\nwant %s", rec.Body, want)
	}
	if stored, err := rig.st.LifecycleSchedule(rig.sleepable.ID, uint(id)); err != nil || stored.Action != "wake" || stored.SleepLevel != 0 || stored.Enabled {
		t.Fatalf("stored after update = %+v %v", stored, err)
	}

	if rec = rig.do(rig.session, http.MethodDelete, schedPath(rig.sleepable, sid), ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body)
	}
	rec = rig.do(rig.session, http.MethodGet, path, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[],"total":0}` {
		t.Fatalf("list after delete = %d %s", rec.Code, rec.Body)
	}
	if rec = rig.do(rig.session, http.MethodDelete, schedPath(rig.sleepable, sid), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d %s", rec.Code, rec.Body)
	}
}

func TestScheduleReplicasOrderOnASingleNodeDeployment(t *testing.T) {
	rig := newScheduleRig(t)
	// A replicas order needs no sleep capability.
	rec := rig.do(rig.session, http.MethodPost, schedPath(rig.plain, "schedule"), `{"action":"replicas","weekdays":[3],"time":"00:00","replicas":32,"enabled":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["replicas"] != float64(32) || body["timezone"] != "" || body["time"] != "00:00" {
		t.Fatalf("body = %v", body)
	}
	if _, has := body["sleep_level"]; has {
		t.Fatalf("a replicas order has no sleep_level: %v", body)
	}
	rec = rig.do(rig.session, http.MethodPost, schedPath(rig.plain, "schedule"), `{"action":"replicas","weekdays":[3],"time":"00:00","replicas":1}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("replicas 1 = %d %s", rec.Code, rec.Body)
	}
	// Enabled defaults to true when absent.
	if decodeBody(t, rec)["enabled"] != true {
		t.Fatalf("enabled did not default to true: %s", rec.Body)
	}
}

// A stored order shows the next run the automation will make: in its zone,
// and none for an order in a zone the server lost since, which the automation
// skips (deployment.AutomationSchedule).
func TestAStoredOrderShowsTheAutomationsNextRun(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	row := store.LifecycleSchedule{ID: 4, DeploymentID: 9, Action: store.ScheduleActionSleep, Weekdays: 1<<7 - 1, Minute: 60, Timezone: "Asia/Shanghai", Enabled: true}
	if out := scheduleDTO(row, now); out.NextAt == nil || !out.NextAt.Equal(time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("next run = %v, want 01:00 Shanghai", out.NextAt)
	}
	row.Timezone = "Nowhere/Never"
	if out := scheduleDTO(row, now); out.NextAt != nil {
		t.Fatalf("an order the automation skips shows a next run at %v", out.NextAt)
	}
}

func TestScheduleValidation(t *testing.T) {
	rig := newScheduleRig(t)
	type refusal struct {
		name, body string
		dep        *store.Deployment
		status     int
		code       string
		field      string
	}
	base := func(extra string) string {
		return `{"action":"sleep","weekdays":[1],"time":"21:00","timezone":"UTC","sleep_level":2` + extra + `}`
	}
	cases := []refusal{
		{name: "empty weekdays", body: `{"action":"wake","weekdays":[],"time":"21:00"}`, field: "weekdays"},
		{name: "missing weekdays", body: `{"action":"wake","time":"21:00"}`, field: "weekdays"},
		{name: "weekday 7", body: `{"action":"wake","weekdays":[7],"time":"21:00"}`, field: "weekdays"},
		{name: "negative weekday", body: `{"action":"wake","weekdays":[-1],"time":"21:00"}`, field: "weekdays"},
		{name: "duplicate weekday", body: `{"action":"wake","weekdays":[2,2],"time":"21:00"}`, field: "weekdays"},
		{name: "time without leading zero", body: `{"action":"wake","weekdays":[1],"time":"9:00"}`, field: "time"},
		{name: "time 24:00", body: `{"action":"wake","weekdays":[1],"time":"24:00"}`, field: "time"},
		{name: "minute 60", body: `{"action":"wake","weekdays":[1],"time":"12:60"}`, field: "time"},
		{name: "time empty", body: `{"action":"wake","weekdays":[1],"time":""}`, field: "time"},
		{name: "time with seconds", body: `{"action":"wake","weekdays":[1],"time":"09:00:00"}`, field: "time"},
		{name: "unknown action", body: `{"action":"stop","weekdays":[1],"time":"21:00"}`, field: "action"},
		{name: "missing action", body: `{"weekdays":[1],"time":"21:00"}`, field: "action"},
		{name: "sleep without level", body: `{"action":"sleep","weekdays":[1],"time":"21:00"}`, field: "sleep_level"},
		{name: "sleep level 0", body: `{"action":"sleep","weekdays":[1],"time":"21:00","sleep_level":0}`, field: "sleep_level"},
		{name: "sleep level 3", body: `{"action":"sleep","weekdays":[1],"time":"21:00","sleep_level":3}`, field: "sleep_level"},
		{name: "wake with level", body: `{"action":"wake","weekdays":[1],"time":"21:00","sleep_level":1}`, field: "sleep_level"},
		{name: "replicas with level", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2,"sleep_level":1}`, dep: rig.plain, field: "sleep_level"},
		{name: "sleep with replicas", body: base(`,"replicas":2`), field: "replicas"},
		{name: "wake with replicas", body: `{"action":"wake","weekdays":[1],"time":"21:00","replicas":2}`, field: "replicas"},
		{name: "replicas without count", body: `{"action":"replicas","weekdays":[1],"time":"21:00"}`, dep: rig.plain, field: "replicas"},
		{name: "replicas 0", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":0}`, dep: rig.plain, field: "replicas"},
		{name: "replicas 1025", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":1025}`, dep: rig.plain, field: "replicas"},
		{name: "replicas negative", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":-1}`, dep: rig.plain, field: "replicas"},
		{name: "unknown time zone", body: `{"action":"wake","weekdays":[1],"time":"21:00","timezone":"Mars/Base"}`, status: 400, code: "SCHEDULE_TIMEZONE_UNKNOWN"},
		{name: "replicas of a multi-node deployment placed by hand", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2}`, dep: rig.multi, status: 400, code: "SCHEDULE_REPLICAS_MANUAL_PLACEMENT"},
		{name: "replicas of a PD deployment without a role", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2}`, dep: rig.pd, field: "role"},
		{name: "replicas of a PD deployment for an unknown role", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2,"role":"encode"}`, dep: rig.pd, field: "role"},
		{name: "a role of a deployment that is not PD", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2,"role":"decode"}`, dep: rig.plain, field: "role"},
		{name: "a role of a multi-node deployment", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2,"role":"decode"}`, dep: rig.multiAuto, field: "role"},
		{name: "a role on a sleep order", body: base(`,"role":"decode"`), dep: rig.pd, field: "role"},
		{name: "a role on a wake order", body: `{"action":"wake","weekdays":[1],"time":"21:00","role":"decode"}`, field: "role"},
		{name: "decode to 0 replicas", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":0,"role":"decode"}`, dep: rig.pd, field: "replicas"},
		{name: "prefill over the limit", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":33,"role":"prefill"}`, dep: rig.pd, field: "replicas"},
		{name: "replicas of a PD deployment placed by hand", body: `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2,"role":"decode"}`, dep: rig.pdManual, status: 400, code: "SCHEDULE_REPLICAS_MANUAL_PLACEMENT"},
		{name: "sleep on a deployment without sleep mode", body: base(``), dep: rig.plain, status: 400, code: "SLEEP_NOT_ENABLED"},
		{name: "wake on a deployment without sleep mode", body: `{"action":"wake","weekdays":[1],"time":"21:00"}`, dep: rig.plain, status: 400, code: "SLEEP_NOT_ENABLED"},
		{name: "unknown JSON field", body: `{"action":"wake","weekdays":[1],"time":"21:00","force":true}`, status: 400},
		{name: "response-only field", body: `{"action":"wake","weekdays":[1],"time":"21:00","next_at":"2026-01-01T00:00:00Z"}`, status: 400},
		{name: "malformed JSON", body: `{"action":`, status: 400},
		{name: "empty body", body: ``, status: 400},
		{name: "weekdays not a list", body: `{"action":"wake","weekdays":"mon","time":"21:00"}`, status: 400},
		{name: "enabled not a bool", body: `{"action":"wake","weekdays":[1],"time":"21:00","enabled":"yes"}`, status: 400},
	}
	// A stored order per deployment for the PUT variant of every case.
	orders := map[uint]uint{}
	orderOf := func(dep *store.Deployment) uint {
		if id, ok := orders[dep.ID]; ok {
			return id
		}
		row := &store.LifecycleSchedule{DeploymentID: dep.ID, Action: store.ScheduleActionWake, Weekdays: 1 << 2, Minute: 6 * 60, Enabled: true}
		if err := rig.st.CreateLifecycleSchedule(row); err != nil {
			t.Fatal(err)
		}
		orders[dep.ID] = row.ID
		return row.ID
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := tc.dep
			if dep == nil {
				dep = rig.sleepable
			}
			wantStatus := tc.status
			if wantStatus == 0 {
				wantStatus = http.StatusBadRequest
			}
			wantCode := tc.code
			if tc.field != "" {
				wantCode = "SCHEDULE_INVALID"
			}
			for _, method := range []string{http.MethodPost, http.MethodPut} {
				path := schedPath(dep, "schedule")
				if method == http.MethodPut {
					// The same validation guards an update of a real order.
					path = schedPath(dep, fmt.Sprintf("schedule/%d", orderOf(dep)))
				}
				rec := rig.do(rig.session, method, path, tc.body)
				if rec.Code != wantStatus {
					t.Fatalf("%s: status = %d %s, want %d", method, rec.Code, rec.Body, wantStatus)
				}
				if wantCode == "" {
					continue
				}
				body := decodeBody(t, rec)
				if body["code"] != wantCode {
					t.Fatalf("%s: code = %v (%s), want %s", method, body["code"], rec.Body, wantCode)
				}
				params, _ := body["params"].(map[string]any)
				if tc.field != "" && params["field"] != tc.field {
					t.Fatalf("%s: params.field = %v (%s), want %s", method, params["field"], rec.Body, tc.field)
				}
				if wantCode == "SCHEDULE_TIMEZONE_UNKNOWN" && params["timezone"] != "Mars/Base" {
					t.Fatalf("%s: params = %v", method, params)
				}
				if wantCode == "SLEEP_NOT_ENABLED" && params["engine"] != "vllm" {
					t.Fatalf("%s: params = %v", method, params)
				}
			}
		})
	}
	// None of the refusals stored an order.
	for _, dep := range []*store.Deployment{rig.sleepable, rig.plain, rig.multi, rig.pd, rig.pdManual} {
		rows, err := rig.st.LifecycleSchedules(dep.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Action != "wake" || row.Minute != 6*60 {
				t.Fatalf("deployment %s: a refused request changed or stored an order: %+v", dep.Name, row)
			}
		}
	}
}

// A multi-node deployment the platform places takes a replica-count order, as
// a single-node one does: its new replicas get their nodes when they grow.
func TestScheduleReplicasOfAnAutomaticallyPlacedMultiNodeDeployment(t *testing.T) {
	rig := newScheduleRig(t)
	path := schedPath(rig.multiAuto, "schedule")
	rec := rig.do(rig.session, http.MethodPost, path, `{"action":"replicas","weekdays":[1],"time":"21:00","timezone":"UTC","replicas":3}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if body := decodeBody(t, rec); body["action"] != "replicas" || body["replicas"] != float64(3) {
		t.Fatalf("created = %v", body)
	}
	// The refusal for a deployment placed by hand names the cause in its
	// own code, so a client can tell the operator to switch placement.
	rec = rig.do(rig.session, http.MethodPost, schedPath(rig.multi, "schedule"), `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2}`)
	if body := decodeBody(t, rec); rec.Code != http.StatusBadRequest || body["code"] != "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" {
		t.Fatalf("manual placement = %d %s", rec.Code, rec.Body)
	}
}

// A PD deployment's replica count is per role: the order names the role, the
// answer says it, and the mode's minimum bounds the count (prefill may be
// scheduled down to 0, decode may not).
func TestScheduleReplicasOfAPDRole(t *testing.T) {
	rig := newScheduleRig(t)
	path := schedPath(rig.pd, "schedule")

	rec := rig.do(rig.session, http.MethodPost, path, `{"action":"replicas","weekdays":[1],"time":"21:00","timezone":"UTC","replicas":3,"role":"decode","enabled":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created dto.LifecycleSchedule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{"id":%d,"action":"replicas","weekdays":[1],"time":"21:00","timezone":"UTC","replicas":3,"role":"decode","enabled":false}`, created.ID); strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("create body\n got %s\nwant %s", rec.Body, want)
	}
	if stored, err := rig.st.LifecycleSchedule(rig.pd.ID, created.ID); err != nil || stored.Role != "decode" || stored.Replicas != 3 {
		t.Fatalf("stored = %+v %v", stored, err)
	}

	// Prefill to 0 is a count, not an absent one: the answer states it.
	rec = rig.do(rig.session, http.MethodPost, path, `{"action":"replicas","weekdays":[1],"time":"22:00","timezone":"UTC","replicas":0,"role":"prefill","enabled":false}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"replicas":0,"role":"prefill"`) {
		t.Fatalf("prefill to 0 = %d %s", rec.Code, rec.Body)
	}
	var zero dto.LifecycleSchedule
	_ = json.Unmarshal(rec.Body.Bytes(), &zero)

	rec = rig.do(rig.session, http.MethodGet, path, "")
	var list struct {
		Items []dto.LifecycleSchedule `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 2 ||
		list.Items[0].Role != "decode" || list.Items[0].Replicas == nil || *list.Items[0].Replicas != 3 ||
		list.Items[1].Role != "prefill" || list.Items[1].Replicas == nil || *list.Items[1].Replicas != 0 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}

	// An update restates the order whole, role included: another role now,
	// and none leaves it refused.
	sid := fmt.Sprintf("schedule/%d", created.ID)
	rec = rig.do(rig.session, http.MethodPut, schedPath(rig.pd, sid), `{"action":"replicas","weekdays":[1],"time":"21:00","timezone":"UTC","replicas":2,"role":"prefill","enabled":false}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replicas":2,"role":"prefill"`) {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}
	if stored, _ := rig.st.LifecycleSchedule(rig.pd.ID, created.ID); stored.Role != "prefill" || stored.Replicas != 2 {
		t.Fatalf("stored after update = %+v", stored)
	}
	rec = rig.do(rig.session, http.MethodPut, schedPath(rig.pd, sid), `{"action":"wake","weekdays":[1],"time":"21:00"}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "role") || strings.Contains(rec.Body.String(), "replicas") {
		t.Fatalf("turned into a wake = %d %s", rec.Code, rec.Body)
	}
	if stored, _ := rig.st.LifecycleSchedule(rig.pd.ID, created.ID); stored.Role != "" || stored.Replicas != 0 {
		t.Fatalf("stored after becoming a wake = %+v: the role and count go with the replicas order", stored)
	}

	// A sleep order carries no role in its answer.
	rec = rig.do(rig.session, http.MethodPost, schedPath(rig.sleepable, "schedule"), sleepOrder)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), `"role"`) || strings.Contains(rec.Body.String(), `"replicas"`) {
		t.Fatalf("sleep order = %d %s", rec.Code, rec.Body)
	}

	// The refusals name the field, and the role the modes allow.
	rec = rig.do(rig.session, http.MethodPost, path, `{"action":"replicas","weekdays":[1],"time":"21:00","replicas":2,"role":"encode"}`)
	body := decodeBody(t, rec)
	params, _ := body["params"].(map[string]any)
	roles, _ := json.Marshal(params["roles"])
	if rec.Code != http.StatusBadRequest || body["code"] != "SCHEDULE_INVALID" || params["field"] != "role" || string(roles) != `["decode","prefill"]` {
		t.Fatalf("unknown role = %d %s", rec.Code, rec.Body)
	}
}
