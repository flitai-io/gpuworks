package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// sharingRow is the row of one deployment in a timeline answer, as it is on
// the wire, and whether there is one.
func sharingRow(t *testing.T, body string, id uint) (map[string]any, bool) {
	t.Helper()
	var got struct {
		Deployments []map[string]any `json:"deployments"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	for _, row := range got.Deployments {
		if uint(row["deployment_id"].(float64)) == id {
			return row, true
		}
	}
	return nil, false
}

// addGoneDeployment adds a deployment whose instance held card 0 from t0 and
// was deleted at gone, as DeleteDeployment leaves it: its events and the
// removal kept, its name in the deleted deployments.
func (r *timelineRig) addGoneDeployment(name string, gone time.Duration, yieldsTo []time.Duration, to uint) uint {
	r.t.Helper()
	dep, err := r.st.CreateDeployment(presetDeployment(&store.Deployment{ModelID: 1, NodeID: r.node.ID, Name: name, ServedName: name, Engine: "vllm", Sleepable: true, State: store.DeployActive}, yieldpolicy.Anytime))
	if err != nil {
		r.t.Fatal(err)
	}
	uuid := "inst-" + name
	if err := r.st.DB().Create(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: r.node.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: r.t0}).Error; err != nil {
		r.t.Fatal(err)
	}
	r.event(uuid, dep.ID, "state", "INSTANCE_CREATED", 0, `{"state":"pending"}`)
	r.event(uuid, dep.ID, "state", "INSTANCE_STATUS_UPDATED", 5*time.Minute, `{"state":"running","phase":""}`)
	for _, at := range yieldsTo {
		r.event("", dep.ID, store.EventKindAutomation, "AUTO_SLEEP", at, fmt.Sprintf(`{"trigger":"preempt","reason":"rank","for_deployment":%d,"level":1,"instances":[%q]}`, to, uuid))
	}
	if err := r.st.DeleteDeployment(dep.ID); err != nil {
		r.t.Fatal(err)
	}
	// It went at gone, not as the test ran.
	when := r.t0.Add(gone)
	if err := r.st.DB().Model(&store.DeletedDeployment{}).Where("id = ?", dep.ID).Update("deleted_at", when).Error; err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.DB().Model(&store.InstanceEvent{}).Where("instance_uuid = ? AND code = ?", uuid, store.EventInstanceRemoved).Update("created_at", when).Error; err != nil {
		r.t.Fatal(err)
	}
	return dep.ID
}

// A deployment deleted since it yielded: the yields count toward the one it
// made room for in the fleet's stats as in the node's, and it is listed in
// both, marked deleted, over the part of the window it existed in. A deleted
// deployment that took part in no yield is not listed.
func TestSharingStatsCountADeletedDeploymentsYields(t *testing.T) {
	r := newTimelineRig(t)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 0, `{"state":"running","phase":""}`)
	gone := r.addGoneDeployment("old-batch", 2*time.Hour, []time.Duration{30 * time.Minute, time.Hour}, r.shared.ID)
	quiet := r.addGoneDeployment("quiet", time.Hour, nil, 0)
	// One yield by a deployment that still exists, to compare with.
	r.st.AddDeploymentEventCode(r.plain.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "turn", "for_deployment": r.shared.ID, "instances": []string{"inst-plain"}})

	views := map[string]string{"fleet": "?sharing=1&hours=6", "node": "?node=" + r.node.UUID + "&hours=6"}
	rows := map[string][2]map[string]any{}
	asked := time.Now()
	for view, query := range views {
		status, body := r.get(query)
		if status != http.StatusOK {
			t.Fatalf("%s = %d %s", view, status, body)
		}
		shared, ok := sharingRow(t, body, r.shared.ID)
		if !ok {
			t.Fatalf("%s lists no row for the shared deployment: %s", view, body)
		}
		if shared["yielded_to"] != 3.0 {
			t.Errorf("%s: yielded_to = %v, want 3 (two by the deleted deployment, one by an existing one)", view, shared["yielded_to"])
		}
		deleted, ok := sharingRow(t, body, gone)
		if !ok {
			t.Fatalf("%s lists no row for the deleted deployment: %s", view, body)
		}
		if _, ok := sharingRow(t, body, quiet); ok {
			t.Errorf("%s lists a deleted deployment that took part in no yield", view)
		}
		// The deleted one on the wire: its name, marked deleted, no policy;
		// present from its instance's creation to its deletion, awake from
		// +5 min on.
		wire := body[strings.Index(body, `"deployments":[`):]
		wire = wire[strings.Index(wire, fmt.Sprintf(`{"deployment_id":%d,`, gone)):]
		wire = wire[:strings.Index(wire, "}")+1]
		if want := fmt.Sprintf(`{"deployment_id":%d,"deployment":"old-batch","deployment_deleted":true,"yield_policy":"","awake_seconds":6900,"awake_share":0.9583333333333334,"present_seconds":7200,"switches":2,"yielded":2,"yielded_to":0,"wakes":0,"held_requests":0,"cards":[]}`, gone); wire != want {
			t.Errorf("%s: deleted row\n got  %s\n want %s", view, wire, want)
		}
		rows[view] = [2]map[string]any{shared, deleted}
	}
	// Every instance is on the one node, so the two views agree on every
	// switch count and on the time present. Each request reads the clock for
	// itself, in whole seconds, so what is still present when asked differs
	// by the seconds that went by between the two requests, at most.
	slack := float64(int(time.Since(asked)/time.Second) + 1)
	for i, name := range []string{"shared", "deleted"} {
		for _, key := range []string{"switches", "yielded", "yielded_to", "wakes"} {
			if f, n := rows["fleet"][i][key], rows["node"][i][key]; f != n {
				t.Errorf("%s %s: fleet %v, node %v", name, key, f, n)
			}
		}
		if f, n := rows["fleet"][i]["present_seconds"].(float64), rows["node"][i]["present_seconds"].(float64); f > n+slack || n > f+slack {
			t.Errorf("%s present_seconds: fleet %v, node %v, more than %v s apart", name, f, n, slack)
		}
	}
	// An existing deployment's row says nothing about deletion.
	if _, ok := rows["fleet"][0]["deployment_deleted"]; ok {
		t.Error("an existing deployment is marked deleted")
	}
}

// A deployment created inside the window and awake since is awake for nearly
// all the time it existed, not for the fraction of the window that is. The
// fleet counts its presence from the deployment's creation; a node from when
// its instance there first held cards.
func TestSharingStatsShareIsOfTheTimeADeploymentExisted(t *testing.T) {
	r := newTimelineRig(t)
	born := r.t0.Add(-16 * time.Hour) // 19 h before now, 5 h into a 24 h window
	if err := r.st.DB().Model(&store.Deployment{}).Where("id = ?", r.shared.ID).Update("created_at", born).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.st.DB().Model(&store.Instance{}).Where("uuid = ?", "inst-shared").Update("created_at", born.Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_CREATED", -16*time.Hour+time.Minute, `{"state":"pending"}`)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", -16*time.Hour+2*time.Minute, `{"state":"running","phase":""}`)
	for view, query := range map[string]string{"fleet": "?sharing=1&hours=24", "node": "?node=" + r.node.UUID + "&hours=24"} {
		status, body := r.get(query)
		var got dto.SchedulingTimeline
		if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || len(got.Deployments) != 1 {
			t.Fatalf("%s = %d %s", view, status, body)
		}
		d := got.Deployments[0]
		// Present from the deployment's creation (before its instance's), or
		// on the node from its instance's.
		present := int64(got.Window.To.Sub(born).Seconds())
		if view == "node" {
			present = int64(got.Window.To.Sub(born.Add(time.Minute)).Seconds())
		}
		awake := int64(got.Window.To.Sub(born.Add(2 * time.Minute)).Seconds())
		if d.PresentSeconds != present || d.AwakeSeconds != awake {
			t.Fatalf("%s: present %d awake %d, want %d and %d", view, d.PresentSeconds, d.AwakeSeconds, present, awake)
		}
		if want := float64(awake) / float64(present); d.AwakeShare != want || d.AwakeShare < 0.99 || got.Scope != view {
			t.Fatalf("%s: awake share = %v, want %v (not %v, the share of the window)", view, d.AwakeShare, want, float64(awake)/(24*3600))
		}
	}
}

func TestTimelineMarksDeletedNonSharingDeployment(t *testing.T) {
	r := newTimelineRig(t)
	gone := r.addGoneDeployment("quiet-deleted", time.Hour, nil, 0)
	status, body := r.get("?node=" + r.node.UUID + "&hours=6")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if _, ok := sharingRow(t, body, gone); ok {
		t.Fatal("quiet deployment unexpectedly in sharing stats")
	}
	var got struct {
		Nodes []struct {
			GPUs []struct {
				Segments []map[string]any `json:"segments"`
			} `json:"gpus"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, node := range got.Nodes {
		for _, gpu := range node.GPUs {
			for _, segment := range gpu.Segments {
				if uint(segment["deployment_id"].(float64)) == gone {
					count++
					if segment["deployment_deleted"] != true || segment["deployment"] != "quiet-deleted" {
						t.Errorf("deleted segment = %v", segment)
					}
				} else if _, ok := segment["deployment_deleted"]; ok {
					t.Errorf("live segment carries deletion flag: %v", segment)
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("no history for deleted deployment")
	}
}
