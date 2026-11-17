package engine

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// waitGone waits until uuid is neither desired nor being reconciled.
func waitGone(t *testing.T, e *Engine, uuid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		_, desired := e.desired[uuid]
		busy := e.inFlight[uuid]
		e.mu.Unlock()
		if !desired && !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s was not torn down", uuid)
}

// What the agent knows about an instance's engine process - its sleep level,
// that it holds an L1 host buffer, that its sleep was prewarmed, the processes
// it keeps on its cards - belongs to that process. When the server removes or
// stops the container, a container started later under the same UUID is a new
// process that holds none of it, and is reported so (a process replaced
// without the server's order is TestAReplacedEngineProcessIsPutToSleepAgain): a stale "holds its L1 host buffer" lets the server skip
// the host memory check of the next L1 sleep.
func TestANewContainerUnderTheSameUUIDKnowsNothingOfTheOldProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		gone func(t *testing.T, e *Engine, log *statusLog, spec proto.InstanceDesiredState)
	}{
		{"removed by the server", func(t *testing.T, e *Engine, _ *statusLog, spec proto.InstanceDesiredState) {
			e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
			waitGone(t, e, spec.UUID)
		}},
		{"stopped by the server", func(t *testing.T, e *Engine, log *statusLog, spec proto.InstanceDesiredState) {
			stopped := spec
			stopped.DesiredState, stopped.DesiredSleep = "stopped", 0
			before := len(log.states(proto.InstStopped))
			e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{stopped})
			log.waitFor(t, proto.InstStopped, before+1)
			waitIdle(t, e, spec.UUID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			port := healthyEngine(t) // the engine answers that it is awake
			// The first process starts awake (and is prewarmed), then sleeps L1.
			spec := runningSpec("process-facts", port)
			spec.Sleepable = true
			serve(t, e, log, spec)
			waitIdle(t, e, spec.UUID)
			e.mu.Lock()
			warmed := e.processOfLocked(spec.UUID).prewarmed
			e.mu.Unlock()
			if !warmed {
				t.Fatal("fixture: the first process was not prewarmed")
			}
			spec.DesiredSleep = 1
			e.ApplyUpdate(context.Background(), []proto.InstanceDesiredState{spec}, nil)
			deadline := time.Now().Add(5 * time.Second)
			for !e.holdsL1HostBuffer(spec.UUID) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			waitIdle(t, e, spec.UUID)
			if !e.holdsL1HostBuffer(spec.UUID) {
				t.Fatal("fixture: the first process did not take its L1 sleep")
			}

			tc.gone(t, e, log, spec)
			if e.holdsL1HostBuffer(spec.UUID) {
				t.Error("the old process's L1 host buffer is remembered after its container is gone")
			}

			// A new process that does not sleep holds no L1 host buffer.
			plain := runningSpec("process-facts", port)
			serve(t, e, log, plain)
			waitIdle(t, e, plain.UUID)
			running := log.states(proto.InstRunning)
			if last := running[len(running)-1]; last.L1HostBufferHeld {
				t.Error("a new process that never slept is reported holding the old process's L1 host buffer")
			}

			// A new sleepable process is prewarmed again (the prewarm's own L1
			// sleep then really does pin its host buffer).
			tc.gone(t, e, log, plain)
			again := runningSpec("process-facts", port)
			again.Sleepable = true
			before := len(log.states(proto.InstOptimizing))
			serve(t, e, log, again)
			waitIdle(t, e, again.UUID)
			if after := len(log.states(proto.InstOptimizing)); after == before {
				t.Error("the new process was not prewarmed: the old process's prewarm was taken for it")
			}
		})
	}
}
