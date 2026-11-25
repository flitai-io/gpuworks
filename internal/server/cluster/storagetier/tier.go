// Package storagetier names the two timing profiles of a control-plane
// cluster. A tier is chosen when the cluster is installed, from how slowly its
// members' storage acknowledges a synchronous write, and every member must run
// the same one: it sets how long the database waits before it elects a new
// leader.
//
// A tier's timing can be set apart from its name: "slow-storage@100ms" is the
// slow-storage tier run at a database latency of 100 ms. Every value derived
// from the timing (the election timeout, the heartbeat, the membership
// grant's retry window, the recovery target) follows the latency, so the
// suffix is the one thing to change.
package storagetier

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Tier is a cluster's timing profile.
type Tier string

const (
	// Standard assumes storage whose synchronous writes do not stall for as
	// long as its leader step-down time.
	Standard Tier = "standard"
	// SlowStorage tolerates stalls of several hundred milliseconds: fewer
	// leader changes while idle, a slower change when a leader really fails.
	SlowStorage Tier = "slow-storage"
)

// electionFactor is how the database derives its election timeout from the
// network latency it is given. A follower waits between one and two timeouts
// before it campaigns; a leader steps down after one without a majority.
const electionFactor = 15

// The latency a tier may be run at: whole milliseconds from MinLatency
// (election timeout 150 ms, the standard tier's) to MaxLatency (3 s). Above
// that a follower may wait six seconds and more before it campaigns, longer
// than a membership grant is given at all.
const (
	MinLatency = 10 * time.Millisecond
	MaxLatency = 200 * time.Millisecond
)

// latencySeparator joins a tier's name and the latency it is run at.
const latencySeparator = "@"

// oneCommit is the time allowed for the replicated commit that repeats a
// membership grant once a new leader is elected.
const oneCommit = 500 * time.Millisecond

type profile struct {
	latency  time.Duration
	recovery time.Duration
}

// The latencies are the only values offered. A larger one makes the election
// outlast the membership grant's retry window, which then has to grow with
// it; the store's tests hold every tier to that relation.
//
// The recovery targets are what was measured when a leader was killed on
// four-core members with networked storage (five runs each): 0.8 to 2.1 s at
// 10 ms and 1.7 to 3.1 s at 50 ms.
var profiles = map[Tier]profile{
	Standard:    {latency: 10 * time.Millisecond, recovery: 10 * time.Second},
	SlowStorage: {latency: 50 * time.Millisecond, recovery: 6 * time.Second},
}

// All lists the tiers from the fastest timing to the slowest.
func All() []Tier { return []Tier{Standard, SlowStorage} }

// Parse reads a recorded or configured tier. The empty string is Standard: a
// cluster installed before tiers existed records none. A name may carry the
// latency the tier is run at ("slow-storage@100ms"). The result is the one
// spelling of that timing (WithLatency): "slow-storage@50ms" is slow-storage,
// and "standard@50ms" is slow-storage too.
func Parse(name string) (Tier, error) {
	if name == "" {
		return Standard, nil
	}
	base, latency, timed, err := split(name)
	if err != nil {
		return "", err
	}
	if !timed {
		return base, nil
	}
	return base.WithLatency(latency)
}

// split is the one reading of a tier's text: its named tier and, when it
// carries one, the latency after the separator, written as whole
// milliseconds without a sign or leading zeros and with the unit.
func split(name string) (base Tier, latency time.Duration, timed bool, err error) {
	head, tail, timed := strings.Cut(name, latencySeparator)
	if _, ok := profiles[Tier(head)]; !ok {
		return "", 0, false, fmt.Errorf("unknown storage tier %q: want %s or %s, optionally with the latency it runs at (%s%s100ms)", name, Standard, SlowStorage, SlowStorage, latencySeparator)
	}
	if !timed {
		return Tier(head), 0, false, nil
	}
	ms, ok := strings.CutSuffix(tail, "ms")
	n, parseErr := strconv.ParseUint(ms, 10, 16)
	if !ok || parseErr != nil || strconv.FormatUint(n, 10) != ms {
		return "", 0, false, fmt.Errorf("storage tier %q: the latency after %q must be whole milliseconds, as in %s%s100ms", name, latencySeparator, head, latencySeparator)
	}
	return Tier(head), time.Duration(n) * time.Millisecond, true, nil
}

// ValidLatency reports whether a database node may be run at this latency.
func ValidLatency(latency time.Duration) bool {
	return latency >= MinLatency && latency <= MaxLatency && latency%time.Millisecond == 0
}

// ElectionTimeoutFor is the election timeout of a database node run at this
// latency.
func ElectionTimeoutFor(latency time.Duration) time.Duration { return latency * electionFactor }

// WithLatency is the tier run at another latency. One timing has one
// spelling: a named tier's own latency gives that tier, whichever name it
// was asked on, and any other latency is written on slow-storage. Members
// compare their recorded tiers as text, and "standard@50ms" beside
// "slow-storage" would be one timing taken for two.
func (t Tier) WithLatency(latency time.Duration) (Tier, error) {
	if !ValidLatency(latency) {
		return "", fmt.Errorf("storage tier %s: latency %s is outside %s to %s in whole milliseconds", t.Base(), latency, MinLatency, MaxLatency)
	}
	if named, ok := ForNetworkLatency(latency); ok {
		return named, nil
	}
	return Tier(string(SlowStorage) + latencySeparator + strconv.FormatInt(latency.Milliseconds(), 10) + "ms"), nil
}

// Base is the named tier without a latency of its own.
func (t Tier) Base() Tier {
	base, _, _, err := split(string(t))
	if err != nil {
		return Standard
	}
	return base
}

// ForNetworkLatency is the tier that runs the database at this latency.
func ForNetworkLatency(latency time.Duration) (Tier, bool) {
	for _, tier := range All() {
		if profiles[tier].latency == latency {
			return tier, true
		}
	}
	return "", false
}

// Recorded is what a member stores for the tier: nothing for Standard, so a
// standard cluster's files are those of a cluster installed before tiers.
func (t Tier) Recorded() string {
	if t == Standard || t == "" {
		return ""
	}
	return string(t)
}

// NetworkLatency is the latency the database node is started with. A Tier
// comes from Parse or WithLatency; text that neither would produce (an
// unknown name, a latency Parse refuses or one outside the range) is no tier
// and reads as Standard, as the empty tier does. It never reads as another
// timing than the text names.
func (t Tier) NetworkLatency() time.Duration {
	base, latency, timed, err := split(string(t))
	switch {
	case err != nil:
		return profiles[Standard].latency
	case !timed:
		return profiles[base].latency
	case !ValidLatency(latency):
		return profiles[Standard].latency
	}
	return latency
}

// HeartbeatInterval is how often the leader's database sends to an idle
// follower: one and a half times the latency.
func (t Tier) HeartbeatInterval() time.Duration { return t.NetworkLatency() * 3 / 2 }

// ElectionTimeout is how long a leader goes without a majority before it
// steps down, and the shortest a follower waits before it campaigns.
func (t Tier) ElectionTimeout() time.Duration { return ElectionTimeoutFor(t.NetworkLatency()) }

// ElectionTimer is the range a follower's randomized election timer is drawn
// from.
func (t Tier) ElectionTimer() (shortest, longest time.Duration) {
	return t.ElectionTimeout(), 2 * t.ElectionTimeout()
}

// RecoveryTarget is the time within which a write succeeds again after the
// leader's process is killed. It was measured at each tier's own latency; a
// tier run slower is given the two election timeouts by which its followers
// may wait longer.
func (t Tier) RecoveryTarget() time.Duration {
	base := t.Base()
	target := profiles[base].recovery
	if extra := t.ElectionTimeout() - base.ElectionTimeout(); extra > 0 {
		target += 2 * extra
	}
	return target
}

// GrantRetryNeed is how long a membership grant interrupted by a leader
// change must be repeated for to meet the next leader: a follower campaigns
// up to two election timeouts after the leader is gone, and the repeated
// grant then needs one replicated commit.
func (t Tier) GrantRetryNeed() time.Duration {
	_, longest := t.ElectionTimer()
	return longest + oneCommit
}

// RollsWith reports whether members running t and members running other can
// serve side by side for as long as it takes to restart the members one at a
// time. Whichever of them leads must send to an idle follower at least three
// times within the shortest wait before the other campaigns, so that two lost
// heartbeats do not start an election. Timings further apart than that are
// changed with every member stopped, or in steps.
func (t Tier) RollsWith(other Tier) bool {
	slow, fast := t, other
	if fast.Slower(slow) {
		slow, fast = fast, slow
	}
	return 3*slow.HeartbeatInterval() <= fast.ElectionTimeout()
}

// Stretch lengthens a window that is fixed in the code to the timing the tier
// runs at. Such windows (how long a leader observation stays fresh, how long
// an unacknowledged peer connection is kept) were set against the
// slow-storage election timing; a tier that elects later than that needs them
// longer in proportion, or they end before an election can. A tier at
// slow-storage's latency or below keeps the window as it is.
func (t Tier) Stretch(window time.Duration) time.Duration {
	return StretchFor(t.NetworkLatency(), window)
}

// StretchFor is Stretch for a database node run at this latency.
func StretchFor(latency, window time.Duration) time.Duration {
	base := profiles[SlowStorage].latency
	if latency <= base {
		return window
	}
	return time.Duration(float64(window) * float64(latency) / float64(base)).Round(time.Millisecond)
}

// Slower reports whether t waits longer than other before electing a leader.
func (t Tier) Slower(other Tier) bool { return t.NetworkLatency() > other.NetworkLatency() }
