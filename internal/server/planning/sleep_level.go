package planning

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Every sleep order - an operator's for a deployment, a role or a replica, a
// schedule's, the automation's idle sleep, the residency arbiter's yield and
// an operator's yielding wake - takes its level and is written here, so the
// rule of which level a sleep lands at is stated once.
//
// A level is asked for as a preference (yieldpolicy.LevelAuto, LevelL1,
// LevelL2): auto sleeps at L1 and at L2 where a host cannot hold the weights
// an L1 sleep moves into its memory (L1HostMemoryRefusal); l1 is refused
// there instead; l2 needs no host memory and is never refused for it.

// SleepPreference is the preference an order's numeric level states: an
// explicit proto.SleepLevelL1 or proto.SleepLevelL2, and anything else (the
// automation's LevelAuto, 0) leaves the choice to the platform.
func SleepPreference(level int) string {
	switch level {
	case proto.SleepLevelL1:
		return yieldpolicy.LevelL1
	case proto.SleepLevelL2:
		return yieldpolicy.LevelL2
	}
	return yieldpolicy.LevelAuto
}

// ResolveSleepLevel is the level a sleep of the named instances of dep lands
// at for the preference pref, or why it may not: an L1 preference the hosts
// cannot hold is refused (SLEEP_HOST_MEMORY_INSUFFICIENT), an auto one falls
// back to L2. host nil knows nothing and refuses nothing.
func ResolveSleepLevel(st *store.Store, dep *store.Deployment, named []store.Instance, pref string, host HostMemory) (int, *SleepRefusal) {
	if pref == yieldpolicy.LevelL2 {
		return proto.SleepLevelL2, nil
	}
	refusal := L1HostMemoryRefusal(st, dep, named, host)
	switch {
	case refusal == nil:
		return proto.SleepLevelL1, nil
	case pref == yieldpolicy.LevelL1:
		return 0, refusal
	}
	return proto.SleepLevelL2, nil
}

// SleepOrder is one sleep order for a deployment.
type SleepOrder struct {
	// Pref is the level preference (yieldpolicy.LevelAuto, LevelL1,
	// LevelL2).
	Pref string
	// Mode is the pause mode (proto.SleepModeWait or SleepModeAbort).
	Mode string
	// Held says an operator or a schedule ordered it: a request does not
	// wake the instances it puts to sleep (store.Instance.SleepHeld). A
	// sleep the platform chose (a yield) is not held, and leaves every
	// instance's hold as it was.
	Held bool
	// Instances names the instances ordered to sleep; nil orders the whole
	// deployment, whose awake sleepers are what an L1 sleep moves into host
	// memory. A whole-deployment order is held (store.OrderDeploymentSleep);
	// one that is not names every sleeper instead.
	Instances []string
}

// OrderSleep writes a sleep order for dep at the level its preference
// resolves to (ResolveSleepLevel) and returns that level. A refusal - the
// deployment cannot sleep (SleepEligibility), or an L1 preference the hosts
// cannot hold - writes nothing. err is a store failure; an instance named
// that is no longer the deployment's is store.ErrNotFound. The caller holds
// the server's order lock (lifecycle.Orders.Lock).
func OrderSleep(st *store.Store, dep *store.Deployment, order SleepOrder, host HostMemory) (int, *SleepRefusal, error) {
	if code := SleepEligibility(dep); code != "" {
		return 0, &SleepRefusal{Code: code, Params: map[string]any{"engine": dep.Engine}}, nil
	}
	level := proto.SleepLevelL2
	if order.Instances == nil && !order.Held {
		insts, err := st.InstancesForDeployment(dep.ID)
		if err != nil {
			return 0, nil, err
		}
		order.Instances = instanceUUIDs(Sleepers(insts))
		if len(order.Instances) == 0 {
			return 0, nil, nil
		}
	}
	if order.Pref != yieldpolicy.LevelL2 {
		insts, err := st.InstancesForDeployment(dep.ID)
		if err != nil {
			return 0, nil, err
		}
		named := Sleepers(insts)
		if order.Instances != nil {
			named = instancesNamed(insts, order.Instances)
		}
		var refusal *SleepRefusal
		if level, refusal = ResolveSleepLevel(st, dep, named, order.Pref, host); refusal != nil {
			return 0, refusal, nil
		}
	}
	if order.Instances == nil {
		return level, nil, st.OrderDeploymentSleep(dep.ID, level, order.Mode)
	}
	return level, nil, st.OrderInstancesSleepHeld(dep.ID, order.Instances, level, order.Mode, order.Held)
}

// Err is the refusal as a product error, for a caller with no response of
// its own to write (the automation records the code on the timeline).
func (r *SleepRefusal) Err() error {
	fallback := "the sleep order was refused"
	switch r.Code {
	case sleep.CodeHostMemoryInsufficient:
		fallback = "the host memory cannot hold the weights an L1 sleep moves there"
	case sleep.CodeNotEnabled, sleep.CodeEngineUnsupported:
		fallback = "the deployment cannot sleep"
	}
	return messages.NewProductError(r.Code, fallback, r.Params)
}

// instancesNamed are the instances of insts the uuids name, in insts' order.
func instancesNamed(insts []store.Instance, uuids []string) []store.Instance {
	want := make(map[string]bool, len(uuids))
	for _, uuid := range uuids {
		want[uuid] = true
	}
	var out []store.Instance
	for _, in := range insts {
		if want[in.UUID] {
			out = append(out, in)
		}
	}
	return out
}
