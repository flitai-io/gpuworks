package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/lifecycle"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// cardsWakeFixture is a node of two 24 GiB cards, a sharing deployment
// ("auto", anytime) whose replicas sleep on the given cards, and a
// higher-ranked deployment ("chat", idle) whose replica waits to be placed on
// card 1, behind the real adapter.
func cardsWakeFixture(t *testing.T, replicas ...string) (*automationAdapter, *store.Store, *store.Deployment, *placewait.Registry) {
	t.Helper()
	st, err := store.Open("file:automation_cards_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("cards-node", store.HashToken("token"), "cards-node")
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"gpu-0", "gpu-1"} {
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: i, UUID: uuid, Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/cards", Name: "cards", WeightBytes: 2 << 30, Dtype: "bfloat16"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, policy string, replicas int) *store.Deployment {
		dep, err := st.CreateDeployment(&store.Deployment{Name: name, ModelID: model.ID, NodeID: node.ID, ServedName: name, Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: replicas})
		if err != nil {
			t.Fatal(err)
		}
		p, _ := yieldpolicy.Parse(policy, yieldpolicy.Overrides{})
		if err := st.SetDeploymentYieldPolicy(dep.ID, p); err != nil {
			t.Fatal(err)
		}
		return dep
	}
	dep := mk("auto", yieldpolicy.Anytime, len(replicas))
	chat := mk("chat", yieldpolicy.Idle, 1)
	var uuids []string
	for i, gpus := range replicas {
		uuid := "auto-" + string(rune('1'+i))
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: gpus, NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
		uuids = append(uuids, uuid)
	}
	// Every replica sleeps by the platform's choice (a yield): not held.
	if err := st.OrderInstancesSleep(dep.ID, uuids, 1, ""); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range uuids {
		if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: uuid, State: proto.InstRunning, SleepLevel: 1}); err != nil {
			t.Fatal(err)
		}
	}
	waits := placewait.New()
	waits.Wait(chat.ID, placewait.Cards{NodeID: node.ID, GPUIndexes: []int{1}}, nil, time.Now())
	adapter := &automationAdapter{store: st, now: time.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil)), waits: waits, orders: &lifecycle.Orders{}}
	return adapter, st, dep, waits
}

// A sleeper on cards 0 and 1 (TP=2) does not wake while a higher-ranked
// replica waits to be placed on card 1: the replica stays asleep whole and
// waits for room, and wakes once the placement is done.
func TestAWakeLeavesAReplicaSpanningAPlacementsCardAsleepWhole(t *testing.T) {
	adapter, st, dep, waits := cardsWakeFixture(t, "[0,1]")
	result, err := adapter.Wake(dep.ID, true)
	if err != nil || len(result.Woken) != 0 || !slices.Equal(result.Waiting, []string{"auto-1"}) {
		t.Fatalf("wake = %+v err=%v, want auto-1 waiting", result, err)
	}
	if intentOf(t, st, "auto-1") != 1 || mustInstance(t, st, "auto-1").WakeWaitingSince == nil {
		t.Fatal("auto-1 is not asleep and waiting for room")
	}
	if mustInstance(t, st, "auto-1").SleepHeld {
		t.Fatal("a wake that waits left the sleep held")
	}
	// Still the placement's card: the waiting wake leaves it.
	if woken, err := adapter.WakeWaiting(dep.ID, []string{"auto-1"}); err != nil || len(woken) != 0 {
		t.Fatalf("waiting wake onto the placement's card = %v err=%v", woken, err)
	}
	waits.Done(waits.Waiting(time.Now())[0].DeploymentID)
	if woken, err := adapter.WakeWaiting(dep.ID, []string{"auto-1"}); err != nil || !slices.Equal(woken, []string{"auto-1"}) {
		t.Fatalf("waiting wake once the placement is done = %v err=%v", woken, err)
	}
	if intentOf(t, st, "auto-1") != 0 {
		t.Fatal("auto-1 was not woken")
	}
}

// A two-replica sleeper with one replica on the placement's card: the other
// wakes now, that one waits for room.
func TestAWakeWakesTheReplicaOffAPlacementsCard(t *testing.T) {
	adapter, st, dep, _ := cardsWakeFixture(t, "[0]", "[1]")
	result, err := adapter.Wake(dep.ID, true)
	if err != nil || !slices.Equal(result.Woken, []string{"auto-1"}) || !slices.Equal(result.Waiting, []string{"auto-2"}) {
		t.Fatalf("wake = %+v err=%v, want auto-1 woken and auto-2 waiting", result, err)
	}
	if intentOf(t, st, "auto-1") != 0 || intentOf(t, st, "auto-2") != 1 {
		t.Fatalf("intents = %d, %d", intentOf(t, st, "auto-1"), intentOf(t, st, "auto-2"))
	}
	if mustInstance(t, st, "auto-1").WakeWaitingSince != nil || mustInstance(t, st, "auto-2").WakeWaitingSince == nil {
		t.Fatal("the wait is marked on the wrong replica")
	}
}
