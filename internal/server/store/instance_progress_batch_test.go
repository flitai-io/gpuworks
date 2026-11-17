package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestInstanceProgressOnly(t *testing.T) {
	base := proto.InstanceStatus{UUID: "i", State: proto.InstPulling, Phase: proto.PhaseImagePull, Message: "pulling", Progress: 0.5, BytesDone: 1, BytesTotal: 2, RateBPS: 3, ContainerID: "c", DirectAddr: "a"}
	if !InstanceProgressOnly(base) {
		t.Fatal("a pull progress report must be progress-only")
	}
	level, paths := 1, []string{"/v1/models"}
	for name, mutate := range map[string]func(*proto.InstanceStatus){
		"no state":            func(st *proto.InstanceStatus) { st.State = "" },
		"running":             func(st *proto.InstanceStatus) { st.State = proto.InstRunning },
		"removing":            func(st *proto.InstanceStatus) { st.State = proto.InstRemoving },
		"draining":            func(st *proto.InstanceStatus) { st.State = proto.InstDraining },
		"stopped":             func(st *proto.InstanceStatus) { st.State = proto.InstStopped },
		"failed":              func(st *proto.InstanceStatus) { st.State = proto.InstFailed },
		"pending removal":     func(st *proto.InstanceStatus) { st.State = proto.InstPendingRemoval },
		"sleep phase":         func(st *proto.InstanceStatus) { st.Phase = proto.PhaseSleep },
		"runtime failure":     func(st *proto.InstanceStatus) { st.RuntimeFailure = &proto.RuntimeFailure{} },
		"ports":               func(st *proto.InstanceStatus) { st.Ports = []int{8000} },
		"measured":            func(st *proto.InstanceStatus) { st.Measured = json.RawMessage(`{}`) },
		"sleep level":         func(st *proto.InstanceStatus) { st.SleepLevel = 1 },
		"sleep target":        func(st *proto.InstanceStatus) { st.SleepTarget = &level },
		"sleep residue":       func(st *proto.InstanceStatus) { st.SleepResidueBytes = 1 },
		"l1 host buffer":      func(st *proto.InstanceStatus) { st.L1HostBufferHeld = true },
		"asset digests":       func(st *proto.InstanceStatus) { st.RuntimeAssetDigests = []string{"sha"} },
		"runtime observation": func(st *proto.InstanceStatus) { st.RuntimeObservation = &proto.InstanceRuntimeObservation{} },
		"openapi paths":       func(st *proto.InstanceStatus) { st.OpenAPIPaths = &paths },
	} {
		st := base
		mutate(&st)
		if InstanceProgressOnly(st) {
			t.Errorf("%s: report carries more than progress but was classified progress-only", name)
		}
	}
}

type progressFixture struct {
	row    Instance
	report proto.InstanceStatus
	// applies says whether the batch takes the report itself.
	applies bool
}

func progressFixtures(node, other uint) map[string]progressFixture {
	pull := func(uuid string) proto.InstanceStatus {
		return proto.InstanceStatus{UUID: uuid, State: proto.InstPulling, Phase: proto.PhaseImagePull, Message: "pulling 40%",
			Progress: 0.4, BytesDone: 40, BytesTotal: 100, RateBPS: 7, ContainerID: "ctr", DirectAddr: "10.0.0.1:1"}
	}
	start := pull("start")
	start.State, start.Phase = proto.InstStarting, proto.PhaseContainerLaunch
	return map[string]progressFixture{
		"same state and phase":                        {Instance{UUID: "same", NodeID: node, State: proto.InstPulling, Phase: proto.PhaseImagePull, Message: "old", BytesDone: 10, RuntimeFailure: `{"code":"x"}`}, pull("same"), true},
		"clears a held host buffer on a starting row": {Instance{UUID: "start", NodeID: node, State: proto.InstStarting, Phase: proto.PhaseContainerLaunch, L1HostBufferHeld: true}, start, true},
		"phase moved on":                              {Instance{UUID: "phase", NodeID: node, State: proto.InstPulling, Phase: proto.PhaseImageVerify}, pull("phase"), false},
		"state moved on":                              {Instance{UUID: "state", NodeID: node, State: proto.InstStarting, Phase: proto.PhaseImagePull}, pull("state"), false},
		"server removing":                             {Instance{UUID: "removing", NodeID: node, State: proto.InstRemoving, Phase: proto.PhaseImagePull}, pull("removing"), false},
		"asleep":                                      {Instance{UUID: "asleep", NodeID: node, State: proto.InstPulling, Phase: proto.PhaseImagePull, SleepLevel: 1}, pull("asleep"), false},
		"another node":                                {Instance{UUID: "foreign", NodeID: other, State: proto.InstPulling, Phase: proto.PhaseImagePull}, pull("foreign"), false},
		"not progress-only": {Instance{UUID: "ports", NodeID: node, State: proto.InstPulling, Phase: proto.PhaseImagePull}, func() proto.InstanceStatus {
			st := pull("ports")
			st.Ports = []int{8000}
			return st
		}(), false},
	}
}

// progressStore opens a file-backed store with two nodes and the fixtures'
// rows, subscribed to instance events before anything is applied.
func progressStore(t *testing.T, name string) (*Store, uint, uint, *bus.Subscription) {
	t.Helper()
	s, err := Open(t.TempDir()+"/"+name+".db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	node, other := Node{UUID: "node", Name: "node"}, Node{UUID: "other", Name: "other"}
	for _, n := range []*Node{&node, &other} {
		if err := s.DB().Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range progressFixtures(node.ID, other.ID) {
		row := f.row
		if _, err := s.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	sub := s.Bus.Subscribe(1024, "instance", "deployment")
	t.Cleanup(sub.Close)
	return s, node.ID, other.ID, sub
}

func progressRow(t *testing.T, s *Store, uuid string) Instance {
	t.Helper()
	row, err := s.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	row.ID, row.CreatedAt, row.UpdatedAt = 0, time.Time{}, time.Time{} // stores assign their own
	return *row
}

func drainEvents(sub *bus.Subscription) []string {
	var got []string
	for {
		select {
		case e := <-sub.C():
			got = append(got, fmt.Sprintf("%s/%s/%s", e.Topic, e.Key, e.Reason))
		case <-time.After(100 * time.Millisecond):
			sort.Strings(got)
			return got
		}
	}
}

func instanceEventCount(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.DB().Model(&InstanceEvent{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// The batch is an optimisation of UpdateInstanceStatusFromNode, never a second
// meaning: a report it takes leaves the row, the timeline and the change
// notifications exactly as the full path would, and every report it does not
// take comes back untouched, in order, for the full path.
func TestApplyInstanceProgressMatchesFullPath(t *testing.T) {
	full, fullNode, _, fullSub := progressStore(t, "full")
	batch, batchNode, batchOther, batchSub := progressStore(t, "batch")
	fixtures := progressFixtures(batchNode, batchOther)

	var reports []proto.InstanceStatus
	names := make([]string, 0, len(fixtures))
	for name := range fixtures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		reports = append(reports, fixtures[name].report)
	}
	unapplied, err := batch.ApplyInstanceProgress(batchNode, reports)
	if err != nil {
		t.Fatal(err)
	}
	var wantUnapplied []proto.InstanceStatus
	for _, name := range names {
		if !fixtures[name].applies {
			wantUnapplied = append(wantUnapplied, fixtures[name].report)
		}
	}
	if !reflect.DeepEqual(unapplied, wantUnapplied) {
		t.Fatalf("unapplied = %+v\nwant %+v", unapplied, wantUnapplied)
	}
	for _, name := range names {
		f := fixtures[name]
		if !f.applies {
			if got, want := progressRow(t, batch, f.row.UUID), progressRow(t, full, f.row.UUID); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: a report the batch refused changed the row:\n got %+v\nwant %+v", name, got, want)
			}
			continue
		}
		if err := full.UpdateInstanceStatusFromNode(fullNode, f.report); err != nil {
			t.Fatalf("%s: full path: %v", name, err)
		}
		if got, want := progressRow(t, batch, f.row.UUID), progressRow(t, full, f.row.UUID); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: batch and full path disagree:\nbatch %+v\n full %+v", name, got, want)
		}
	}
	if got, want := drainEvents(batchSub), drainEvents(fullSub); !reflect.DeepEqual(got, want) {
		t.Errorf("notifications differ:\nbatch %v\n full %v", got, want)
	}
	if got, want := instanceEventCount(t, batch), instanceEventCount(t, full); got != want {
		t.Errorf("timeline rows: batch %d, full path %d", got, want)
	}
}

// A failing write rolls the whole batch back and publishes nothing, so the
// caller's full-path retry starts from the rows as they were.
func TestApplyInstanceProgressRollsBackOnError(t *testing.T) {
	s, node, _, sub := progressStore(t, "rollback")
	before := progressRow(t, s, "same")
	injected := errors.New("injected write failure")
	if err := s.DB().Callback().Update().Before("gorm:update").Register("test:fail_start", func(db *gorm.DB) {
		if updates, ok := db.Statement.Dest.(map[string]any); ok && updates["phase"] == proto.PhaseContainerLaunch {
			_ = db.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	fixtures := progressFixtures(node, 0)
	// "same" applies first, then "start" fails: "same" must roll back too.
	_, err := s.ApplyInstanceProgress(node, []proto.InstanceStatus{fixtures["same state and phase"].report, fixtures["clears a held host buffer on a starting row"].report})
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if got := progressRow(t, s, "same"); !reflect.DeepEqual(got, before) {
		t.Fatalf("a failed batch left a partial write:\n got %+v\nwant %+v", got, before)
	}
	if got := drainEvents(sub); len(got) != 0 {
		t.Fatalf("a failed batch published %v", got)
	}
}

// progressFields are the InstanceStatus fields a progress-only report may
// carry. Every other field must make InstanceProgressOnly false, so a field
// added to the protocol later is never silently batched as progress.
var progressFields = map[string]bool{
	"UUID": true, "State": true, "ContainerID": true, "Message": true, "Phase": true,
	"Progress": true, "BytesDone": true, "BytesTotal": true, "RateBPS": true, "DirectAddr": true,
}

func TestInstanceProgressOnlyRejectsEveryOtherField(t *testing.T) {
	base := proto.InstanceStatus{UUID: "i", State: proto.InstPulling, Phase: proto.PhaseImagePull}
	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if progressFields[field.Name] {
			continue
		}
		st := base
		v := reflect.ValueOf(&st).Elem().Field(i)
		switch v.Kind() {
		case reflect.Pointer:
			v.Set(reflect.New(field.Type.Elem()))
		case reflect.Slice:
			v.Set(reflect.MakeSlice(field.Type, 1, 1))
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Int, reflect.Int64:
			v.SetInt(1)
		case reflect.String:
			v.SetString("x")
		default:
			t.Fatalf("field %s has kind %s: teach this test how to set it", field.Name, v.Kind())
		}
		if InstanceProgressOnly(st) {
			t.Errorf("a report carrying %s was classified progress-only: extend InstanceProgressOnly or progressFields", field.Name)
		}
	}
}

// Once a report of an instance is left for the full path, its later reports
// in the batch are too: the full path runs after the batch, so applying a
// later one here would let the earlier one land last.
func TestApplyInstanceProgressKeepsAnInstanceInOrder(t *testing.T) {
	s, node, _, _ := progressStore(t, "order")
	first := progressFixtures(node, 0)["same state and phase"].report
	extract := first
	extract.Phase, extract.BytesDone = proto.PhaseImageExtract, 50
	back := first
	back.BytesDone = 60
	unapplied, err := s.ApplyInstanceProgress(node, []proto.InstanceStatus{first, extract, back})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unapplied, []proto.InstanceStatus{extract, back}) {
		t.Fatalf("unapplied = %+v, want the phase change and every later report of the instance", unapplied)
	}
	if got := progressRow(t, s, first.UUID).BytesDone; got != first.BytesDone {
		t.Fatalf("bytes_done = %d: a report after one left for the full path was applied first", got)
	}
}

// Every row state a node can report into, against every progress report shape,
// awake or asleep: applying the batch and then the full path for what it
// returns must leave each row, its timeline and the notifications exactly as
// the full path alone. This pins the batch's guard to the full path's rules
// rather than to hand-picked cases.
func TestApplyInstanceProgressMatchesFullPathEverywhere(t *testing.T) {
	rowStates := []string{proto.InstPending, proto.InstPulling, proto.InstStarting, proto.InstRunning, proto.InstRemoving,
		proto.InstDraining, proto.InstStopped, proto.InstFailed, proto.InstPendingRemoval}
	phases := []string{proto.PhaseImagePull, proto.PhaseImageExtract, proto.PhaseSleep}
	reportStates := []string{proto.InstPending, proto.InstPulling, proto.InstStarting}
	type combo struct {
		row    Instance
		report proto.InstanceStatus
	}
	var combos []combo
	for _, rs := range rowStates {
		for _, rp := range phases {
			for _, sleep := range []int{0, 1} {
				for _, st := range reportStates {
					for _, sp := range phases[:2] {
						uuid := fmt.Sprintf("%s.%s.%d.%s.%s", rs, rp, sleep, st, sp)
						combos = append(combos, combo{
							row: Instance{UUID: uuid, State: rs, Phase: rp, SleepLevel: sleep, L1HostBufferHeld: true, Message: "before", BytesDone: 1},
							report: proto.InstanceStatus{UUID: uuid, State: st, Phase: sp, Message: "after", Progress: 0.5,
								BytesDone: 5, BytesTotal: 10, RateBPS: 2, ContainerID: "ctr", DirectAddr: "addr"},
						})
					}
				}
			}
		}
	}
	open := func(name string) (*Store, uint, *bus.Subscription) {
		s, err := Open(t.TempDir()+"/"+name+".db", bus.New())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		node := Node{UUID: "node", Name: "node"}
		if err := s.DB().Create(&node).Error; err != nil {
			t.Fatal(err)
		}
		for _, c := range combos {
			row := c.row
			row.NodeID = node.ID
			if _, err := s.CreateInstance(&row); err != nil {
				t.Fatal(err)
			}
		}
		sub := s.Bus.Subscribe(4096, "instance", "deployment")
		t.Cleanup(sub.Close)
		return s, node.ID, sub
	}
	full, fullNode, fullSub := open("full")
	batch, batchNode, batchSub := open("batch")
	reports := make([]proto.InstanceStatus, 0, len(combos))
	for _, c := range combos {
		reports = append(reports, c.report)
		_ = full.UpdateInstanceStatusFromNode(fullNode, c.report)
	}
	unapplied, err := batch.ApplyInstanceProgress(batchNode, reports)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range unapplied {
		_ = batch.UpdateInstanceStatusFromNode(batchNode, st)
	}
	applied := len(combos) - len(unapplied)
	if applied == 0 || len(unapplied) == 0 {
		t.Fatalf("applied %d of %d: the matrix must exercise both the batch and the fallback", applied, len(combos))
	}
	for _, c := range combos {
		if got, want := progressRow(t, batch, c.row.UUID), progressRow(t, full, c.row.UUID); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: batch then full path disagrees with the full path:\nbatch %+v\n full %+v", c.row.UUID, got, want)
		}
	}
	if got, want := drainEvents(batchSub), drainEvents(fullSub); !reflect.DeepEqual(got, want) {
		t.Errorf("notifications differ: batch %d, full path %d", len(got), len(want))
	}
	var gotEvents, wantEvents []InstanceEvent
	batch.DB().Order("instance_uuid, id").Find(&gotEvents)
	full.DB().Order("instance_uuid, id").Find(&wantEvents)
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("timeline rows: batch %d, full path %d", len(gotEvents), len(wantEvents))
	}
	for i := range gotEvents {
		if gotEvents[i].InstanceUUID != wantEvents[i].InstanceUUID || gotEvents[i].Kind != wantEvents[i].Kind || gotEvents[i].Code != wantEvents[i].Code {
			t.Fatalf("timeline row %d: batch %s/%s, full path %s/%s", i, gotEvents[i].InstanceUUID, gotEvents[i].Kind, wantEvents[i].InstanceUUID, wantEvents[i].Kind)
		}
	}
	t.Logf("%d combinations: %d batched, %d through the full path", len(combos), applied, len(unapplied))
}
