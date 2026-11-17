package monitorapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/monitor"
)

// The overview counts its own services by their wire state, every state in
// the summary's order with zeros kept: a deployment list's
// pending/pulling/sleeping/deleting read as the wire's starting/asleep/
// stopped, so the counts agree with services[].state row for row.
func TestServiceOverviewCountsServicesByState(t *testing.T) {
	inv := monitor.Inventory{Deployments: []monitor.DeploymentRef{
		{ID: 1, Name: "a", State: "running"},
		{ID: 2, Name: "b", State: "running"},
		{ID: 3, Name: "c", State: "pending"},
		{ID: 4, Name: "d", State: "pulling"},
		{ID: 5, Name: "e", State: "sleeping"},
		{ID: 6, Name: "f", State: "failed"},
		{ID: 7, Name: "g", State: "deleting"},
	}}
	svc, _ := newOverviewTestService(t, inv, baseTime)
	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	raw, err := json.Marshal(out.StateCounts)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"state":"running","count":2},{"state":"starting","count":2},{"state":"asleep","count":1},{"state":"offline","count":0},{"state":"failed","count":1},{"state":"stopped","count":1}]`
	if string(raw) != want {
		t.Fatalf("state_counts = %s, want %s", raw, want)
	}
	perState := map[string]int{}
	for _, row := range out.Services {
		perState[row.State]++
	}
	for _, c := range out.StateCounts {
		if perState[c.State] != c.Count {
			t.Fatalf("state_counts %s = %d, services[] has %d", c.State, c.Count, perState[c.State])
		}
	}
}

// An empty fleet still lists every state, each at zero, never null.
func TestServiceOverviewStateCountsOfAnEmptyFleet(t *testing.T) {
	svc, _ := newOverviewTestService(t, monitor.Inventory{}, baseTime)
	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	const want = `"state_counts":[{"state":"running","count":0},{"state":"starting","count":0},{"state":"asleep","count":0},{"state":"offline","count":0},{"state":"failed","count":0},{"state":"stopped","count":0}]`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("overview = %s, want %s", raw, want)
	}
}

// Every state a deployment can show (deployment.States) maps onto one of the
// overview's serviceStates, so state_counts never drops a service and "All N"
// always equals the number of rows. A new deployment state that
// deploymentState passes through unmapped fails here.
func TestDeploymentStateStaysWithinServiceStates(t *testing.T) {
	known := make(map[string]bool, len(serviceStates))
	for _, state := range serviceStates {
		known[state] = true
	}
	var rows []dto.MonitorServiceRow
	for _, state := range deployment.States {
		got := deploymentState(monitor.DeploymentRef{State: state})
		if !known[got] {
			t.Errorf("deployment state %q reads as %q, which serviceStates does not list", state, got)
		}
		rows = append(rows, dto.MonitorServiceRow{State: got})
	}
	total := 0
	for _, count := range stateCountsDTO(rows) {
		total += count.Count
	}
	if total != len(rows) {
		t.Fatalf("state_counts add up to %d, want one per row (%d)", total, len(rows))
	}
}
