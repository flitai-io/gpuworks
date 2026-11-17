package engine

import (
	"context"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Every Engine field is either per-instance state - kept by instance UUID -
// or said here not to be. A new field fails this test until it is classified,
// and a per-instance one must say whether the server removing the instance
// drops it: TestRemovingAnInstanceLeavesNothingUnderItsUUID then checks that
// it really does (ADR 0066). The target is one record per lifetime; the
// per-instance maps still separate are listed so the next change can fold
// them in.
func TestEveryEngineFieldIsClassified(t *testing.T) {
	seen := map[string]bool{}
	ty := reflect.TypeOf(Engine{})
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		seen[name] = true
		_, per := perInstanceEngineFields[name]
		if !per && notPerInstanceEngineFields[name] == "" {
			t.Errorf("Engine.%s is not classified: if it is kept by instance, put it in the instance's record for the lifetime it shares (processFacts for the engine process) and add it to perInstanceEngineFields; otherwise say why in notPerInstanceEngineFields", name)
		}
		byUUID := ty.Field(i).Type.Kind() == reflect.Map && ty.Field(i).Type.Key().Kind() == reflect.String
		if per && !byUUID {
			t.Errorf("Engine.%s is listed per instance but is not a map keyed by instance UUID", name)
		}
		if !per && byUUID {
			t.Errorf("Engine.%s is a map keyed by string, so it is per-instance state: list it in perInstanceEngineFields", name)
		}
	}
	for name := range perInstanceEngineFields {
		if !seen[name] {
			t.Errorf("perInstanceEngineFields lists %s, which Engine no longer has", name)
		}
	}
	for name := range notPerInstanceEngineFields {
		if !seen[name] {
			t.Errorf("notPerInstanceEngineFields lists %s, which Engine no longer has", name)
		}
	}
}

// perInstanceEngineFields: true when the server removing the instance drops
// the UUID's entry.
var perInstanceEngineFields = map[string]bool{
	// The engine process: starts empty where a container is created, dropped
	// where it is confirmed gone and on a runtime switch.
	"processes": true,
	// The desired generation and its reconcile attempt.
	"desired":         true,
	"inFlight":        true,
	"inFlightCancel":  true,
	"pending":         true,
	"cleanupRequired": true,
	// The attempt that reported the instance running; a stopped instance
	// keeps it, unread because only a desired-running instance is watched.
	"servingEpoch": true,
	"healthProbes": true,
	// Kept for the UUID's life in this agent on purpose: a removal moves it
	// to a fresh global epoch, so a reused UUID never matches an old attempt.
	"runtimeEpoch": false,
}

var notPerInstanceEngineFields = map[string]string{
	"driver": "a dependency", "hostRuntimeFacts": "a dependency", "prober": "a dependency",
	"report": "a dependency", "log": "a dependency", "runtimeMounts": "a dependency",
	"extractLibvgpu": "a dependency", "checkPorts": "a dependency",
	"captureObservationConnection": "a dependency", "reportRuntimeObservation": "a dependency",
	"networkInterfaceName": "a dependency", "networkConfig": "a dependency", "logDrain": "a dependency",
	"puller": "a dependency", "otherRuntimes": "a dependency", "cardGate": "a dependency",
	"owner":    "the agent's own identity, labelled on every container it launches",
	"stateDir": "configuration", "hostRoot": "configuration", "residueSettle": "configuration",
	"headlessProbeInterval": "configuration", "headlessStableChecks": "configuration",
	"livenessInterval": "configuration", "healthProbeAttempts": "configuration",
	"healthProbeInterval": "configuration", "healthProbeTimeout": "configuration",
	"mu":               "guards the per-instance state",
	"assetMu":          "serializes content-addressed cache publication",
	"partitionMu":      "serializes per-card virtual-id allocation",
	"desiredKnown":     "whether the server has sent the full desired set once",
	"nextRuntimeEpoch": "the global epoch counter",
}

// Removing an instance leaves nothing under its UUID in any per-instance
// field, except the ones classified as kept on purpose. The instance first
// slept at L1, so its process record, desired generation, epochs and serving
// attempt all hold an entry the removal must take away.
func TestRemovingAnInstanceLeavesNothingUnderItsUUID(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("forgotten", healthyEngine(t))
	spec.Sleepable = true
	serve(t, e, log, spec)
	waitIdle(t, e, spec.UUID)
	spec.DesiredSleep = 1
	e.ApplyUpdate(context.Background(), []proto.InstanceDesiredState{spec}, nil)
	deadline := time.Now().Add(5 * time.Second)
	for !e.holdsL1HostBuffer(spec.UUID) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	waitIdle(t, e, spec.UUID)

	held := func() map[string]bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		v := reflect.ValueOf(e).Elem()
		out := map[string]bool{}
		for name := range perInstanceEngineFields {
			if v.FieldByName(name).MapIndex(reflect.ValueOf(spec.UUID)).IsValid() {
				out[name] = true
			}
		}
		return out
	}
	before := held()
	for _, name := range []string{"processes", "desired", "runtimeEpoch", "servingEpoch"} {
		if !before[name] {
			t.Fatalf("fixture: %s holds nothing for the instance before its removal (held: %v)", name, before)
		}
	}

	e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
	waitGone(t, e, spec.UUID)
	for name := range held() {
		if perInstanceEngineFields[name] {
			t.Errorf("Engine.%s still holds the removed instance", name)
		}
	}
}
