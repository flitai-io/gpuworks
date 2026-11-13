package store

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// NoticeRead records that a user has read a notice of the console's notice
// bell (a publisher's notice or the installation's own). It is the one fact
// about notices kept: the notices themselves are fetched again from the
// distribution channel, and a mark whose notice is gone is removed by
// DeleteNoticeReadsExcept.
type NoticeRead struct {
	UserID   uint      `gorm:"primaryKey;autoIncrement:false"`
	NoticeID string    `gorm:"primaryKey;size:64"`
	ReadAt   time.Time `gorm:"not null"`
}

// createNoticeReads adds the notice read marks table.
func createNoticeReads(db *gorm.DB) error {
	return db.AutoMigrate(&NoticeRead{})
}

// NoticeReads lists the notices a user has read.
func (s *Store) NoticeReads(userID uint) (map[string]bool, error) {
	var ids []string
	if err := s.reader().Model(&NoticeRead{}).Where("user_id = ?", userID).Pluck("notice_id", &ids).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// MarkNoticesRead records that a user has read the notices; marking one
// again keeps the first time.
func (s *Store) MarkNoticesRead(userID uint, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	rows := make([]NoticeRead, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, NoticeRead{UserID: userID, NoticeID: id, ReadAt: at.UTC()})
	}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}

// DeleteNoticeReadsExcept removes the read marks of every notice not in
// keep, once the channel no longer serves it.
func (s *Store) DeleteNoticeReadsExcept(keep []string) error {
	q := s.db.Where("1 = 1")
	if len(keep) > 0 {
		q = s.db.Where("notice_id NOT IN ?", keep)
	}
	return q.Delete(&NoticeRead{}).Error
}

// UsageReportLastDay is the UTC day (YYYY-MM-DD) of the last usage report
// delivered, or of the day before the program started; "" before either.
func (s *Store) UsageReportLastDay() (string, error) {
	raw, err := s.LookupSetting(SettingUsageReportLastDay)
	if err != nil || raw == "" {
		return "", err
	}
	var day string
	if err := json.Unmarshal([]byte(raw), &day); err != nil {
		return "", nil //nolint:nilerr // An invalid optional last-report marker is treated as not yet reported.
	}
	return day, nil
}

// SetUsageReportLastDay records the last day reported.
func (s *Store) SetUsageReportLastDay(day string) error {
	raw, err := json.Marshal(day)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingUsageReportLastDay, string(raw))
}

// FleetVendors lists the accelerator vendors the fleet's nodes have, from
// the gpu_vendor index.
func (s *Store) FleetVendors() ([]string, error) {
	var vendors []string
	err := s.reader().Model(&Node{}).Distinct("gpu_vendor").Where("gpu_vendor <> ''").Order("gpu_vendor").Pluck("gpu_vendor", &vendors).Error
	return vendors, err
}
