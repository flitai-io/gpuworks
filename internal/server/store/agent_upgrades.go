package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

var ErrAgentUpgradeConflict = errors.New("another agent upgrade is active or unconfirmed")
var ErrAgentUpgradeRequestChanged = errors.New("idempotency key was used for a different upgrade request")

// AgentUpgradeRun retains user intent and immutable target artifacts. Counts
// and the aggregate state are derived from its items.
type AgentUpgradeRun struct {
	ID                 uint `gorm:"primaryKey"`
	RequestedBy        string
	CreatedAt          time.Time
	IdempotencyKey     string `gorm:"uniqueIndex;size:128"`
	RequestFingerprint string `gorm:"size:64"`
	TargetVersion      string
	SourceRunID        uint
	StopRequestedAt    *time.Time
	StopRequestedBy    string
	PauseReason        string
	InterruptedReason  string
}

// AgentUpgradeItem records one attempt; VerifiedAt resolves an unconfirmed
// reservation without rewriting the historical timeout outcome.
type AgentUpgradeItem struct {
	ID                uint   `gorm:"primaryKey"`
	RunID             uint   `gorm:"index;uniqueIndex:idx_agent_upgrade_member"`
	NodeUUID          string `gorm:"index;uniqueIndex:idx_agent_upgrade_member;size:128"`
	Name              string
	FromVersion       string
	OS                string
	Arch              string
	Transport         string
	ArtifactName      string
	ArtifactSHA256    string `gorm:"size:64"`
	ArtifactSize      int64
	Phase             string `gorm:"index;size:32"`
	Reason            string
	Error             string
	BaselineSessionID string
	StartedAt         *time.Time
	DeadlineAt        *time.Time
	FinishedAt        *time.Time
	RegisteredAt      *time.Time
	ObservedVersion   string
	VerifiedAt        *time.Time
	VerifiedBy        string
	VerificationCode  string
	VerificationNote  string
}

func createAgentUpgradeTables(db *gorm.DB) error {
	if err := db.AutoMigrate(&AgentUpgradeRun{}); err != nil {
		return err
	}
	return db.AutoMigrate(&AgentUpgradeItem{})
}

func (s *Store) AgentUpgradeRun(id uint) (*AgentUpgradeRun, error) {
	var r AgentUpgradeRun
	err := s.reader().First(&r, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &r, err
}

func (s *Store) AgentUpgradeRuns() ([]AgentUpgradeRun, error) {
	var runs []AgentUpgradeRun
	err := s.reader().Order("id desc").Find(&runs).Error
	return runs, err
}

func (s *Store) AgentUpgradeItems(runID uint) ([]AgentUpgradeItem, error) {
	var items []AgentUpgradeItem
	q := s.reader().Order("id")
	if runID != 0 {
		q = q.Where("run_id = ?", runID)
	}
	err := q.Find(&items).Error
	return items, err
}

func (s *Store) AgentUpgradeItem(runID uint, uuid string) (*AgentUpgradeItem, error) {
	var item AgentUpgradeItem
	err := s.reader().Where("run_id = ? AND node_uuid = ?", runID, uuid).First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &item, err
}

// upgradeOutstanding selects the attempts that hold the platform's single
// upgrade slot: those in flight, and those unconfirmed at heldSince or later
// and not verified since. An unconfirmed attempt holds it only for a while
// (the caller's policy): its agent has settled by then, confirmed or rolled
// back, so another node may go ahead; the attempt itself stays unverified
// until its node says how it ended. The zero heldSince holds every
// unverified one.
func upgradeOutstanding(q *gorm.DB, heldSince time.Time) *gorm.DB {
	return q.Where("phase IN ? OR (phase = ? AND verified_at IS NULL AND finished_at >= ?)", []string{"transferring", "waiting_registration", "observing_recovery"}, "unconfirmed", heldSince)
}

// AgentUpgradeBlocker is the first attempt that holds the slot
// (upgradeOutstanding), nil when none does.
func (s *Store) AgentUpgradeBlocker(heldSince time.Time) (*AgentUpgradeItem, error) {
	var item AgentUpgradeItem
	err := upgradeOutstanding(s.reader(), heldSince).Order("id").First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &item, err
}

// CreateAgentUpgrade atomically reserves the global upgrade slot and, when
// continuing a paused run, closes its old queue before creating the new one.
// heldSince is as for upgradeOutstanding.
func (s *Store) CreateAgentUpgrade(run *AgentUpgradeRun, items []AgentUpgradeItem, now, heldSince time.Time) (*AgentUpgradeRun, error) {
	var saved AgentUpgradeRun
	err := s.db.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("idempotency_key = ?", run.IdempotencyKey).First(&saved).Error
		if err == nil {
			if saved.RequestFingerprint != run.RequestFingerprint {
				return ErrAgentUpgradeRequestChanged
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var count int64
		if err := upgradeOutstanding(tx.Model(&AgentUpgradeItem{}), heldSince).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrAgentUpgradeConflict
		}
		if run.SourceRunID != 0 {
			var source AgentUpgradeRun
			if err := tx.First(&source, run.SourceRunID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
		}
		var queues []AgentUpgradeItem
		if err := tx.Where("phase = ?", "queued").Find(&queues).Error; err != nil {
			return err
		}
		if len(queues) > 0 {
			if run.SourceRunID == 0 {
				return ErrAgentUpgradeConflict
			}
			var source AgentUpgradeRun
			if err := tx.First(&source, run.SourceRunID).Error; err != nil {
				return err
			}
			if source.PauseReason == "" && source.InterruptedReason == "" {
				return ErrAgentUpgradeConflict
			}
			for _, item := range queues {
				if item.RunID != run.SourceRunID {
					return ErrAgentUpgradeConflict
				}
			}
			if err := tx.Model(&AgentUpgradeItem{}).Where("run_id = ? AND phase = ?", source.ID, "queued").Updates(map[string]any{"phase": "cancelled", "reason": "superseded", "finished_at": now}).Error; err != nil {
				return err
			}
			if err := tx.Model(&AgentUpgradeRun{}).Where("id = ? AND stop_requested_at IS NULL", source.ID).Updates(map[string]any{"stop_requested_at": now, "stop_requested_by": run.RequestedBy}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(run).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].RunID = run.ID
		}
		if len(items) > 0 {
			if err := tx.Create(&items).Error; err != nil {
				return err
			}
		}
		saved = *run
		return nil
	})
	if err == nil {
		s.Bus.PublishChange("agent_upgrade", uintKey(saved.ID), "created")
	}
	return &saved, err
}

// ClaimAgentUpgrade starts a queued attempt when nothing holds the slot
// (heldSince as for upgradeOutstanding).
func (s *Store) ClaimAgentUpgrade(itemID uint, session string, now, deadline, heldSince time.Time) (bool, error) {
	claimed := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var item AgentUpgradeItem
		if err := tx.First(&item, itemID).Error; err != nil {
			return err
		}
		var run AgentUpgradeRun
		if err := tx.First(&run, item.RunID).Error; err != nil {
			return err
		}
		if run.StopRequestedAt != nil || run.PauseReason != "" || run.InterruptedReason != "" {
			return nil
		}
		var count int64
		if err := upgradeOutstanding(tx.Model(&AgentUpgradeItem{}), heldSince).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return nil
		}
		r := tx.Model(&AgentUpgradeItem{}).Where("id = ? AND phase = ?", itemID, "queued").Updates(map[string]any{"phase": "transferring", "baseline_session_id": session, "started_at": now, "deadline_at": deadline})
		claimed = r.RowsAffected == 1
		return r.Error
	})
	if err == nil && claimed {
		s.Bus.PublishChange("agent_upgrade", "", "transferring")
	}
	return claimed, err
}

// UpdateAgentUpgradeItem is a compare-and-set transition, so late workers
// cannot reopen finished history or overwrite another attempt.
func (s *Store) UpdateAgentUpgradeItem(id uint, from []string, values map[string]any) (bool, error) {
	r := s.db.Model(&AgentUpgradeItem{}).Where("id = ? AND phase IN ?", id, from).Updates(values)
	if r.Error == nil && r.RowsAffected > 0 {
		s.Bus.PublishChange("agent_upgrade", "", "updated")
	}
	return r.RowsAffected > 0, r.Error
}

func (s *Store) PauseAgentUpgrade(id uint, reason string) error {
	err := s.db.Model(&AgentUpgradeRun{}).Where("id = ?", id).Update("pause_reason", reason).Error
	if err == nil {
		s.Bus.PublishChange("agent_upgrade", uintKey(id), "paused")
	}
	return err
}

func (s *Store) StopAgentUpgrade(id uint, now time.Time, actor string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var run AgentUpgradeRun
		if err := tx.First(&run, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Model(&AgentUpgradeRun{}).Where("id = ? AND stop_requested_at IS NULL", run.ID).Updates(map[string]any{"stop_requested_at": now, "stop_requested_by": actor}).Error; err != nil {
			return err
		}
		return tx.Model(&AgentUpgradeItem{}).Where("run_id = ? AND phase = ?", id, "queued").Updates(map[string]any{"phase": "cancelled", "reason": "stopped", "finished_at": now}).Error
	})
	if err == nil {
		s.Bus.PublishChange("agent_upgrade", uintKey(id), "stopped")
	}
	return err
}

func (s *Store) VerifyAgentUpgrade(id uint, at time.Time, by, code, note string) (bool, error) {
	r := s.db.Model(&AgentUpgradeItem{}).Where("id = ? AND phase = ? AND verified_at IS NULL", id, "unconfirmed").Updates(map[string]any{"verified_at": at, "verified_by": by, "verification_code": code, "verification_note": note})
	if r.Error == nil && r.RowsAffected > 0 {
		s.Bus.PublishChange("agent_upgrade", "", "verified")
	}
	return r.RowsAffected > 0, r.Error
}

// RecoverAgentUpgrades never repeats I/O that may already have installed a
// binary. The runner observes these rows against their persisted deadline.
func (s *Store) RecoverAgentUpgrades(target string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&AgentUpgradeItem{}).Where("phase IN ?", []string{"transferring", "waiting_registration"}).Update("phase", "observing_recovery").Error; err != nil {
			return err
		}
		// Older servers paused a batch whenever one node's attempt went
		// unconfirmed, and left it for an administrator. An unconfirmed
		// attempt no longer pauses anything, so those batches go on.
		if err := tx.Model(&AgentUpgradeRun{}).Where("pause_reason = ?", "confirmation_required").Update("pause_reason", "").Error; err != nil {
			return err
		}
		unfinished := tx.Model(&AgentUpgradeItem{}).Select("run_id").Where("phase IN ? OR (phase = ? AND verified_at IS NULL)", []string{"queued", "observing_recovery"}, "unconfirmed")
		return tx.Model(&AgentUpgradeRun{}).Where("target_version <> ? AND id IN (?)", target, unfinished).Update("interrupted_reason", "target_changed").Error
	})
}

func (s *Store) AgentUpgradePendingItems() ([]AgentUpgradeItem, error) {
	var items []AgentUpgradeItem
	err := s.reader().Where("phase IN ? OR (phase = ? AND verified_at IS NULL)", []string{"queued", "transferring", "waiting_registration", "observing_recovery"}, "unconfirmed").Order("id").Find(&items).Error
	return items, err
}
func (s *Store) AgentUpgradeQueuedBlocker() (*AgentUpgradeItem, error) {
	var item AgentUpgradeItem
	err := s.reader().Where("phase = ?", "queued").Order("id").First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &item, err
}

func (s *Store) InterruptAgentUpgrade(id uint, reason string) error {
	err := s.db.Model(&AgentUpgradeRun{}).Where("id = ?", id).Update("interrupted_reason", reason).Error
	if err == nil {
		s.Bus.PublishChange("agent_upgrade", uintKey(id), "interrupted")
	}
	return err
}
