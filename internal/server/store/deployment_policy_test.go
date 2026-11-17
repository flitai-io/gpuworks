package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func openPolicyStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Open seeds the policy, so a fresh installation has a bound from the start
// rather than an empty setting that reads as "unbounded".
func TestDeploymentPolicySeededOnOpen(t *testing.T) {
	st := openPolicyStore(t)
	got := st.DeploymentPolicy()
	want := DefaultDeploymentPolicy()
	if got != want {
		t.Fatalf("seeded policy = %+v, want %+v", got, want)
	}
	if got.PendingTimeout() != 10*time.Minute {
		t.Fatalf("default pending timeout = %s, want 10m", got.PendingTimeout())
	}
	if got.StartTimeoutSec != 1800 {
		t.Fatalf("default start timeout = %ds, want 1800s", got.StartTimeoutSec)
	}
	if got.EventRetentionDays != 180 || got.EventRetentionBytes != 50<<30 {
		t.Fatalf("default retention = %d days / %d bytes", got.EventRetentionDays, got.EventRetentionBytes)
	}
}

// A corrupt value must not widen a bound. Falling through to "no limit" would
// turn a typo into an unbounded event table, so the shipped default wins.
func TestDeploymentPolicyFallsBackOnCorruptValue(t *testing.T) {
	st := openPolicyStore(t)
	for _, raw := range []string{
		"not json",
		`{"pending_timeout_sec": 0}`,
		`{"pending_timeout_sec": -5, "event_retention_days": 1, "event_retention_bytes": 67108864}`,
		`{"pending_timeout_sec": 600, "event_retention_days": 999999, "event_retention_bytes": 67108864}`,
		`{"pending_timeout_sec": 600, "start_timeout_sec": 59, "event_retention_days": 1, "event_retention_bytes": 67108864}`,
	} {
		if err := st.SetSetting(SettingDeploymentPolicy, raw); err != nil {
			t.Fatal(err)
		}
		if got := st.DeploymentPolicy(); got != DefaultDeploymentPolicy() {
			t.Fatalf("raw %q gave %+v, want the shipped default", raw, got)
		}
	}
}

func TestDeploymentPolicyAcceptsValidValues(t *testing.T) {
	st := openPolicyStore(t)
	want := DeploymentPolicy{
		PendingTimeoutSec:   120,
		StartTimeoutSec:     3600,
		EventRetentionDays:  7,
		EventRetentionBytes: 1 << 30,
	}
	b, _ := json.Marshal(want)
	if err := st.SetSetting(SettingDeploymentPolicy, string(b)); err != nil {
		t.Fatal(err)
	}
	if got := st.DeploymentPolicy(); got != want {
		t.Fatalf("policy = %+v, want %+v", got, want)
	}
	if got := want.PendingTimeout(); got != 2*time.Minute {
		t.Fatalf("PendingTimeout = %s, want 2m", got)
	}
}

func TestValidateDeploymentPolicyBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    DeploymentPolicy
		ok   bool
	}{
		{"default", DefaultDeploymentPolicy(), true},
		{"timeout too small", DeploymentPolicy{PendingTimeoutSec: DeploymentPendingTimeoutSecLo - 1, StartTimeoutSec: 1800, EventRetentionDays: 180, EventRetentionBytes: 50 << 30}, false},
		{"timeout at the floor", DeploymentPolicy{PendingTimeoutSec: DeploymentPendingTimeoutSecLo, StartTimeoutSec: 1800, EventRetentionDays: 180, EventRetentionBytes: 50 << 30}, true},
		{"timeout too large", DeploymentPolicy{PendingTimeoutSec: 24*60*60 + 1, StartTimeoutSec: 1800, EventRetentionDays: 180, EventRetentionBytes: 50 << 30}, false},
		{"retention days zero", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: 1800, EventRetentionDays: 0, EventRetentionBytes: 50 << 30}, false},
		{"retention days too large", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: 1800, EventRetentionDays: 3651, EventRetentionBytes: 50 << 30}, false},
		{"retention bytes too small", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: 1800, EventRetentionDays: 180, EventRetentionBytes: 1 << 20}, false},
		{"retention bytes too large", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: 1800, EventRetentionDays: 180, EventRetentionBytes: 1<<40 + 1}, false},
		{"start timeout too small", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: DeploymentStartTimeoutSecLo - 1, EventRetentionDays: 180, EventRetentionBytes: 50 << 30}, false},
		{"start timeout at the floor", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: DeploymentStartTimeoutSecLo, EventRetentionDays: 180, EventRetentionBytes: 50 << 30}, true},
		{"start timeout at the ceiling", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: DeploymentStartTimeoutSecHi, EventRetentionDays: 180, EventRetentionBytes: 50 << 30}, true},
		{"start timeout too large", DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: DeploymentStartTimeoutSecHi + 1, EventRetentionDays: 180, EventRetentionBytes: 50 << 30}, false},
	} {
		if err := ValidateDeploymentPolicy(tc.p); (err == nil) != tc.ok {
			t.Fatalf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// The sweep is what makes keeping events affordable, so both bounds have to
// actually bite - and independently, because a burst of failures outgrows a
// size budget long before it is old.
func TestSweepEventRetentionAppliesBothBounds(t *testing.T) {
	t.Run("age", func(t *testing.T) {
		st := openPolicyStore(t)
		p := DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: 1800, EventRetentionDays: 1, EventRetentionBytes: 1 << 30}
		b, _ := json.Marshal(p)
		_ = st.SetSetting(SettingDeploymentPolicy, string(b))

		old := &InstanceEvent{DeploymentID: 1, InstanceUUID: "u-old", Kind: EventKindSchedule,
			Code: EventDeploymentUnschedulable, CreatedAt: time.Now().Add(-72 * time.Hour)}
		fresh := &InstanceEvent{DeploymentID: 1, InstanceUUID: "u-new", Kind: EventKindSchedule,
			Code: EventDeploymentUnschedulable, CreatedAt: time.Now().Add(-time.Hour)}
		if err := st.DB().Create(old).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(fresh).Error; err != nil {
			t.Fatal(err)
		}

		n, err := st.SweepEventRetention(context.Background(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("swept %d rows, want 1", n)
		}
		kept, err := st.DeploymentEvents(1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(kept) != 1 || kept[0].InstanceUUID != "u-new" {
			t.Fatalf("kept = %+v, want only the fresh event", kept)
		}
	})

	t.Run("size", func(t *testing.T) {
		st := openPolicyStore(t)
		p := DeploymentPolicy{PendingTimeoutSec: 600, StartTimeoutSec: 1800, EventRetentionDays: 3650, EventRetentionBytes: 64 << 20}
		b, _ := json.Marshal(p)
		_ = st.SetSetting(SettingDeploymentPolicy, string(b))

		// A burst of recent events that together exceed the byte budget. Only
		// the size bound can catch this: none of them is old.
		blob := make([]byte, 1<<20)
		for i := range blob {
			blob[i] = 'x'
		}
		for i := 0; i < 100; i++ {
			if err := st.DB().Create(&InstanceEvent{
				DeploymentID: 2, Kind: EventKindSchedule, Code: EventDeploymentUnschedulable,
				Data: string(blob), CreatedAt: time.Now(),
			}).Error; err != nil {
				t.Fatal(err)
			}
		}
		n, err := st.SweepEventRetention(context.Background(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("size bound did not bite; a burst of failures would grow without limit")
		}
		var largest int64
		if err := st.DB().Model(&InstanceEvent{}).
			Select("COALESCE(MAX(LENGTH(data)), 0)").Scan(&largest).Error; err != nil {
			t.Fatal(err)
		}
		var remaining int64
		if err := st.DB().Model(&InstanceEvent{}).Count(&remaining).Error; err != nil {
			t.Fatal(err)
		}
		if remaining >= 100 {
			t.Fatalf("swept %d of 100 rows; the byte budget did not reduce the table", n)
		}
	})
}

// A policy stored before start_timeout_sec existed has no such key. It must
// keep its own values and read the shipped start timeout, not fall back to the
// defaults wholesale because the missing key decoded as zero.
func TestDeploymentPolicyOldValueGainsStartTimeoutDefault(t *testing.T) {
	st := openPolicyStore(t)
	if err := st.SetSetting(SettingDeploymentPolicy, `{"pending_timeout_sec":120,"event_retention_days":30,"event_retention_bytes":1073741824}`); err != nil {
		t.Fatal(err)
	}
	got := st.DeploymentPolicy()
	want := DeploymentPolicy{PendingTimeoutSec: 120, StartTimeoutSec: 1800, EventRetentionDays: 30, EventRetentionBytes: 1 << 30}
	if got != want {
		t.Fatalf("old stored policy read as %+v, want %+v", got, want)
	}
}

func TestValidStartTimeoutSec(t *testing.T) {
	for seconds, want := range map[int]bool{
		0: false, -1: false, DeploymentStartTimeoutSecLo - 1: false, DeploymentStartTimeoutSecLo: true,
		3600: true, DeploymentStartTimeoutSecHi: true, DeploymentStartTimeoutSecHi + 1: false,
	} {
		if got := ValidStartTimeoutSec(seconds); got != want {
			t.Fatalf("ValidStartTimeoutSec(%d) = %v, want %v", seconds, got, want)
		}
	}
}
