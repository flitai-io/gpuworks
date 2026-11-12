package store

import "time"

// ImageSeries is the stable, user-facing family of runtime images. Concrete
// tags are stored as Image rows in image_versions.
type ImageSeries struct {
	ID              uint   `gorm:"primaryKey"`
	Name            string `gorm:"size:128;uniqueIndex"`
	Engine          string `gorm:"size:32;index"`
	Purpose         string `gorm:"size:16;index"` // inference | evaluation
	Framework       string `gorm:"size:32;index"` // cuda | cann | rocm | hggc | dtk
	CommonArgs      string `gorm:"type:text"`     // JSON []manifest.CommonArg
	CommonEnv       string `gorm:"type:text"`     // JSON []manifest.CommonEnv
	Source          string `gorm:"size:16;index"` // official | community | custom
	DeprecatedSince string `gorm:"size:32"`
	Notes           string `gorm:"type:text"` // free text
	Enabled         bool   `gorm:"index"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Image is a catalog entry for a runtime image. The image itself lives in the
// node's container runtime; this table owns only distribution metadata and
// compatibility facts, never launch configuration (ADR 0031).
type Image struct {
	ID                uint   `gorm:"primaryKey"`
	SeriesID          uint   `gorm:"index"`
	SeriesName        string `gorm:"-" json:"-"`
	Purpose           string `gorm:"-" json:"-"`
	Name              string `gorm:"size:128"`
	Engine            string `gorm:"size:32;index"`
	EngineVersion     string `gorm:"size:64"`
	Framework         string `gorm:"size:32;index"`
	FrameworkVersion  string `gorm:"size:64"`
	Variant           string `gorm:"size:32"`
	Arch              string `gorm:"size:16"`
	ImageRef          string `gorm:"uniqueIndex;size:512"`
	Source            string `gorm:"size:16;index"` // official | community | custom
	Deprecated        bool   `gorm:"index"`
	Enabled           bool   `gorm:"index"`
	SizeBytes         int64
	VerificationState string `gorm:"size:16;index"` // verified | failed | unknown
	VerificationError string `gorm:"size:512"`
	Notes             string `gorm:"type:text"` // free text
	OfflineBlobPath   string `gorm:"size:512"`
	OfflineBlobSHA256 string `gorm:"size:64"`
	OfflineBlobBytes  int64
	DeprecatedSince   string `gorm:"size:32"`
	RegistrySources   string `gorm:"type:text"` // JSON []string of registry references and archive URLs
	// RollingTag records that the tag's content moves upstream (latest,
	// nightly, ...). A system row takes it from the manifest; a community or
	// custom row has it when the operator declares it or the tag is one of the
	// rolling names (community.IsRollingTag). Automatic image selection skips
	// such rows; a node can re-pull them.
	RollingTag   bool `gorm:"index"`
	UpstreamGone bool `gorm:"index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ImageVersion is the descriptive name for Image in new code. The alias keeps
// existing store/API callers source-compatible while they are renamed.
type ImageVersion = Image

func (Image) TableName() string { return "image_versions" }

// ImageNode is the node-local presence and transfer state for a catalog image.
type ImageNode struct {
	// RuntimeSizeBytes is a nullable runtime observation, separate from transfer SizeBytes.
	RuntimeSizeBytes *int64
	ID               uint `gorm:"primaryKey"`
	ImageVersionID   uint `gorm:"index:idx_image_node,unique,priority:1"`
	// ImageID is a source-compatibility field for old callers. Store methods
	// mirror it to ImageVersionID; it is not persisted in the new schema.
	ImageID          uint   `gorm:"-" json:"-"`
	NodeID           uint   `gorm:"index:idx_image_node,unique,priority:2"`
	State            string `gorm:"size:16;index"` // absent|pulling|present|syncing|failed
	SizeBytes        int64
	TransferredBytes int64
	RateBPS          int64
	RateLimitKiB     int64  `gorm:"column:rate_limit_kib"`
	LastRateLimitKiB int64  `gorm:"column:last_rate_limit_kib"`
	TransferID       string `gorm:"size:64;index"`
	// LastTransferID fences delayed terminal reports after the active ID clears.
	LastTransferID    string `gorm:"size:64;not null;default:''"`
	TransferErrorCode string `gorm:"size:64;not null;default:''"`
	SourceNodeID      uint   `gorm:"index"`
	Source            string `gorm:"size:32"` // peer | offline | quay | huaweicloud | direct
	Message           string `gorm:"size:512"`
	ImageDigestID     string `gorm:"size:128"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
