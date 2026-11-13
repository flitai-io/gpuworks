package cloud

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// CloudHost preserves the initial discovered snapshot, including reservations,
// independently of the agent's live Node/GPU reports.
//
//nolint:revive // GORM derives the persisted cloud_hosts table name from this type.
type CloudHost struct {
	ID                    uint   `gorm:"primaryKey"`
	ProviderID            uint   `gorm:"uniqueIndex:idx_cloud_host"`
	ExternalID            string `gorm:"uniqueIndex:idx_cloud_host"`
	Snapshot              string `gorm:"type:text"` // JSON provider/zstack/cloud/inventory.CloudResource; the whole installation-time inventory snapshot.
	InitialCPUReserved    *int64
	InitialMemoryReserved *int64
	NodeUUID              string `gorm:"index"`
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

const (
	CloudLeaseReservePending = "reserve_pending"
	CloudLeaseReserved       = "reserved"
	CloudLeaseReleasePending = "release_pending"
	CloudLeaseReleased       = "released"
	CloudLeaseFailed         = "failed"
	// CloudLeaseUnknown is the legacy value for an unconfirmed reservation.
	CloudLeaseUnknown = "unknown"
)

// CloudResourceLease tracks GPUWorks resource ownership from a pending
// reservation through its confirmed release. OperationID deduplicates requests
// for the same ownership; pending records protect resources across restarts.
//
//nolint:revive // GORM derives the persisted cloud_resource_leases table name from this type.
type CloudResourceLease struct {
	ID           uint   `gorm:"primaryKey"`
	OperationID  string `gorm:"uniqueIndex"`
	ProviderID   uint   `gorm:"index"`
	CloudHostID  uint   `gorm:"index"`
	NodeUUID     string `gorm:"size:64;index"`
	InstanceUUID string `gorm:"size:64;index"`
	ResourceType string `gorm:"size:32;index"`
	ResourceID   string `gorm:"size:128"`
	Amount       int64
	Unit         string `gorm:"size:16"`
	State        string `gorm:"size:32;index"`
	ErrorCode    string `gorm:"size:64"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (l CloudResourceLease) IsReservationPending() bool {
	return l.State == CloudLeaseReservePending || l.State == CloudLeaseUnknown
}

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

var ErrNotFound = errors.New("not found")

var ErrCloudConflict = errors.New("Cloud identity or idempotency key conflict")

func (s *Store) CloudHosts(providerID uint) ([]CloudHost, error) {
	var hosts []CloudHost
	err := s.db.Where("provider_id = ?", providerID).Order("id").Find(&hosts).Error
	return hosts, err
}

func (s *Store) CloudHostByNodeUUID(nodeUUID string) (*CloudHost, error) {
	var host CloudHost
	err := s.db.Where("node_uuid = ?", nodeUUID).First(&host).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &host, err
}

func (s *Store) CloudResourceLeaseByID(id uint) (*CloudResourceLease, error) {
	var lease CloudResourceLease
	err := s.db.First(&lease, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &lease, err
}

func (s *Store) CloudResourceLeaseByOperation(operationID string) (*CloudResourceLease, error) {
	var lease CloudResourceLease
	err := s.db.Where("operation_id = ?", operationID).First(&lease).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &lease, err
}

func (s *Store) CloudResourceLeasesForInstance(instanceUUID string) ([]CloudResourceLease, error) {
	var leases []CloudResourceLease
	err := s.db.Where("instance_uuid = ? AND state <> ?", instanceUUID, CloudLeaseReleased).Order("id").Find(&leases).Error
	return leases, err
}

func (s *Store) UnresolvedCloudResourceLeases() ([]CloudResourceLease, error) {
	var leases []CloudResourceLease
	err := s.db.Where("state IN ?", []string{CloudLeaseReservePending, CloudLeaseUnknown, CloudLeaseReleasePending}).Order("id").Find(&leases).Error
	return leases, err
}

func (s *Store) ActiveCloudResourceLeasesForResource(providerID uint, resourceType, resourceID string) ([]CloudResourceLease, error) {
	var leases []CloudResourceLease
	err := s.db.Where("provider_id = ? AND resource_type = ? AND resource_id = ? AND state <> ?",
		providerID, resourceType, resourceID, CloudLeaseReleased).Order("id").Find(&leases).Error
	return leases, err
}

// CreateCloudResourceLease is idempotent on OperationID. A replay with a
// different resource request is rejected instead of silently changing the
// amount already sent to Cloud.
func (s *Store) CreateCloudResourceLease(lease *CloudResourceLease) (*CloudResourceLease, error) {
	if lease.OperationID == "" || lease.ProviderID == 0 || lease.CloudHostID == 0 || lease.Amount <= 0 || lease.ResourceType == "" {
		return nil, errors.New("invalid Cloud resource lease")
	}
	var out CloudResourceLease
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing CloudResourceLease
		err := tx.Where("operation_id = ?", lease.OperationID).First(&existing).Error
		if err == nil {
			if existing.ProviderID != lease.ProviderID || existing.CloudHostID != lease.CloudHostID || existing.InstanceUUID != lease.InstanceUUID || existing.ResourceType != lease.ResourceType || existing.ResourceID != lease.ResourceID || existing.Amount != lease.Amount || existing.Unit != lease.Unit {
				return ErrCloudConflict
			}
			out = existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.First(&CloudHost{}, lease.CloudHostID).Error; err != nil {
			return err
		}
		if lease.State == "" || lease.IsReservationPending() {
			lease.State = CloudLeaseReservePending
		}
		if err := tx.Create(lease).Error; err != nil {
			return err
		}
		out = *lease
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) DeletePendingCloudResourceLease(id uint) error {
	if id == 0 {
		return errors.New("invalid Cloud resource lease ID")
	}
	result := s.db.Where("id = ? AND state IN ?", id, []string{CloudLeaseReservePending, CloudLeaseUnknown}).Delete(&CloudResourceLease{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrCloudConflict
	}
	return nil
}

func (s *Store) SetCloudResourceLeaseState(id uint, state, errorCode string) error {
	if id == 0 || state == "" {
		return errors.New("invalid Cloud resource lease state")
	}
	result := s.db.Model(&CloudResourceLease{}).Where("id = ?", id).Updates(map[string]any{"state": state, "error_code": errorCode})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
