package controller

import (
	"testing"
	"time"
)

// A refusal is remembered for poolRefusalMemory and a wait for
// poolSettleBackoff, each by deployment; settling ends the wait and
// forgetting drops both.
func TestPoolMemoryWindows(t *testing.T) {
	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	var m poolMemory
	if m.refusedRecently(1, 9, start) || m.waitingRecently(1, start) {
		t.Fatal("an empty memory remembers something")
	}
	m.refuse(1, 9, start)
	m.wait(1, start)
	m.refuse(2, 9, start)
	m.wait(2, start)
	if !m.refusedRecently(1, 9, start.Add(poolRefusalMemory-time.Nanosecond)) || m.refusedRecently(1, 8, start) {
		t.Fatal("the refusal is not remembered for its node alone")
	}
	if m.refusedRecently(1, 9, start.Add(poolRefusalMemory)) || m.refusals(1) != 0 {
		t.Fatalf("an expired refusal is still remembered (%d)", m.refusals(1))
	}
	if !m.refusedRecently(2, 9, start) {
		t.Fatal("dropping deployment 1's expired refusal dropped deployment 2's")
	}
	if !m.waitingRecently(1, start.Add(poolSettleBackoff-time.Nanosecond)) || m.waitingRecently(1, start.Add(poolSettleBackoff)) {
		t.Fatal("the wait does not last exactly poolSettleBackoff")
	}
	m.wait(1, start)
	m.settled(1)
	if m.waitingRecently(1, start) || !m.waitingRecently(2, start) {
		t.Fatal("settling deployment 1 did not end its wait alone")
	}
	m.forget(2)
	if m.refusals(2) != 0 || m.waitingRecently(2, start) {
		t.Fatal("a forgotten deployment is still remembered")
	}
}
