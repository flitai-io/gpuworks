package store

import (
	"time"

	"gorm.io/gorm"
)

// CardHealthEpisode is one stretch in which a card check warned or failed
// (cardhealth.Tracker). Only problems are stored: a check's passing states
// live in memory and come again with every status report.
type CardHealthEpisode struct {
	ID     uint `gorm:"primaryKey"`
	NodeID uint `gorm:"not null;index:idx_card_health_node_started,priority:1"`
	// CardIdent is the card's identity (accelerator.CardIdent), so a card
	// replaced in the same slot is another card with its own history.
	CardIdent string `gorm:"size:128;not null"`
	// CheckID is an accelerator card check id.
	CheckID string `gorm:"size:32;not null"`
	// Level is the check status: warn or fail (proto.CheckWarn, CheckFail).
	Level string `gorm:"size:8;not null"`
	// Reason is the code the check gave, UPPER_SNAKE (rows written before
	// that were rewritten by migration 216).
	Reason    string    `gorm:"size:64;not null;default:''"`
	StartedAt time.Time `gorm:"not null;index:idx_card_health_node_started,priority:2"`
	// EndedAt is when the check started passing for good; nil while open.
	EndedAt    *time.Time `gorm:"index"`
	LastSeenAt time.Time  `gorm:"not null"`
	// Count is how many runs of the check found the problem.
	Count int `gorm:"not null;default:1"`
	// Flapping: the problem recurred often within a day.
	Flapping bool `gorm:"not null;default:false"`
	// FirstEvidence and LatestEvidence are what the first and the latest run
	// read, in the text form of a card's health message
	// (accelerator.FormatHealthEvidence), at most 1 KiB each.
	FirstEvidence  string `gorm:"type:text;not null;default:''"` // free text: accelerator.FormatHealthEvidence, not JSON
	LatestEvidence string `gorm:"type:text;not null;default:''"` // free text: accelerator.FormatHealthEvidence, not JSON
}

func createCardHealthEpisodes(db *gorm.DB) error {
	return db.AutoMigrate(&CardHealthEpisode{})
}

// OpenCardHealthEpisodes are the episodes not yet closed.
func (s *Store) OpenCardHealthEpisodes() ([]CardHealthEpisode, error) {
	var rows []CardHealthEpisode
	err := s.reader().Where("ended_at IS NULL").Find(&rows).Error
	return rows, err
}

// SaveCardHealthEpisodes writes episodes in one transaction: a row without an
// id is inserted and gets one, the others are updated whole. Rows of a node
// that no longer exists are skipped.
func (s *Store) SaveCardHealthEpisodes(rows []*CardHealthEpisode) error {
	if len(rows) == 0 {
		return nil
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		// A node deleted since the episode was observed takes its
		// episodes with it; writing them now would bring them back.
		ids := make([]uint, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.NodeID)
		}
		var live []uint
		if err := tx.Model(&Node{}).Where("id IN ?", ids).Pluck("id", &live).Error; err != nil {
			return err
		}
		exists := make(map[uint]bool, len(live))
		for _, id := range live {
			exists[id] = true
		}
		for _, row := range rows {
			if !exists[row.NodeID] {
				continue
			}
			if row.ID == 0 {
				if err := tx.Create(row).Error; err != nil {
					return err
				}
				continue
			}
			if err := tx.Select("*").Save(row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteCardHealthEpisodesEndedBefore deletes at most limit episodes that
// closed before the time, and says how many it deleted.
func (s *Store) DeleteCardHealthEpisodesEndedBefore(before time.Time, limit int) (int, error) {
	res := s.db.Where("id IN (?)", s.db.Model(&CardHealthEpisode{}).Select("id").
		Where("ended_at IS NOT NULL AND ended_at < ?", before.UTC()).Limit(limit)).
		Delete(&CardHealthEpisode{})
	return int(res.RowsAffected), res.Error
}

// CardHealthEpisodeQuery selects a node's episodes, optionally one card's or
// one check's, a page at a time.
type CardHealthEpisodeQuery struct {
	NodeID        uint
	CardIdent     string
	CheckID       string
	Offset, Limit int
	StartedAfter  time.Time
}

// CardHealthEpisodes returns a page of episodes, open ones first and then the
// newest, and how many match in all.
func (s *Store) CardHealthEpisodes(q CardHealthEpisodeQuery) ([]CardHealthEpisode, int64, error) {
	db := s.reader().Model(&CardHealthEpisode{}).Where("node_id = ?", q.NodeID)
	if q.CardIdent != "" {
		db = db.Where("card_ident = ?", q.CardIdent)
	}
	if q.CheckID != "" {
		db = db.Where("check_id = ?", q.CheckID)
	}
	if !q.StartedAfter.IsZero() {
		db = db.Where("ended_at IS NULL OR ended_at >= ?", q.StartedAfter.UTC())
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []CardHealthEpisode
	err := db.Order("ended_at IS NULL DESC").Order("started_at DESC").Order("id DESC").
		Offset(q.Offset).Limit(q.Limit).Find(&rows).Error
	return rows, total, err
}
