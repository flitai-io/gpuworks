package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DeploymentPolicy holds the cluster-wide deployment bounds: how long an
// unscheduled deployment may sit without a placement before that is reported
// as a failure, how long a started instance may take to answer its health
// check, and how long the timeline is kept.
type DeploymentPolicy struct {
	// PendingTimeoutSec is how long a deployment whose weights are all present
	// may remain unscheduled before it is reported as failed instead of pending.
	//
	// The clock starts when the weights become present, NOT when the deployment
	// is created. A 66 GiB download legitimately takes tens of minutes, so
	// counting from creation would report a timeout while the download is still
	// running normally. This is the same condition that distinguishes a stalled
	// deployment from a queued one, expressed once.
	PendingTimeoutSec int `json:"pending_timeout_sec"`
	// StartTimeoutSec is the default health budget of a started instance: how
	// long the agent waits for the engine's health endpoint before it reports
	// the start as failed. A deployment may state its own (its
	// start_timeout_sec); this applies to every deployment that does not.
	// Large checkpoints spend most of a first start compiling kernels and
	// loading weights, so the default leaves room for that.
	StartTimeoutSec int `json:"start_timeout_sec"`
	// EventRetentionDays and EventRetentionBytes bound the event timeline;
	// whichever trips first wins.
	EventRetentionDays  int   `json:"event_retention_days"`
	EventRetentionBytes int64 `json:"event_retention_bytes"`
}

// Bounds. A zero or negative value is never accepted: the timeout decides when
// a deployment is reported as failed, so "no bound" is not a safe default.
//
// The lower bound is deliberately small. The timeout only decides how long a
// deployment the scheduler has ALREADY refused keeps reading as "pending", so a
// small value cannot hurry a deployment that is merely queued - the branch is
// unreachable until the scheduler has produced a refusal. A low floor is what
// makes the end-to-end behaviour testable at L2 without waiting a real minute.
const (
	DeploymentPendingTimeoutSecLo = 5
	DeploymentPendingTimeoutSecHi = 24 * 60 * 60

	DeploymentStartTimeoutSecLo = 60
	DeploymentStartTimeoutSecHi = 24 * 60 * 60

	DeploymentEventRetentionDaysLo = 1
	DeploymentEventRetentionDaysHi = 3650

	DeploymentEventRetentionBytesLo = int64(64) << 20
	DeploymentEventRetentionBytesHi = int64(1) << 40
)

// DefaultDeploymentPolicy is the shipped policy: 10 minutes to schedule, 30
// minutes for a started instance to become healthy, and a timeline kept for
// 180 days or 50 GiB, whichever comes first.
func DefaultDeploymentPolicy() DeploymentPolicy {
	return DeploymentPolicy{
		PendingTimeoutSec:   600,
		StartTimeoutSec:     1800,
		EventRetentionDays:  180,
		EventRetentionBytes: 50 << 30,
	}
}

// ValidateDeploymentPolicy rejects values outside the supported range.
func ValidateDeploymentPolicy(p DeploymentPolicy) error {
	for _, c := range []struct {
		name   string
		v      int64
		lo, hi int64
	}{
		{"pending_timeout_sec", int64(p.PendingTimeoutSec),
			DeploymentPendingTimeoutSecLo, DeploymentPendingTimeoutSecHi},
		{"start_timeout_sec", int64(p.StartTimeoutSec),
			DeploymentStartTimeoutSecLo, DeploymentStartTimeoutSecHi},
		{"event_retention_days", int64(p.EventRetentionDays),
			DeploymentEventRetentionDaysLo, DeploymentEventRetentionDaysHi},
		{"event_retention_bytes", p.EventRetentionBytes,
			DeploymentEventRetentionBytesLo, DeploymentEventRetentionBytesHi},
	} {
		if c.v < c.lo || c.v > c.hi {
			return fmt.Errorf("deployment.%s must be between %d and %d", c.name, c.lo, c.hi)
		}
	}
	return nil
}

// PendingTimeout is the configured timeout as a duration.
func (p DeploymentPolicy) PendingTimeout() time.Duration {
	return time.Duration(p.PendingTimeoutSec) * time.Second
}

// ValidStartTimeoutSec reports whether seconds is an acceptable health budget,
// for the policy and for a deployment's own value alike.
func ValidStartTimeoutSec(seconds int) bool {
	return seconds >= DeploymentStartTimeoutSecLo && seconds <= DeploymentStartTimeoutSecHi
}

// EnsureDeploymentPolicy seeds defaults when the key is unset.
func (s *Store) EnsureDeploymentPolicy() error {
	if strings.TrimSpace(s.GetSetting(SettingDeploymentPolicy)) != "" {
		return nil
	}
	b, err := json.Marshal(DefaultDeploymentPolicy())
	if err != nil {
		return err
	}
	return s.SetSetting(SettingDeploymentPolicy, string(b))
}

// DeploymentPolicy returns the current policy, falling back to defaults when the
// key is missing or unparseable. A corrupt setting must not take scheduling
// down, and must not silently widen a bound either, so an unreadable value
// yields the shipped default rather than an unbounded one.
func (s *Store) DeploymentPolicy() DeploymentPolicy {
	out := DefaultDeploymentPolicy()
	raw := strings.TrimSpace(s.GetSetting(SettingDeploymentPolicy))
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return DefaultDeploymentPolicy()
	}
	if err := ValidateDeploymentPolicy(out); err != nil {
		return DefaultDeploymentPolicy()
	}
	return out
}

// EventRetentionCutoff is the oldest event timestamp the policy keeps.
func (p DeploymentPolicy) EventRetentionCutoff(now time.Time) time.Time {
	if p.EventRetentionDays <= 0 {
		return time.Time{}
	}
	return now.UTC().AddDate(0, 0, -p.EventRetentionDays)
}

// SweepEventRetention applies the configured bounds to the event timeline and
// reports how many rows were removed.
//
// This is what makes keeping events affordable. They are history, deliberately
// kept when their deployment is deleted (see deleteInstanceRecords), so without
// a bound the table would grow for the life of the installation. The sweep is
// passive and caller-driven, matching MarkStaleOffline: the store does no
// scheduling of its own. It stops between batches once ctx ends: the server
// waits for it on shutdown, and the next sweep picks up the rest.
func (s *Store) SweepEventRetention(ctx context.Context, now time.Time) (int64, error) {
	policy := s.DeploymentPolicy()
	return s.DeleteEventsBefore(ctx, policy.EventRetentionCutoff(now), policy.EventRetentionBytes)
}
