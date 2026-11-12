package store

import "time"

// Model is an original model (source + repo id + revision). Weight/arch
// metadata is filled by the estimate/modelhub packages.
type Model struct {
	ID       uint   `gorm:"primaryKey"`
	Source   string `gorm:"size:32;index:idx_model_lookup,priority:1"`  // huggingface | modelscope | local | url
	SourceID string `gorm:"size:255;index:idx_model_lookup,priority:2"` // e.g. Qwen/Qwen3-8B
	Revision string `gorm:"size:64;index:idx_model_lookup,priority:3"`
	// SourceURL is the normalized HTTP directory the weights are served from,
	// for Source="url" only. A plain file server has no repository API, so the
	// directory cannot be derived from SourceID the way a HuggingFace or
	// ModelScope URL can: it is the one fact about such a model that nothing
	// else in the schema carries.
	SourceURL         string `gorm:"size:1024"`
	ArtifactSubfolder string `gorm:"size:512;index:idx_model_lookup,priority:4"`
	Name              string `gorm:"size:255"`
	Arch              string `gorm:"size:64"`
	QuantLabel        string `gorm:"size:32"`
	WeightBytes       int64
	Dtype             string `gorm:"size:16"`
	Meta              string `gorm:"type:text"` // free text: the repository's config.json verbatim
	ImageID           uint   `gorm:"index"`
	README            string `gorm:"type:text"` // free text
	LogoPath          string `gorm:"size:512"`
	OriginCreatedAt   *time.Time
	OriginUpdatedAt   *time.Time
	ContextLength     int64
	Precision         string `gorm:"size:32"`
	// JSON arrays containing the editable inference contract. The store keeps
	// them opaque so validation remains in the API/inferenceapi packages.
	Operations      string `gorm:"type:text"`                         // JSON []inferenceapi.Operation
	InputModalities string `gorm:"column:input_modalities;type:text"` // JSON []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// DisplayName is how messages name a model: its name, or its repository id.
func (m Model) DisplayName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.SourceID
}

// ModelFile is a model's files in one storage (path/state/progress). Three kinds
// of row share the table:
//   - local storage: one row per node, Path is the node's directory;
//   - shared storage (nfs, nfs_cache, shared_dir): one row with NodeID 0, Path
//     relative to the storage root;
//   - nfs_cache node cache: one row per node under the same StorageID, Path is
//     the node's cache directory.
type ModelFile struct {
	ID        uint `gorm:"primaryKey"`
	ModelID   uint `gorm:"index:idx_mf,unique,priority:1"`
	StorageID uint `gorm:"index:idx_mf,unique,priority:2"`
	NodeID    uint `gorm:"index:idx_mf,unique,priority:3"`
	// WriterNodeID is the node that writes a shared row (download or copy).
	WriterNodeID uint `gorm:"index"`
	// DownloadDeploymentID records who initiated the current download; zero is an independent user request.
	DownloadDeploymentID uint `gorm:"index;not null;default:0"`
	// LastUsedAt is when an instance last started or stopped on these files;
	// the node cache evicts the least recently used first.
	LastUsedAt           *time.Time
	Path                 string `gorm:"size:512"`
	Revision             string `gorm:"size:64"` // resolved commit/revision used by this node copy
	State                string `gorm:"size:32;index"`
	SizeBytes            int64
	Downloaded           int64
	TransferredBytes     int64
	RateBPS              int64
	RateLimit            int
	LastRateLimit        int
	Message              string `gorm:"size:512"`
	Acquisition          string `gorm:"size:16;index"`
	TransferID           string `gorm:"size:64;index"`
	TransferErrorCode    string `gorm:"size:64;not null;default:''"`
	TransferSourceNodeID uint   `gorm:"index"`
	// TransferSourceStorageID is the storage a copy reads from, with
	// TransferSourceNodeID naming the node for a local source storage.
	TransferSourceStorageID uint
	CreatedAt               time.Time
	UpdatedAt               time.Time
}
