package messages

// Product errors for catalog version validation and node registration.
const (
	ErrorImageFrameworkVersionRequired = "IMAGE_FRAMEWORK_VERSION_REQUIRED"
	ErrorLocalImageCatalogChanged      = "LOCAL_IMAGE_CATALOG_CHANGED"
)

// ErrorImageCatalogUploaded refuses "check for updates" over a system image
// catalog that was uploaded by hand: the channel's catalog ({latest}) would
// replace it ({current}) and delete the versions only the upload has, so the
// request has to ask for that (replace_uploaded=true).
const ErrorImageCatalogUploaded = "IMAGE_CATALOG_UPLOADED"
