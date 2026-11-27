package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
)

// The token usage of usage analytics: what every API key used on every
// deployment, per hour for the recent past and per day for long after
// (ADR 0055). It is what customers are
// billed by, so it lives here, backed up and migrated with the platform,
// rather than in stats.db with the rest of the long-term statistics.

// UsageHourly is one hour of a key's use of a deployment. Counts add up as
// rollup's deltas arrive; see rollup.Usage for what each one counts.
type UsageHourly struct {
	HourMS       int64 `gorm:"primaryKey;autoIncrement:false;index:idx_usage_hourly_key,priority:2"`
	APIKeyID     uint  `gorm:"primaryKey;autoIncrement:false;index:idx_usage_hourly_key,priority:1"`
	UserID       uint  `gorm:"primaryKey;autoIncrement:false"`
	DeploymentID uint  `gorm:"primaryKey;autoIncrement:false"`
	// Source is uxrecord.Source: 0 the /v1 gateway, 1 the playground.
	Source      uint8 `gorm:"primaryKey;autoIncrement:false"`
	UsageCounts `gorm:"embedded"`
}

func (UsageHourly) TableName() string { return "usage_hourly" }

// UsageDaily is one day (YYYYMMDD in the statistics time zone) of a key's use
// of a deployment, rolled up from its hours.
type UsageDaily struct {
	Day          int32 `gorm:"primaryKey;autoIncrement:false;index:idx_usage_daily_key,priority:2"`
	APIKeyID     uint  `gorm:"primaryKey;autoIncrement:false;index:idx_usage_daily_key,priority:1"`
	UserID       uint  `gorm:"primaryKey;autoIncrement:false"`
	DeploymentID uint  `gorm:"primaryKey;autoIncrement:false"`
	Source       uint8 `gorm:"primaryKey;autoIncrement:false"`
	UsageCounts  `gorm:"embedded"`
}

func (UsageDaily) TableName() string { return "usage_daily" }

// UsageCounts are the counts of a usage row (rollup.Usage).
type UsageCounts struct {
	Requests            int64
	NoUsage             int64
	PromptTokens        int64
	CompletionTokens    int64
	ReasoningTokens     int64
	ReasoningReported   int64
	CachedTokens        int64
	CacheReported       int64
	CacheReportedPrompt int64
}

// Rollup returns the counts as rollup.Usage.
func (c UsageCounts) Rollup() rollup.Usage {
	return rollup.Usage(c)
}

// UsageRolledDay records a day whose usage rows are rolled up.
type UsageRolledDay struct {
	Day int32 `gorm:"primaryKey;autoIncrement:false"`
}

func (UsageRolledDay) TableName() string { return "usage_rolled_days" }

// UsageRollupState is the single row saying when live accumulation began
// (LiveSinceMS: requests that ended before it are only known from the
// one-off fill out of the request log) and how far that fill got.
type UsageRollupState struct {
	ID                uint `gorm:"primaryKey"`
	LiveSinceMS       int64
	BackfillThroughMS int64
	BackfillDone      bool
}

func (UsageRollupState) TableName() string { return "usage_rollup_state" }

// DeletedDeployment remembers a deleted deployment's names for the usage and
// statistics it leaves behind; written in the transaction that deletes it.
type DeletedDeployment struct {
	ID         uint   `gorm:"primaryKey;autoIncrement:false"`
	Name       string `gorm:"size:128"`
	ServedName string `gorm:"size:128"`
	DeletedAt  time.Time
}

func createOperationsUsage(db *gorm.DB) error {
	return db.AutoMigrate(&UsageHourly{}, &UsageDaily{}, &UsageRolledDay{}, &UsageRollupState{}, &DeletedDeployment{})
}

// AddUsage adds a delta to the hour rows in one transaction.
func (s *Store) AddUsage(ctx context.Context, delta map[rollup.UsageKey]rollup.Usage) error {
	if len(delta) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return addUsageTx(tx, delta) })
}

// AddUsageBackfill adds a delta filled in from the request log and records,
// in the same transaction, that the fill now reaches back to throughMS: a
// fill cut short resumes there and adds nothing twice.
func (s *Store) AddUsageBackfill(ctx context.Context, delta map[rollup.UsageKey]rollup.Usage, throughMS int64, done bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := addUsageTx(tx, delta); err != nil {
			return err
		}
		return tx.Model(&UsageRollupState{}).Where("id = 1").
			Updates(map[string]any{"backfill_through_ms": throughMS, "backfill_done": done}).Error
	})
}

func addUsageTx(tx *gorm.DB, delta map[rollup.UsageKey]rollup.Usage) error {
	for k, u := range delta {
		if err := tx.Exec(`INSERT INTO usage_hourly (hour_ms, api_key_id, user_id, deployment_id, source,
			requests, no_usage, prompt_tokens, completion_tokens, reasoning_tokens, reasoning_reported,
			cached_tokens, cache_reported, cache_reported_prompt) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (hour_ms, api_key_id, user_id, deployment_id, source) DO UPDATE SET
			requests = requests + excluded.requests, no_usage = no_usage + excluded.no_usage,
			prompt_tokens = prompt_tokens + excluded.prompt_tokens, completion_tokens = completion_tokens + excluded.completion_tokens,
			reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens, reasoning_reported = reasoning_reported + excluded.reasoning_reported,
			cached_tokens = cached_tokens + excluded.cached_tokens, cache_reported = cache_reported + excluded.cache_reported,
			cache_reported_prompt = cache_reported_prompt + excluded.cache_reported_prompt`,
			k.HourMS, k.APIKeyID, k.UserID, k.DeploymentID, uint8(k.Source),
			u.Requests, u.NoUsage, u.PromptTokens, u.CompletionTokens, u.ReasoningTokens, u.ReasoningReported,
			u.CachedTokens, u.CacheReported, u.CacheReportedPrompt).Error; err != nil {
			return err
		}
	}
	return nil
}

// RollupUsageDay writes day's rows from its hours, replacing any written
// before, and records it as rolled.
func (s *Store) RollupUsageDay(ctx context.Context, day rollup.Day, loc *time.Location) error {
	from, to := day.HoursOf(loc)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("day = ?", int32(day)).Delete(&UsageDaily{}).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO usage_daily (day, api_key_id, user_id, deployment_id, source,
			requests, no_usage, prompt_tokens, completion_tokens, reasoning_tokens, reasoning_reported,
			cached_tokens, cache_reported, cache_reported_prompt)
			SELECT ?, api_key_id, user_id, deployment_id, source,
			SUM(requests), SUM(no_usage), SUM(prompt_tokens), SUM(completion_tokens), SUM(reasoning_tokens), SUM(reasoning_reported),
			SUM(cached_tokens), SUM(cache_reported), SUM(cache_reported_prompt)
			FROM usage_hourly WHERE hour_ms >= ? AND hour_ms < ?
			GROUP BY api_key_id, user_id, deployment_id, source`, int32(day), from, to).Error; err != nil {
			return fmt.Errorf("roll up usage %d: %w", day, err)
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&UsageRolledDay{Day: int32(day)}).Error
	})
}

// UsageDayRolled reports whether day's usage is rolled up.
func (s *Store) UsageDayRolled(ctx context.Context, day rollup.Day) (bool, error) {
	var n int64
	err := s.reader().WithContext(ctx).Model(&UsageRolledDay{}).Where("day = ?", int32(day)).Count(&n).Error
	return n > 0, err
}

// usageRetainBatch bounds one delete, so retention never holds the writer
// long.
const usageRetainBatch = 5000

// RetainUsage deletes hour rows older than hourCutoffMS and day rows before
// dayCutoff, in batches, and returns how many went.
func (s *Store) RetainUsage(ctx context.Context, hourCutoffMS int64, dayCutoff rollup.Day) (int64, error) {
	var total int64
	for _, step := range []struct {
		table, column string
		cutoff        int64
	}{
		{"usage_hourly", "hour_ms", hourCutoffMS},
		{"usage_daily", "day", int64(dayCutoff)},
		{"usage_rolled_days", "day", int64(dayCutoff)},
	} {
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			res := s.db.WithContext(ctx).Exec(`DELETE FROM `+step.table+` WHERE rowid IN (SELECT rowid FROM `+
				step.table+` WHERE `+step.column+` < ? LIMIT ?)`, step.cutoff, usageRetainBatch)
			if res.Error != nil {
				return total, fmt.Errorf("retain %s: %w", step.table, res.Error)
			}
			total += res.RowsAffected
			if res.RowsAffected < usageRetainBatch {
				break
			}
		}
	}
	return total, nil
}

// UsageRowCounts is how many hour and day rows the usage holds.
func (s *Store) UsageRowCounts(ctx context.Context) (hourly, daily int64, err error) {
	db := s.reader().WithContext(ctx)
	if err := db.Model(&UsageHourly{}).Count(&hourly).Error; err != nil {
		return 0, 0, err
	}
	err = db.Model(&UsageDaily{}).Count(&daily).Error
	return hourly, daily, err
}

// UsageRollupState reads the state row; the first call of a fresh database
// creates it with live accumulation starting at now.
func (s *Store) UsageRollupState(now time.Time) (UsageRollupState, error) {
	var state UsageRollupState
	err := s.db.Transaction(func(tx *gorm.DB) error {
		err := tx.First(&state, 1).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			state = UsageRollupState{ID: 1, LiveSinceMS: now.UnixMilli(), BackfillThroughMS: now.UnixMilli()}
			return tx.Create(&state).Error
		}
		return err
	})
	return state, err
}

// DeletedDeployments returns the names deleted deployments left, by id.
func (s *Store) DeletedDeployments(ids []uint) (map[uint]DeletedDeployment, error) {
	out := map[uint]DeletedDeployment{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []DeletedDeployment
	if err := s.reader().Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

// GPUTimeSnapshot is what the card-time sampler reads once a minute: the
// cards every awake instance holds and every card of an online node, each
// in lean columns (one indexed read per table, no instance specs).
type GPUTimeSnapshot struct {
	Holdings []GPUHolding
	Cards    []GPU
	// NodeUUIDs names the online nodes by ID: GPU samples carry the UUID.
	NodeUUIDs map[uint]string
}

// GPUHolding is an instance holding cards: its deployment, node and the
// cards' identities (Instance.GPUIdents), with the deployment's dGPU memory
// limit (0 for whole cards).
type GPUHolding struct {
	DeploymentID   uint
	NodeID         uint
	Idents         []string
	VRAMLimitBytes int64
}

// GPUTimeSnapshot reads the card-time sampler's view.
func (s *Store) GPUTimeSnapshot() (GPUTimeSnapshot, error) {
	db := s.reader()
	var out GPUTimeSnapshot
	var nodes []Node
	if err := db.Select("ID", "UUID").Where("state = ?", NodeStateOnline).Find(&nodes).Error; err != nil {
		return out, err
	}
	online := make([]uint, 0, len(nodes))
	out.NodeUUIDs = make(map[uint]string, len(nodes))
	for _, n := range nodes {
		online = append(online, n.ID)
		out.NodeUUIDs[n.ID] = n.UUID
	}
	if len(online) > 0 {
		if err := db.Select("ID", "NodeID", "Ident", "Index", "UUID", "Name", "MemoryTotal", "Ownership", "HealthStatus", "ForeignProcesses", "SchedulingStopped", "SchedulingAllowedAt", "SchedulingHeldBy").
			Where("node_id IN ?", online).Find(&out.Cards).Error; err != nil {
			return out, err
		}
	}
	var insts []Instance
	if err := db.Select("DeploymentID", "NodeID", "GPUIdents").
		Where("state IN ? AND sleep_level = 0", []string{proto.InstRunning, proto.InstStarting}).Find(&insts).Error; err != nil {
		return out, err
	}
	depIDs := map[uint]bool{}
	for _, in := range insts {
		var idents []string
		if json.Unmarshal([]byte(in.GPUIdents), &idents) != nil || len(idents) == 0 {
			continue
		}
		out.Holdings = append(out.Holdings, GPUHolding{DeploymentID: in.DeploymentID, NodeID: in.NodeID, Idents: idents})
		depIDs[in.DeploymentID] = true
	}
	if len(depIDs) == 0 {
		return out, nil
	}
	ids := make([]uint, 0, len(depIDs))
	for id := range depIDs {
		ids = append(ids, id)
	}
	var deps []Deployment
	// Columns are named by field: GORM spells VRAMLimitBytes' column its own
	// way, and a hand-written name is how a query drifts from the entity.
	if err := db.Select("ID", "VRAMLimitBytes").Where("id IN ?", ids).Find(&deps).Error; err != nil {
		return out, err
	}
	limits := make(map[uint]int64, len(deps))
	for _, d := range deps {
		if d.VRAMLimitBytes > 0 {
			limits[d.ID] = d.VRAMLimitBytes
		}
	}
	for i := range out.Holdings {
		out.Holdings[i].VRAMLimitBytes = limits[out.Holdings[i].DeploymentID]
	}
	return out, nil
}
