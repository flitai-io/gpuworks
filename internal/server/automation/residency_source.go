package automation

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// What the residency arbiter reads and how it acts (residency.go).

// Waiter is a deployment with requests held for its wake, or (Place) one
// whose next replica waits for room to be placed.
type Waiter struct {
	DeploymentID uint
	Policy       yieldpolicy.Policy
	// Since is when its oldest held request came, or its replica started
	// waiting.
	Since time.Time
	// Place says it has nothing asleep to wake: what blocks it is what keeps
	// its replica from being placed, and once that sleeps the controller
	// places it.
	Place bool
}

// Holder is an awake replica on cards a waiting deployment needs, and the
// deployment it serves.
type Holder struct {
	DeploymentID uint
	Name         string
	Policy       yieldpolicy.Policy
	// Instances are the replica's instances, put to sleep together.
	Instances []string
	// Starting says the replica is still starting: it has served nothing
	// yet. Only the replica's own instances say so; another replica of its
	// deployment starting or waiting to be placed does not.
	Starting bool
}

// Blocking is what keeps a waiting deployment's sleepers from waking: the
// awake replicas on the cards they need, or an occupant nothing can put to
// sleep (a process outside the platform, a deployment that cannot sleep).
type Blocking struct {
	Holders []Holder
	Blocked bool
}

// Activity is a deployment's traffic as the gateway sees it.
type Activity struct {
	InFlight    int
	Held        int
	LastRequest time.Time
}

// ResidencySource is what the residency arbiter reads.
type ResidencySource interface {
	// Waiters are the deployments with a request held for their wake whose
	// sleep a request may end (not held by an operator or a schedule).
	Waiters() ([]Waiter, error)
	// Wakeable says the deployment is active and has a sleeper the
	// platform put to sleep (one a request wakes), with its policy; a
	// deployment whose sleepers an operator or a schedule all holds is not.
	Wakeable(deploymentID uint) (policy yieldpolicy.Policy, ok bool)
	// Blocking is what keeps the deployment's waiting sleepers from waking.
	Blocking(deploymentID uint) (Blocking, error)
	// Activity is the deployment's traffic.
	Activity(deploymentID uint) Activity
	// InstancesInFlight is how many requests run on the instances: what a
	// holder's replica drains before it sleeps, its siblings serving on.
	InstancesInFlight(instances []string) int
	// Settling says the deployment is not serving yet: its replica waits to
	// be placed, or an instance of it is still starting. It is asked of a
	// deployment a holder gave its cards to (stillWakingFor); whether a
	// holder itself is starting is its own replica's (Holder.Starting).
	Settling(deploymentID uint) bool
	// AwakeSince is when the deployment's instances last began serving, as
	// their state and sleep events say, looking back at most within: the
	// latest of them when there are several. ok is false when the events do
	// not show every one of them serving now. It is the start of a holder's
	// turn the arbiter did not see begin: a wake ordered by an operator or a
	// schedule, or one before the server started. A stretch longer than
	// within reads as within.
	AwakeSince(deploymentID uint, instances []string, within time.Duration) (time.Time, bool)
	// Preempted are the deployments put to sleep for another that still
	// sleep for it, a request waking them, as the event log leaves them: the
	// resume list of a server that has just started (Preemption).
	Preempted() ([]Preemption, error)
}

// Preemption is a deployment whose latest sleep was a yield to another
// (AUTO_SLEEP, trigger preempt) and whose sleepers a request still wakes.
type Preemption struct {
	DeploymentID uint
	// Since is when it yielded; For is whom it yielded to (0 when the event
	// does not say).
	Since time.Time
	For   uint
}

// ResidencyActions is how the residency arbiter acts.
type ResidencyActions interface {
	// Yield puts a holder's replica to sleep for another deployment, cutting
	// its requests off when abort; it returns the level it chose (the
	// holder's sleep level preference, yieldpolicy.Policy.SleepLevel).
	Yield(deploymentID uint, instances []string, abort bool) (int, error)
	// Drain stops (on) or resumes routing new requests to the deployment's
	// instances; its other replicas take them, and with none left they are
	// held as if it slept.
	Drain(deploymentID uint, instances []string, on bool)
}
