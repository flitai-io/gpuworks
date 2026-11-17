package kube

import (
	"context"
	"sync"
	"time"
)

// The cluster entry's idle-return grace period and NVIDIA RuntimeClass are
// not part of the DaemonSet manifest: the server hands them to a running
// agent (proto.KubeSettings) when it connects and whenever the entry is
// edited, and ApplySettings makes them current without a restart. What the
// manifest or the enrollment gave the agent at start (the
// GPUWORKS_KUBE_RUNTIME_CLASS of a manifest from before, the idle return
// enrollment returned) is only the value used until the first message.

// runtimeClassState is the RuntimeClass NVIDIA instance pods are started
// with, read by Run and replaced by ApplySettings, so it has a lock of its own.
type runtimeClassState struct {
	mu    sync.RWMutex
	class string
	// pushed is set once the server has given the class; Detect's own lookup
	// never replaces it.
	pushed bool
}

func (s *runtimeClassState) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.class
}

// initial sets the class Options or detection came up with, unless the
// server's has been applied already (or, for detection, one is set).
func (s *runtimeClassState) initial(class string, onlyIfEmpty bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pushed || (onlyIfEmpty && s.class != "") {
		return
	}
	s.class = class
}

func (s *runtimeClassState) push(class string) (changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed = s.class != class
	s.class, s.pushed = class, true
	return changed
}

// settingsLookupTimeout bounds the API read of the cluster's RuntimeClass
// that an empty setting asks for.
const settingsLookupTimeout = 10 * time.Second

// ApplySettings makes the entry's settings current: idleReturn is how long a
// borrowed card outlives its last instance (negative keeps the cards for
// good), runtimeClass the class of the NVIDIA instance pods started from now
// on (empty is the cluster's "nvidia" class when it has one, the node's
// default runtime otherwise). Instance pods that already run keep theirs.
// It is safe to call while instances start and the card pool sweeps.
func (d *Driver) ApplySettings(ctx context.Context, idleReturn time.Duration, runtimeClass string) {
	if runtimeClass == "" {
		lookup, cancel := context.WithTimeout(ctx, settingsLookupTimeout)
		runtimeClass = d.nvidiaRuntimeClass(lookup)
		cancel()
	}
	if d.runtimeClass.push(runtimeClass) {
		d.o.Log.Info("NVIDIA instance pods started from now on use another RuntimeClass", "runtime_class", runtimeClass)
	}
	if old := d.cards.setIdleReturn(idleReturn); old != idleReturn {
		d.o.Log.Info("the idle return of borrowed cards changed", "idle_return", idleReturn)
	}
	select {
	case d.settingsChanged <- struct{}{}:
	default:
	}
}

// RuntimeClass is the class NVIDIA instance pods are started with now.
func (d *Driver) RuntimeClass() string { return d.runtimeClass.get() }

// IdleReturn is how long an unused borrowed card is kept now; negative keeps
// cards.
func (d *Driver) IdleReturn() time.Duration { return d.cards.idleReturnSetting() }
