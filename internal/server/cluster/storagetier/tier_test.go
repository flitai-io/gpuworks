package storagetier

import (
	"testing"
	"time"
)

func TestTiersCarryTheirMeasuredTiming(t *testing.T) {
	for _, tc := range []struct {
		tier                       Tier
		latency, shortest, longest time.Duration
		recovery                   time.Duration
		recorded                   string
	}{
		{Standard, 10 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond, 10 * time.Second, ""},
		{SlowStorage, 50 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond, 6 * time.Second, "slow-storage"},
	} {
		shortest, longest := tc.tier.ElectionTimer()
		if tc.tier.NetworkLatency() != tc.latency || shortest != tc.shortest || longest != tc.longest || tc.tier.ElectionTimeout() != tc.shortest || tc.tier.RecoveryTarget() != tc.recovery || tc.tier.Recorded() != tc.recorded {
			t.Errorf("%s: latency %s timer %s-%s recovery %s recorded %q", tc.tier, tc.tier.NetworkLatency(), shortest, longest, tc.tier.RecoveryTarget(), tc.tier.Recorded())
		}
		if got, ok := ForNetworkLatency(tc.latency); !ok || got != tc.tier {
			t.Errorf("ForNetworkLatency(%s) = %q, %v", tc.latency, got, ok)
		}
	}
	if got := All(); len(got) != 2 || got[0] != Standard || got[1] != SlowStorage {
		t.Fatalf("All() = %v", got)
	}
	if !SlowStorage.Slower(Standard) || Standard.Slower(SlowStorage) || Standard.Slower(Standard) {
		t.Fatal("slow-storage must be the only tier slower than standard")
	}
}

func TestParseAcceptsOnlyTheNamedTiers(t *testing.T) {
	for name, want := range map[string]Tier{"": Standard, "standard": Standard, "slow-storage": SlowStorage} {
		if got, err := Parse(name); err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"slow", "Standard", "slow_storage", "100ms", " standard", "@100ms", "fast@100ms"} {
		if got, err := Parse(name); err == nil {
			t.Errorf("Parse(%q) = %q, want a refusal", name, got)
		}
	}
	for _, latency := range []time.Duration{0, time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond} {
		if tier, ok := ForNetworkLatency(latency); ok {
			t.Errorf("ForNetworkLatency(%s) = %q, want no tier", latency, tier)
		}
	}
	var unset Tier
	if unset.NetworkLatency() != 10*time.Millisecond || unset.Recorded() != "" {
		t.Fatal("an unset tier must behave as standard")
	}
}

// A tier run at a latency of its own is spelled name@<n>ms, and everything
// derived from the timing follows that latency.
func TestATierRunsAtALatencyOfItsOwn(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		name            string
		want            Tier
		latency         time.Duration
		election        time.Duration
		heartbeat       time.Duration
		grantRetryNeed  time.Duration
		recoveryTarget  time.Duration
		recorded        string
		slowerThanNamed bool
	}{
		{"slow-storage@100ms", "slow-storage@100ms", 100 * ms, 1500 * ms, 150 * ms, 3500 * ms, 7500 * ms, "slow-storage@100ms", true},
		{"slow-storage@200ms", "slow-storage@200ms", 200 * ms, 3000 * ms, 300 * ms, 6500 * ms, 10500 * ms, "slow-storage@200ms", true},
		// The tier's own latency is the tier itself.
		{"slow-storage@50ms", SlowStorage, 50 * ms, 750 * ms, 75 * ms, 2000 * ms, 6000 * ms, "slow-storage", false},
		{"standard@10ms", Standard, 10 * ms, 150 * ms, 15 * ms, 800 * ms, 10000 * ms, "", false},
		// A tier run faster than itself keeps its measured recovery target.
		{"slow-storage@20ms", "slow-storage@20ms", 20 * ms, 300 * ms, 30 * ms, 1100 * ms, 6000 * ms, "slow-storage@20ms", false},
		// One timing has one spelling, whichever name it was asked on.
		{"standard@50ms", SlowStorage, 50 * ms, 750 * ms, 75 * ms, 2000 * ms, 6000 * ms, "slow-storage", false},
		{"slow-storage@10ms", Standard, 10 * ms, 150 * ms, 15 * ms, 800 * ms, 10000 * ms, "", false},
		{"standard@100ms", "slow-storage@100ms", 100 * ms, 1500 * ms, 150 * ms, 3500 * ms, 7500 * ms, "slow-storage@100ms", true},
	}
	for _, tc := range cases {
		tier, err := Parse(tc.name)
		if err != nil || tier != tc.want {
			t.Errorf("Parse(%q) = %q, %v; want %q", tc.name, tier, err, tc.want)
			continue
		}
		if got := tier.NetworkLatency(); got != tc.latency {
			t.Errorf("%s: latency %s, want %s", tc.name, got, tc.latency)
		}
		if got := tier.ElectionTimeout(); got != tc.election || ElectionTimeoutFor(tc.latency) != tc.election {
			t.Errorf("%s: election timeout %s (for the latency: %s), want %s", tc.name, got, ElectionTimeoutFor(tc.latency), tc.election)
		}
		if shortest, longest := tier.ElectionTimer(); shortest != tc.election || longest != 2*tc.election {
			t.Errorf("%s: election timer %s to %s", tc.name, shortest, longest)
		}
		if got := tier.HeartbeatInterval(); got != tc.heartbeat {
			t.Errorf("%s: heartbeat %s, want %s", tc.name, got, tc.heartbeat)
		}
		if got := tier.GrantRetryNeed(); got != tc.grantRetryNeed {
			t.Errorf("%s: grant retry need %s, want %s", tc.name, got, tc.grantRetryNeed)
		}
		if got := tier.RecoveryTarget(); got != tc.recoveryTarget {
			t.Errorf("%s: recovery target %s, want %s", tc.name, got, tc.recoveryTarget)
		}
		if got := tier.Recorded(); got != tc.recorded {
			t.Errorf("%s: recorded as %q, want %q", tc.name, got, tc.recorded)
		}
		if got := tier.Slower(tier.Base()); got != tc.slowerThanNamed {
			t.Errorf("%s: slower than %s = %v", tc.name, tier.Base(), got)
		}
		// What is recorded reads back as the same tier.
		if again, err := Parse(tier.Recorded()); err != nil || again != tier {
			t.Errorf("%s: recorded %q reads back as %q, %v", tc.name, tier.Recorded(), again, err)
		}
		if !ValidLatency(tc.latency) {
			t.Errorf("%s: its latency is not valid", tc.name)
		}
	}
	for _, name := range []string{
		"slow-storage@9ms", "slow-storage@201ms", "slow-storage@0ms", "slow-storage@100", "slow-storage@100s",
		"slow-storage@1.5ms", "slow-storage@-50ms", "slow-storage@050ms", "slow-storage@", "slow-storage@100ms@100ms",
		"slow-storage@ 100ms", "slow-storage@100MS", "slow-storage@99999999999ms",
	} {
		if got, err := Parse(name); err == nil {
			t.Errorf("Parse(%q) = %q, want a refusal", name, got)
		}
	}
	for latency, want := range map[time.Duration]bool{
		10 * ms: true, 200 * ms: true, 137 * ms: true, 9 * ms: false, 201 * ms: false, 0: false,
		50*ms + 500*time.Microsecond: false, -50 * ms: false,
	} {
		if got := ValidLatency(latency); got != want {
			t.Errorf("ValidLatency(%s) = %v, want %v", latency, got, want)
		}
	}
	if _, err := SlowStorage.WithLatency(250 * ms); err == nil {
		t.Error("a latency above the range was accepted")
	}
	// A tier carried under a name this build does not know behaves as
	// standard, as an unset one does.
	if got := Tier("unknown@100ms").Base(); got != Standard {
		t.Errorf("base of an unknown tier = %q", got)
	}
}

// Members restarted one at a time run two timings side by side for a while.
// That is allowed only when the slower timing still sends three heartbeats
// within the faster one's election timeout, whichever way the change goes.
func TestTimingsThatMayRunSideBySide(t *testing.T) {
	at := func(ms int) Tier {
		tier, err := SlowStorage.WithLatency(time.Duration(ms) * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return tier
	}
	for _, tc := range []struct {
		a, b Tier
		want bool
	}{
		{SlowStorage, SlowStorage, true},
		{SlowStorage, at(100), true},
		{at(100), at(200), true},
		{at(60), at(200), true},  // 900 ms against heartbeats of 300 ms
		{at(59), at(200), false}, // 885 ms
		{SlowStorage, at(200), false},
		{Standard, at(33), true},
		{Standard, at(34), false},
		{Standard, SlowStorage, false},
	} {
		if got := tc.a.RollsWith(tc.b); got != tc.want {
			t.Errorf("%s with %s: %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := tc.b.RollsWith(tc.a); got != tc.want {
			t.Errorf("%s with %s: %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}

// A fixed window keeps its length up to slow-storage's timing and grows in
// proportion beyond it.
func TestWindowsStretchWithALaterElection(t *testing.T) {
	at := func(ms int) Tier {
		tier, err := SlowStorage.WithLatency(time.Duration(ms) * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return tier
	}
	for _, tc := range []struct {
		tier Tier
		want time.Duration
	}{
		{Standard, 3 * time.Second},
		{at(20), 3 * time.Second},
		{SlowStorage, 3 * time.Second},
		{at(75), 4500 * time.Millisecond},
		{at(100), 6 * time.Second},
		{at(200), 12 * time.Second},
	} {
		if got := tc.tier.Stretch(3 * time.Second); got != tc.want || StretchFor(tc.tier.NetworkLatency(), 3*time.Second) != tc.want {
			t.Errorf("%s: a 3 s window becomes %s, want %s", tc.tier, got, tc.want)
		}
		// The window never ends before the longest election of the tier
		// can: two election timeouts.
		if _, longest := tc.tier.ElectionTimer(); tc.tier.Stretch(3*time.Second) < longest {
			t.Errorf("%s: the window %s is shorter than the longest election timer %s", tc.tier, tc.tier.Stretch(3*time.Second), longest)
		}
	}
}

// A Tier comes from Parse. Text Parse refuses is no tier: read as a Tier it
// is Standard, never the timing its digits might suggest and never another
// tier's.
func TestTextParseRefusesIsNoTier(t *testing.T) {
	for _, text := range []string{
		"slow-storage@100", "slow-storage@0100ms", "slow-storage@+100ms", "slow-storage@100ms ", "slow-storage@",
		"slow-storage@201ms", "slow-storage@9ms", "slow-storage@1000ms", "glacial", "glacial@100ms",
	} {
		if tier, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) = %q, want it refused", text, tier)
		}
		tier := Tier(text)
		if got := tier.NetworkLatency(); got != Standard.NetworkLatency() {
			t.Errorf("Tier(%q).NetworkLatency() = %s, want Standard's", text, got)
		}
		if got := tier.ElectionTimeout(); got != Standard.ElectionTimeout() {
			t.Errorf("Tier(%q).ElectionTimeout() = %s, want Standard's", text, got)
		}
	}
	if Tier("").NetworkLatency() != Standard.NetworkLatency() || Tier("glacial").Base() != Standard {
		t.Error("the empty and the unknown tier are not Standard")
	}
}
