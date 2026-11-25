package dto

import "time"

// UpdateStatus is GET /api/v1/updates: what the distribution channel offers
// next to what this server runs (ADR 0040). Available is false when no source
// served a valid index; Sources then says why for each of them.
type UpdateStatus struct {
	// Channel is nightly or release; ChannelEditable says the console can
	// switch it (the server keeps it in server.yaml).
	Channel         string `json:"channel"`
	ChannelEditable bool   `json:"channel_editable"`
	// AutoCheck is whether the server checks the channel by itself and
	// describes the fleet (node count, GPU models, agent versions) to it
	// while doing so; false means it asks only when an administrator does,
	// and then says nothing. Editable follows ChannelEditable.
	AutoCheck bool      `json:"auto_check"`
	CheckedAt time.Time `json:"checked_at"`
	Available bool      `json:"available"`
	// UnavailableReason says why no source served the channel's index when
	// Available is false: "unpublished" (a source answered that the channel
	// has no index — it is reachable and empty), "invalid" (a source served an
	// index that did not verify) or "unreachable" (none answered at all).
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	// Source is the base URL whose index the latest values come from.
	Source          string               `json:"source,omitempty"`
	Sources         []UpdateSourceStatus `json:"sources"`
	Server          ServerUpdate         `json:"server"`
	ImageCatalog    CatalogUpdate        `json:"image_catalog"`
	TemplateCatalog CatalogUpdate        `json:"template_catalog"`
}

// UpdateSourceStatus is how one distribution source answered.
type UpdateSourceStatus struct {
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// ServerBuildInfo describes this exact running build. Publication details are
// present only when a verified distribution index matches its full identity.
// Edition is set only for the running build (version.Edition).
type ServerBuildInfo struct {
	Version          string `json:"version"`
	Commit           string `json:"commit,omitempty"`
	BuiltAt          string `json:"built_at,omitempty"`
	BuildChannel     string `json:"build_channel,omitempty"`
	Edition          string `json:"edition,omitempty"`
	Kind             string `json:"kind"` // stable | prerelease | development | unknown
	PublishedVersion string `json:"published_version,omitempty"`
	PublishedAt      string `json:"published_at,omitempty"`
}

// ServerUpdate compares the running server with the channel's release.
// Installable says the release has an archive for this server's platform.
// SelfUpgrade says the console can upgrade this server in place; otherwise
// SelfUpgradeBlocker names why (no_service_manager, not_installed,
// not_installed_binary, cluster_member, not_writable) and the CLI is the way. InstallDir is
// the installation the server runs from (the directory of its server.yaml),
// for the commands the page offers; empty when it was started without one.
type ServerUpdate struct {
	CurrentBuild       ServerBuildInfo `json:"current_build"`
	Current            string          `json:"current"`
	Latest             string          `json:"latest,omitempty"`
	BuiltAt            string          `json:"built_at,omitempty"`
	UpdateAvailable    bool            `json:"update_available"`
	Installable        bool            `json:"installable"`
	SelfUpgrade        bool            `json:"self_upgrade"`
	SelfUpgradeBlocker string          `json:"self_upgrade_blocker,omitempty"`
	InstallDir         string          `json:"install_dir,omitempty"`
}

// ServerUpgradeStarted answers a console upgrade: the binary is swapped and
// the server is restarting into To; the next start confirms it or restores
// Backup.
type ServerUpgradeStarted struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Backup string `json:"backup"`
}

type ServerUpgradeSelection struct {
	Channel string `json:"channel"`
	Version string `json:"version"`
	SHA256  string `json:"sha256,omitempty"`
	Attempt string `json:"attempt,omitempty"`
}

type ServerVersionChoices struct {
	Channel  string                `json:"channel"`
	Platform string                `json:"platform"`
	Versions []ServerVersionChoice `json:"versions"`
}

type ServerVersionChoice struct {
	Latest  bool   `json:"latest"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	BuiltAt string `json:"built_at"`
	SHA256  string `json:"sha256,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type ServerUpgradeProgress struct {
	Attempt string `json:"attempt,omitempty"`
	Phase   string `json:"phase"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
}

// CatalogUpdate compares an official catalog in force with the published one.
// Versions are for display; UpdateAvailable compares identities. Compatible
// requires a catalog this server can read; CheckError distinguishes a failed
// check from a package requiring a newer server.
type CatalogUpdate struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	// Uploaded: the catalog in force was uploaded by hand (the image catalog
	// only). The channel's catalog is then not an update of it.
	Uploaded bool `json:"uploaded,omitempty"`
	// ReplacesUpload: applying the channel's catalog would replace an
	// uploaded one, which POST /images/sync does only with replace_uploaded.
	ReplacesUpload bool `json:"replaces_upload,omitempty"`
	Compatible     bool `json:"compatible"`
	// CheckError means compatibility could not be checked, rather than that
	// the package requires a newer server. Other update offers remain usable.
	CheckError *ProductMessage `json:"check_error,omitempty"`
}

// OfficialInferenceCatalogResult is the answer to applying an official
// inference template catalog, from the channel or an upload.
type OfficialInferenceCatalogResult struct {
	Models        int    `json:"models"`
	PackageDigest string `json:"package_digest"`
	Version       string `json:"version"`
	// Skipped lists what this server left out of the package because it
	// cannot read it; a row it already had for one of them is unchanged.
	Skipped []OfficialInferenceCatalogSkipped `json:"skipped,omitempty"`
}

// OfficialInferenceCatalogSkipped is a model, or one spec of a model when
// Spec is set, of an official catalog that needs a newer server.
type OfficialInferenceCatalogSkipped struct {
	ModelID string `json:"model_id"`
	Spec    string `json:"spec,omitempty"`
	Reason  string `json:"reason"`
}
