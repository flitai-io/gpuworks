package scheduler

import (
	"reflect"
	"testing"
)

// held is a card an extension holds out of scheduling, as the planner
// snapshots it (store.GPU.HeldByHolder): neither schedulable nor shareable.
func held(g GPU) GPU {
	g.Schedulable, g.ForeignShareable, g.HeldByHolder = false, false, true
	return g
}

// A held card takes no work: whole-card requests go to the free card beside
// it, and a node whose only free cards are held is refused with the hold,
// naming the cards on their node.
func TestAHeldCardTakesNoWork(t *testing.T) {
	whole := automatic(0)
	whole.PerGPULimitBytes = 0
	res := Schedule(whole, nvidiaNode(held(freeH20(0)), freeH20(1)))
	if !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{1}) {
		t.Fatalf("beside a held card = ok %v %+v plan %+v, want GPU 1", res.OK, res.Message, res.Plan)
	}
	res = Schedule(whole, nvidiaNode(held(freeH20(0)), held(freeH20(1))))
	if res.OK || res.Message.Code != ReasonGPUsHeld {
		t.Fatalf("all held = ok %v %+v, want %s", res.OK, res.Message, ReasonGPUsHeld)
	}
	if p := res.Message.Params; p["count"] != 2 || !reflect.DeepEqual(p["gpus"], []int{0, 1}) || p["node"] != "h20" {
		t.Fatalf("params = %+v", p)
	}
}

// A partition consents to sharing cards it chose with processes outside
// the platform, but not to a hold: a held card that is also used outside
// the platform is refused with the hold, which enabling cannot lift.
func TestAHoldIsNotLiftedByChoosingTheCard(t *testing.T) {
	req := reporterPartition(16) // names card 0 and consents to sharing it
	res := Schedule(req, nvidiaNode(held(reporterCard(0))))
	if res.OK || res.Message.Code != ReasonGPUsHeld {
		t.Fatalf("a chosen held card = ok %v %+v, want %s", res.OK, res.Message, ReasonGPUsHeld)
	}
	items := GPUCandidates(req, nvidiaNode(held(reporterCard(0)), freeH20(1)))
	if len(items) != 2 || items[0].Available || items[0].Reason == nil || items[0].Reason.Code != ReasonGPUsHeld || !items[1].Available {
		t.Fatalf("candidates = %+v", items)
	}
}
