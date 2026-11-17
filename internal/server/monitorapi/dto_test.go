package monitorapi

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

var zeroTime = time.Time{}

func TestDeploymentPlacementCombinesTPAndNodeNames(t *testing.T) {
	dep := monitor.DeploymentRef{}
	instances := []monitor.InstanceRef{
		{NodeUUID: "n1", GPUIndexes: []int{0, 1}},
		{NodeUUID: "n2", GPUIndexes: []int{0, 1, 2, 3}},
	}
	nodeNames := map[string]string{"n1": "gpu3", "n2": "gpu4"}
	if got := deploymentPlacement(dep, instances, nodeNames); got != "TP4 · gpu3, gpu4" {
		t.Fatalf("placement = %q", got)
	}
}

func TestDeploymentPlacementNoInstancesIsEmpty(t *testing.T) {
	if got := deploymentPlacement(monitor.DeploymentRef{}, nil, nil); got != "" {
		t.Fatalf("placement = %q, want empty for a deployment with no replicas yet", got)
	}
}

func TestDeploymentPlacementFallsBackToUUIDWhenNodeUnnamed(t *testing.T) {
	instances := []monitor.InstanceRef{{NodeUUID: "node-uuid-1", GPUIndexes: []int{0}}}
	if got := deploymentPlacement(monitor.DeploymentRef{}, instances, map[string]string{}); got != "TP1 · node-uuid-1" {
		t.Fatalf("placement = %q", got)
	}
}

// sleeper is a running instance whose engine reports a sleep level: asleep by
// the level and by the gateway's judgement alike.
func sleeper(level int) monitor.InstanceRef {
	return monitor.InstanceRef{State: proto.InstRunning, SleepLevel: level, Asleep: true}
}

// The monitor shows the list's state in its own vocabulary and derives
// nothing: every state the list shows maps to exactly one.
func TestDeploymentStateMapsTheListState(t *testing.T) {
	for list, want := range map[string]string{
		"pending": "starting", "pulling": "starting", "starting": "starting",
		"running": "running", "offline": "offline", "failed": "failed",
		"stopped": "stopped", "deleting": "stopped",
		"sleeping": "asleep", "asleep": "asleep",
		"": "stopped",
	} {
		if got := deploymentState(monitor.DeploymentRef{State: list}); got != want {
			t.Errorf("deploymentState(%q) = %q, want %q", list, got, want)
		}
	}
}

func TestReplicaStatePerInstance(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst monitor.InstanceRef
		want string
	}{
		{"failed", monitor.InstanceRef{State: proto.InstFailed}, "failed"},
		{"asleep", sleeper(2), "asleep"},
		{"ordered to sleep, not carried out yet", monitor.InstanceRef{State: proto.InstRunning, Asleep: true}, "asleep"},
		{"running", monitor.InstanceRef{State: proto.InstRunning}, "running"},
		{"starting", monitor.InstanceRef{State: proto.InstOptimizing}, "starting"},
		{"stopped", monitor.InstanceRef{State: proto.InstStopped}, "stopped"},
		{"unknown state falls back to stopped", monitor.InstanceRef{State: "weird"}, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replicaState(tc.inst); got != tc.want {
				t.Fatalf("replicaState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCountReadyReplicasExcludesHeadless(t *testing.T) {
	instances := []monitor.InstanceRef{
		{State: proto.InstRunning},
		{State: proto.InstRunning, Headless: true},
		{State: proto.InstFailed},
	}
	if got := monitor.ReadyReplicas(monitor.DeploymentRef{}, instances); got != 1 {
		t.Fatalf("countReadyReplicas = %d, want 1 (the headless follower must not count)", got)
	}
}

func TestCollectedFloatPtr(t *testing.T) {
	if collectedFloatPtr(false, 42) != nil {
		t.Fatal("collectedFloatPtr(false, ...) must be nil")
	}
	got := collectedFloatPtr(true, 42)
	if got == nil || *got != 42 {
		t.Fatalf("collectedFloatPtr(true, 42) = %v", got)
	}
}

func TestTimeMillisZeroTime(t *testing.T) {
	if got := timeMillis(zeroTime); got != 0 {
		t.Fatalf("timeMillis(zero) = %d, want 0", got)
	}
	at := baseTime
	if got := timeMillis(at); got != at.UnixMilli() {
		t.Fatalf("timeMillis = %d", got)
	}
}

func TestLastPointsTailsLongSeries(t *testing.T) {
	pts := make([]tsdb.Point, 20)
	for i := range pts {
		pts[i] = tsdb.Point{TimeMS: int64(i), Value: float64(i)}
	}
	got := lastPoints(pts, sparkPoints)
	if len(got) != sparkPoints {
		t.Fatalf("lastPoints returned %d points, want %d", len(got), sparkPoints)
	}
	if got[0][0] != float64(20-sparkPoints) || got[len(got)-1][0] != 19 {
		t.Fatalf("lastPoints = %+v, want the trailing %d timestamps", got, sparkPoints)
	}
}

func TestLastPointsShorterThanNReturnsAll(t *testing.T) {
	pts := []tsdb.Point{{TimeMS: 0, Value: 1}, {TimeMS: 1, Value: 2}}
	got := lastPoints(pts, sparkPoints)
	if len(got) != 2 {
		t.Fatalf("lastPoints = %+v, want both points unchanged", got)
	}
}
