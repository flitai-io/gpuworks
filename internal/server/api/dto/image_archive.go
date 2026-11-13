package dto

// ImageArchivePreviewRequest asks the server what a `docker save` archive
// published over plain HTTP contains, before anything downloads it.
type ImageArchivePreviewRequest struct {
	URL string `json:"url"`
}

// ImageArchivePreview is what the server could learn from the archive's
// headers and its tail, reading at most a megabyte of a multi-gigabyte file.
//
// Previewable false is a normal answer, not an error: a compressed archive, or
// one served without range support, cannot be inspected ahead of the download.
// SizeBytes is still reported, and the operator supplies the reference.
type ImageArchivePreview struct {
	URL         string   `json:"url"`
	SizeBytes   int64    `json:"size_bytes"`
	RepoTags    []string `json:"repo_tags"`
	Previewable bool     `json:"previewable"`
}

// ImageNodeSyncRequest is the body of an image distribution request for one
// node. Source names a registry source label, "peer" is expressed by omitting
// it, and "url" selects an archive URL that must already be declared in the
// image's registry_sources. Relay asks the management server to stream the
// archive instead of the node downloading it directly.
type ImageNodeSyncRequest struct {
	Source       string `json:"source,omitempty"`
	SourceNode   string `json:"source_node,omitempty"`
	RateLimitKiB int64  `json:"rate_limit_kib_s,omitempty"`
	ArchiveURL   string `json:"archive_url,omitempty"`
	Relay        bool   `json:"relay,omitempty"`
	// Force pulls again even when the node already holds the image.
	Force bool `json:"force,omitempty"`

	ArchivePath string `json:"archive_path,omitempty"`
}

// ImageNodeSyncResponse acknowledges an accepted distribution request. The
// transfer itself is reported through the image detail endpoint.
type ImageNodeSyncResponse struct {
	State      string `json:"state"`
	TransferID string `json:"transfer_id,omitempty"`
	Message    string `json:"message,omitempty"`
}
