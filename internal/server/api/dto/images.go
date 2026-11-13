package dto

type Image struct {
	ID                uint     `json:"id"`
	SeriesID          uint     `json:"series_id"`
	Purpose           string   `json:"purpose"`
	Name              string   `json:"name"`
	Engine            string   `json:"engine"`
	EngineVersion     string   `json:"engine_version,omitempty"`
	Framework         string   `json:"framework"`
	FrameworkVersion  string   `json:"framework_version,omitempty"`
	DriverRequirement string   `json:"driver_requirement,omitempty"`
	Arch              string   `json:"arch,omitempty"`
	ImageRef          string   `json:"image_ref"`
	Source            string   `json:"source"`
	Deprecated        bool     `json:"deprecated"`
	Enabled           bool     `json:"enabled"`
	Notes             string   `json:"notes,omitempty"`
	SizeBytes         int64    `json:"size_bytes,omitempty"`
	DeprecatedSince   string   `json:"deprecated_since,omitempty"`
	RegistrySources   []string `json:"registry_sources,omitempty"`
	RollingTag        bool     `json:"rolling_tag"`
	UpstreamGone      bool     `json:"upstream_gone"`
	EffectiveEnabled  bool     `json:"effective_enabled"`
	// ReferenceCount is how many deployments, unfinished instances, and
	// models name this image; a deprecated system image shows it.
	ReferenceCount int `json:"reference_count"`
	// PresentNodes and TotalNodes summarize presence. Nodes, one row per
	// node, is filled only where one version is shown (the series detail and
	// the responses of per-version writes), never on the catalog-wide list.
	// LocalRuntimeSizeBytes is a subtotal of measured present copies.
	// LocalSizeKnownNodes distinguishes a complete total from partial/unknown sizes.
	LocalRuntimeSizeBytes int64       `json:"local_runtime_size_bytes"`
	LocalSizeKnownNodes   int         `json:"local_size_known_nodes"`
	PresentNodes          int         `json:"present_nodes"`
	TotalNodes            int         `json:"total_nodes"`
	Nodes                 []ImageNode `json:"nodes,omitempty"`
}

// ImageSeries is the list/detail representation used by the image catalog.
// Versions are populated on the detail endpoint and omitted from the list.
type ImageSeries struct {
	ID           uint             `json:"id"`
	Name         string           `json:"name"`
	Engine       string           `json:"engine,omitempty"`
	Purpose      string           `json:"purpose"`
	Framework    string           `json:"framework,omitempty"`
	CommonArgs   []map[string]any `json:"common_args,omitempty"`
	CommonEnv    []map[string]any `json:"common_env,omitempty"`
	Source       string           `json:"source"`
	Notes        string           `json:"notes,omitempty"`
	Enabled      bool             `json:"enabled"`
	VersionCount int              `json:"version_count"`
	PresentNodes int              `json:"present_nodes"`
	TotalNodes   int              `json:"total_nodes"`
	Versions     []Image          `json:"versions,omitempty"`
}

type ImageNode struct {
	NodeUUID string `json:"node_uuid"`
	// NodeName and NodeState are the node's display name and state, so a
	// client lists an image's nodes without fetching every node.
	NodeName          string `json:"node_name"`
	NodeState         string `json:"node_state"`
	State             string `json:"state"`
	SizeBytes         int64  `json:"size_bytes,omitempty"`
	TransferredBytes  int64  `json:"transferred_bytes,omitempty"`
	RateBPS           int64  `json:"rate_bps,omitempty"`
	RateLimitKiB      int64  `json:"rate_limit_kib_s,omitempty"`
	LastRateLimitKiB  int64  `json:"last_rate_limit_kib_s,omitempty"`
	TransferID        string `json:"transfer_id,omitempty"`
	TransferErrorCode string `json:"transfer_error_code,omitempty"`
	SourceNodeUUID    string `json:"source_node_uuid,omitempty"`
	SourceNodeName    string `json:"source_node_name,omitempty"`
	Source            string `json:"source,omitempty"`
	Message           string `json:"message,omitempty"`
	// ImageDigestID is the runtime image id the node last reported, so a
	// rolling tag's re-pull shows whether the content changed.
	ImageDigestID string `json:"image_digest_id,omitempty"`
	// UpdatedAt is when the node's row last changed (RFC 3339), empty when
	// the node has no row.
	UpdatedAt string `json:"updated_at"`
}

// ImageCommonArg and ImageCommonEnv are a series' suggested launch flags and
// environment, as a community/custom series is written with them.
type ImageCommonArg struct {
	Flag        string `json:"flag" yaml:"flag"`
	Value       string `json:"value,omitempty" yaml:"value"`
	Description string `json:"description,omitempty" yaml:"description"`
}

type ImageCommonEnv struct {
	Key         string `json:"key" yaml:"key"`
	Value       string `json:"value,omitempty" yaml:"value"`
	Description string `json:"description,omitempty" yaml:"description"`
}

// ImageManifestImportResult answers "check for updates" and a manifest
// upload: what the import changed, and the system series as they now are.
type ImageManifestImportResult struct {
	CatalogVersion string                `json:"catalog_version"`
	Created        int                   `json:"created"`
	Updated        int                   `json:"updated"`
	Deleted        int                   `json:"deleted"`
	Deprecated     int                   `json:"deprecated"`
	Skipped        []ImageImportConflict `json:"skipped"`
	Series         []ImageSeries         `json:"series"`
}

// ImageImportConflict is a manifest entry left out because a community or
// custom row owns its name or ref.
type ImageImportConflict struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}
