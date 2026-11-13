package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNodeReplicasExposeDrainSchedulingRefusal(t *testing.T) {
	h, st, deps := schedulingFixture(t)
	if _, err := st.SetNodeScheduling("drain-a", store.NodeDraining); err != nil {
		t.Fatal(err)
	}
	st.AddDeploymentEventCode(deps["pool"].ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{"scheduler": map[string]any{"code": "no_free_gpu", "params": map[string]any{"node": "drain-b"}}})
	status, body := callNode(t, h.nodeReplicas, http.MethodGet, "drain-a", "/replicas", "")
	var wire struct {
		Items []struct {
			Instance string              `json:"instance"`
			Reason   *dto.ScheduleReason `json:"drain_reason"`
		}
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &wire) != nil {
		t.Fatalf("status=%d body=%s", status, body)
	}
	for _, item := range wire.Items {
		if item.Instance == "pool-1" {
			if item.Reason == nil || item.Reason.Code != "no_free_gpu" || item.Reason.Params["node"] != "drain-b" {
				t.Fatalf("drain lost actual scheduler refusal: %s", body)
			}
			return
		}
	}
	t.Fatal("running pool replica disappeared")
}

func TestNodeDrainFeedbackTracksFailedAttemptAndClearsAfterNewAttempt(t *testing.T) {
	h, st, deps := schedulingFixture(t)
	if _, err := st.SetNodeScheduling("drain-a", store.NodeDraining); err != nil {
		t.Fatal(err)
	}
	source, err := st.InstanceByUUID("pool-1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.NodeByUUID("drain-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "replacement-failed", DeploymentID: deps["pool"].ID, NodeID: other.ID, State: proto.InstFailed, Message: "image authorization denied", CreatedAt: source.CreatedAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	read := func() dto.NodeReplica {
		t.Helper()
		status, body := callNode(t, h.nodeReplicas, http.MethodGet, "drain-a", "/replicas", "")
		var out dto.NodeReplicas
		if status != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("status=%d body=%s", status, body)
		}
		for _, item := range out.Items {
			if item.Instance == "pool-1" {
				return item
			}
		}
		t.Fatal("source disappeared")
		return dto.NodeReplica{}
	}
	if got := read().DrainMessage; got != "image authorization denied" {
		t.Fatalf("failed attempt hidden: %q", got)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "replacement-new", DeploymentID: deps["pool"].ID, NodeID: other.ID, State: proto.InstStarting, CreatedAt: source.CreatedAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if got := read().DrainMessage; got != "" {
		t.Fatalf("superseded failure shown as latest: %q", got)
	}
}

func TestNodeDrainFeedbackIncludesFailedReplacementFollower(t *testing.T) {
	h, st, deps := schedulingFixture(t)
	if _, err := st.SetNodeScheduling("drain-a", store.NodeDraining); err != nil {
		t.Fatal(err)
	}
	other, _ := st.NodeByUUID("drain-b")
	for _, row := range []store.Instance{
		{UUID: "new-follower", Headless: true, NodeRank: 1, State: proto.InstFailed, Message: "collective peer could not start"},
		{UUID: "new-leader", State: proto.InstStarting},
	} {
		row.DeploymentID, row.NodeID, row.NNodes, row.DistGroup, row.DistAttempt = deps["multi"].ID, other.ID, 2, "g-multi", 2
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	status, body := callNode(t, h.nodeReplicas, http.MethodGet, "drain-a", "/replicas", "")
	var out dto.NodeReplicas
	if status != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
		t.Fatal(body)
	}
	for _, row := range out.Items {
		if row.Instance == "multi-follower" && row.DrainMessage == "collective peer could not start" {
			return
		}
	}
	t.Fatalf("failed rank hidden by healthy leader: %s", body)
}

func TestNodeReplicasRefreshOfflineCauseWhileRetiring(t *testing.T) {
	h, st, deps := schedulingFixture(t)
	if _, err := st.SetNodeScheduling("drain-a", store.NodeDraining); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionInstances(deps["pool"].ID, []string{"pool-1"}, []string{proto.InstRunning}, proto.InstDraining, ""); err != nil {
		t.Fatal(err)
	}
	online := false
	h.d.Online = func(string) bool { return online }
	for _, connected := range []bool{false, true} {
		online = connected
		status, body := callNode(t, h.nodeReplicas, http.MethodGet, "drain-a", "/replicas", "")
		var out dto.NodeReplicas
		if status != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatal(body)
		}
		if out.DrainOffline == connected {
			t.Fatalf("offline cause stale after online=%v: %s", connected, body)
		}
		found := false
		for _, row := range out.Items {
			if row.Instance == "pool-1" && row.State == proto.InstDraining {
				found = true
			}
		}
		if !found {
			t.Fatalf("retiring instance disappeared: %s", body)
		}
	}
}
