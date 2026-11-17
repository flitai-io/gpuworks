package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/scheduler"
)

var (
	poolWideConcession = scheduler.Concession{Code: "pool_wide", PoolWide: true}
	localConcession    = scheduler.Concession{Code: "local"}
)

// A plan that gives up nothing worth looking elsewhere for is taken at once:
// placement stays first-fit in rank order.
func TestFitPickerTakesAPlanWithoutPoolWideConcessionsAtOnce(t *testing.T) {
	var picker FitPicker[string]
	if !picker.Offer("a", nil) {
		t.Fatal("a plan with no concessions was not taken")
	}
	var local FitPicker[string]
	if !local.Offer("a", []scheduler.Concession{localConcession}) {
		t.Fatal("a plan with only a local concession was not taken: first fit must not change for it")
	}
}

// A pool-wide concession keeps the search going; the best fallback is the
// one with the fewest, and the earlier offer wins a tie.
func TestFitPickerKeepsTheFewestPoolWideConcessionsAsFallback(t *testing.T) {
	var picker FitPicker[string]
	if _, ok := picker.Fallback(); ok {
		t.Fatal("an empty picker offered a fallback")
	}
	if picker.Offer("two", []scheduler.Concession{poolWideConcession, poolWideConcession}) {
		t.Fatal("a plan with pool-wide concessions was taken at once")
	}
	if picker.Offer("one", []scheduler.Concession{poolWideConcession, localConcession}) {
		t.Fatal("a plan with a pool-wide concession was taken at once")
	}
	picker.Offer("one-later", []scheduler.Concession{poolWideConcession})
	if got, ok := picker.Fallback(); !ok || got != "one" {
		t.Fatalf("fallback = %q %v, want the earliest with the fewest pool-wide concessions", got, ok)
	}
	if !picker.Offer("clean", nil) {
		t.Fatal("a clean plan after fallbacks was not taken")
	}
}
