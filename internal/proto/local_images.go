package proto

import "time"

const ImageOperationTimeout = 12 * time.Hour

// ImageRuntimeList and ImageRuntimeInspect are read-only. Import verifies the
// selected runtime/namespace and immutable identity before adopting the tag.
const (
	RPCImageRuntimeList        = "image.runtime_list"
	RPCImageRuntimeInspect     = "image.runtime_inspect"
	RPCImageRuntimeImport      = "image.runtime_import"
	ErrorLocalImageBusy        = "LOCAL_IMAGE_BUSY"
	ErrorLocalImageUnavailable = "LOCAL_IMAGE_UNAVAILABLE"
	ErrorLocalImageChanged     = "LOCAL_IMAGE_CHANGED"
	ErrorLocalImageConflict    = "LOCAL_IMAGE_CONFLICT"
	ErrorLocalImageUnsupported = "LOCAL_IMAGE_UNSUPPORTED"
)

type RuntimeImage struct {
	Ref      string `json:"ref"`
	Identity string `json:"identity"`
	Size     string `json:"size"`
}

type RuntimeImageList struct {
	Runtime         string         `json:"runtime"`
	TargetNamespace string         `json:"target_namespace"`
	Namespace       string         `json:"namespace"`
	Namespaces      []string       `json:"namespaces,omitempty"`
	Items           []RuntimeImage `json:"items"`
}

type RuntimeImageSelection struct {
	Runtime         string `json:"runtime"`
	TargetNamespace string `json:"target_namespace"`
	Namespace       string `json:"namespace"`
	Ref             string `json:"ref"`
	Identity        string `json:"identity"`
}

type RuntimeImageCopyRequest struct {
	RuntimeImageSelection
	TransferID string `json:"transfer_id"`
}

const (
	RPCImageRuntimePrepare = "image.runtime_prepare"
	RPCImageRuntimeCopy    = "image.runtime_copy"
	RuntimeImageCopySource = "runtime"
)

type RuntimeImageDetails struct {
	CopyRequired bool `json:"copy_required,omitempty"`

	RuntimeSizeBytes *int64               `json:"runtime_size_bytes,omitempty"`
	ImageID          string               `json:"image_id"`
	Metadata         ArchiveImageMetadata `json:"metadata"`
}
