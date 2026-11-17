package store

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestUpdateInstanceStatusPersistsStartupProgressAndEventData(t *testing.T) {
	st, err := Open("file:store_instance_progress_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("progress-node", HashToken("progress-token"), "progress")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "progress-inst", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{
		UUID: inst.UUID, State: proto.InstPulling, ContainerID: "gpuworks-progress", Message: "layer 2/4",
		Phase: "image_pull", Progress: 0.5, BytesDone: 512, BytesTotal: 1024, RateBPS: 128,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "image_pull" || got.Progress != 0.5 || got.BytesDone != 512 || got.BytesTotal != 1024 || got.RateBPS != 128 {
		t.Fatalf("progress was not persisted: %+v", got)
	}
	events, err := st.InstanceEvents(inst.ID, 0)
	if err != nil || len(events) == 0 {
		t.Fatalf("events = %+v, err=%v", events, err)
	}
	if events[len(events)-1].Code != instanceEventStatusUpdated || events[len(events)-1].Message != "" {
		t.Fatalf("event did not use stable code: %+v", events[len(events)-1])
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(events[len(events)-1].Data), &data); err != nil {
		t.Fatalf("event data = %q: %v", events[len(events)-1].Data, err)
	}
	if data["phase"] != "image_pull" || data["bytes_done"] != float64(512) {
		t.Fatalf("event progress data = %+v", data)
	}
	if data["state"] != proto.InstPulling || data["detail"] != "layer 2/4" {
		t.Fatalf("event localization params = %+v", data)
	}
}

func TestUpdateInstanceStatusDoesNotOverwriteTerminalControllerState(t *testing.T) {
	for _, terminal := range []string{proto.InstFailed, proto.InstStopped, "pending-removal"} {
		t.Run(terminal, func(t *testing.T) {
			st := newTestStore(t)
			node, err := st.RegisterNode("terminal-node-"+terminal, HashToken("terminal-token-"+terminal), "terminal")
			if err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
			if err != nil {
				t.Fatal(err)
			}
			inst, err := st.CreateInstance(&Instance{
				UUID: "terminal-inst-" + terminal, DeploymentID: dep.ID, NodeID: node.ID,
				State: proto.InstRunning, ContainerID: "preserved-container", Message: "running",
			})
			if err != nil {
				t.Fatal(err)
			}
			setTerminal := st.SetInstanceState(inst.UUID, terminal, "controller terminal evidence")
			if terminal == proto.InstFailed {
				setTerminal = st.SetInstanceFailed(inst.UUID, proto.RuntimeFailure{Code: proto.ErrorServingPathUnavailable}, "controller terminal evidence")
			}
			if err := setTerminal; err != nil {
				t.Fatal(err)
			}

			// This is the ordering observed in the hosted distributed live case:
			// teardown completes after the controller has retained a failed row.
			if err := st.UpdateInstanceStatus(proto.InstanceStatus{
				UUID: inst.UUID, State: proto.InstStopped, Message: "late agent teardown",
			}); err != nil {
				t.Fatal(err)
			}
			got, err := st.InstanceByUUID(inst.UUID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != terminal || got.Message != "controller terminal evidence" || got.ContainerID != "preserved-container" {
				t.Fatalf("late agent report overwrote terminal row: %+v", got)
			}
		})
	}
}

func TestDeletingDeploymentAcceptsLateTeardownAckFromFailedInstance(t *testing.T) {
	st := newTestStore(t)
	node, err := st.RegisterNode("late-teardown-node", HashToken("late-teardown-token"), "late-teardown")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: DeployDeleting})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "late-teardown-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstFailed})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.InstStopped {
		t.Fatalf("late teardown acknowledgement was ignored: %+v", got)
	}
}

// A high-frequency status stream is progress, not history: the columns move,
// the timeline does not. This is the behaviour the whole change exists for.
func TestUpdateInstanceStatusWritesEventsOnlyForStateAndPhaseChanges(t *testing.T) {
	st := newTestStore(t)
	inst := newProgressTestInstance(t, st, "milestone-inst")

	report := func(state, phase, message string, done, total int64) {
		t.Helper()
		fraction := float64(0)
		if total > 0 {
			fraction = float64(done) / float64(total)
		}
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{
			UUID: inst.UUID, State: state, Phase: phase, Message: message,
			BytesDone: done, BytesTotal: total, Progress: fraction,
		}); err != nil {
			t.Fatal(err)
		}
	}

	report(proto.InstPulling, "image_pull", "layer 1/40 0 B/1 GiB", 0, 1<<30)
	// 200 pull samples: different message and byte counters every time, which
	// is exactly what used to append 200 rows.
	for i := 1; i <= 200; i++ {
		done := int64(i) * (1 << 20)
		report(proto.InstPulling, "image_pull", "layer 1/40 "+strconv.FormatInt(done, 10)+" B/1 GiB", done, 1<<30)
	}
	report(proto.InstPulling, "image_extract", "extracting", 0, 0)
	report(proto.InstStarting, "container_start", "starting container", 0, 0)
	for i := 0; i < 50; i++ {
		report(proto.InstStarting, "container_start", "starting container", 0, 0)
	}
	report(proto.InstRunning, "", "", 0, 0)

	events, err := st.InstanceEvents(inst.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// One row per (state, phase) the instance passed through, plus the creation
	// row the instance was born with.
	var codes []string
	for _, e := range events {
		codes = append(codes, e.Code)
	}
	if len(events) != 5 {
		t.Fatalf("wrote %d events for 254 status reports (%v), want one per state/phase change plus creation", len(events), codes)
	}
	got, err := st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.InstRunning || got.Phase != "" {
		t.Fatalf("final row = state %q phase %q", got.State, got.Phase)
	}
}

func TestUpdateInstanceStatusSummarizesTheEndedPhase(t *testing.T) {
	st := newTestStore(t)
	inst := newProgressTestInstance(t, st, "summary-inst")

	if err := st.UpdateInstanceStatus(proto.InstanceStatus{
		UUID: inst.UUID, State: proto.InstPulling, Phase: "image_pull", Message: "pulling",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{
		UUID: inst.UUID, State: proto.InstPulling, Phase: "image_pull", Message: "pulling",
		BytesDone: 900 << 20, BytesTotal: 1 << 30, Progress: 0.87,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{
		UUID: inst.UUID, State: proto.InstStarting, Phase: "container_start", Message: "starting container",
	}); err != nil {
		t.Fatal(err)
	}

	events, err := st.InstanceEvents(inst.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	var data map[string]any
	if err := json.Unmarshal([]byte(last.Data), &data); err != nil {
		t.Fatalf("event data = %q: %v", last.Data, err)
	}
	if data["state"] != proto.InstStarting || data["phase"] != "container_start" {
		t.Fatalf("event describes the wrong milestone: %+v", data)
	}
	// The row that ends image_pull carries what that phase achieved: the final
	// byte counters, which are no longer visible anywhere else once the columns
	// move on to the next phase.
	if data["prev_phase"] != "image_pull" || data["prev_state"] != proto.InstPulling {
		t.Fatalf("phase summary identifies the wrong phase: %+v", data)
	}
	if data["prev_bytes_done"] != float64(900<<20) || data["prev_bytes_total"] != float64(1<<30) {
		t.Fatalf("phase summary byte counters = %+v", data)
	}
	if _, ok := data["prev_duration_ms"]; !ok {
		t.Fatalf("phase summary carries no duration: %+v", data)
	}
	if duration, _ := data["prev_duration_ms"].(float64); duration < 0 {
		t.Fatalf("phase summary duration = %v", duration)
	}
}

// The controller must not be woken by a byte counter. A milestone still names
// the state it reached, so everything that reconciles on state keeps working.
func TestUpdateInstanceStatusPublishesProgressAndMilestonesDistinctly(t *testing.T) {
	b := bus.New()
	st, err := Open("file:store_instance_progress_reason_test?mode=memory&cache=shared", b)
	if err != nil {
		t.Fatal(err)
	}
	inst := newProgressTestInstance(t, st, "reason-inst")
	sub := b.Subscribe(64, "instance")
	defer sub.Close()

	for _, step := range []proto.InstanceStatus{
		{UUID: inst.UUID, State: proto.InstPulling, Phase: "image_pull", Message: "pulling"},
		{UUID: inst.UUID, State: proto.InstPulling, Phase: "image_pull", Message: "pulling more", BytesDone: 10},
		{UUID: inst.UUID, State: proto.InstPulling, Phase: "image_extract", Message: "extracting"},
		{UUID: inst.UUID, State: proto.InstRunning},
	} {
		if err := st.UpdateInstanceStatus(step); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{proto.InstPulling, bus.ReasonProgress, proto.InstPulling, proto.InstRunning}
	for i, wantReason := range want {
		select {
		case ev := <-sub.C():
			if ev.Reason != wantReason {
				t.Fatalf("event %d reason = %q, want %q", i, ev.Reason, wantReason)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %d was never published", i)
		}
	}
}

func TestUpdateInstanceStatusPublishesSleepTransitionsAsRoutingEvents(t *testing.T) {
	b := bus.New()
	st, err := Open("file:store_instance_sleep_reason_test?mode=memory&cache=shared", b)
	if err != nil {
		t.Fatal(err)
	}
	inst := newProgressTestInstance(t, st, "sleep-reason-inst")
	sub := b.Subscribe(8, "instance")
	defer sub.Close()
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	<-sub.C()
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, SleepLevel: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-sub.C():
		if ev.Reason != "sleep" {
			t.Fatalf("sleep event reason=%q, want sleep", ev.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sleep event was not published")
	}
}

func newProgressTestInstance(t *testing.T, st *Store, uuid string) *Instance {
	t.Helper()
	node, err := st.RegisterNode(uuid+"-node", HashToken(uuid+"-token"), uuid)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}
