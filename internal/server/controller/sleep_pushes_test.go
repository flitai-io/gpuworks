package controller

import "testing"

// What was pushed is read back as a copy, recorded per instance, replaced
// whole by a pass, and dropped by deployment.
func TestSleepPushesByDeployment(t *testing.T) {
	var p sleepPushes
	if p.last(1) != nil {
		t.Fatal("an empty memory has pushes")
	}
	p.record(1, "a", sleepOrder{level: 1, mode: "l1"})
	got := p.last(1)
	got["b"] = sleepOrder{level: 2}
	if len(p.last(1)) != 1 || p.last(1)["a"] != (sleepOrder{level: 1, mode: "l1"}) {
		t.Fatalf("last is not a copy of what was recorded: %v", p.last(1))
	}
	p.set(1, map[string]sleepOrder{"c": {level: 2}})
	if pushed := p.last(1); len(pushed) != 1 || pushed["c"].level != 2 {
		t.Fatalf("set did not replace the deployment's pushes: %v", pushed)
	}
	p.record(2, "d", sleepOrder{})
	p.forget(1)
	if p.last(1) != nil || p.last(2) == nil {
		t.Fatal("forget dropped the wrong deployment")
	}
}
