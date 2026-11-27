package api

import "gpuworks.ai/gpuworks/internal/server/messages"

// The image catalog, its import and the update sources it is published
// through: the codes the images page and the channel reader answer with.
const (
	ErrorImageSeriesPatchInvalid         = "IMAGE_SERIES_PATCH_INVALID"
	ErrorOfficialImageSeriesManaged      = "OFFICIAL_IMAGE_SERIES_MANAGED"
	ErrorImageCommonArgFlagRequired      = "IMAGE_COMMON_ARG_FLAG_REQUIRED"
	ErrorImageCommonEnvKeyRequired       = "IMAGE_COMMON_ENV_KEY_REQUIRED"
	ErrorImageSeriesPatchEmpty           = "IMAGE_SERIES_PATCH_EMPTY"
	ErrorImageVersionPatchInvalid        = "IMAGE_VERSION_PATCH_INVALID"
	ErrorImageVersionPatchEmpty          = "IMAGE_VERSION_PATCH_EMPTY"
	ErrorImageInUse                      = "IMAGE_IN_USE"
	ErrorOfficialImageDeleteForbidden    = "OFFICIAL_IMAGE_DELETE_FORBIDDEN"
	ErrorImageSyncUnavailable            = "IMAGE_SYNC_UNAVAILABLE"
	ErrorImageRetryUnavailable           = "IMAGE_RETRY_UNAVAILABLE"
	ErrorImageInspectionUnavailable      = "IMAGE_INSPECTION_UNAVAILABLE"
	ErrorImageImportTooLarge             = "IMAGE_IMPORT_TOO_LARGE"
	ErrorImageFrameworkVersionRequired   = messages.ErrorImageFrameworkVersionRequired
	ErrorImageSeriesVersionRequired      = "IMAGE_SERIES_VERSION_REQUIRED"
	ErrorCatalogPackageDigestInvalid     = "CATALOG_PACKAGE_DIGEST_INVALID"
	ErrorCatalogPackageRequiredForFiles  = "CATALOG_PACKAGE_REQUIRED_FOR_FILES"
	ErrorImageImportInvalid              = "IMAGE_IMPORT_INVALID"
	ErrorImageImportUnsupported          = "IMAGE_IMPORT_UNSUPPORTED"
	ErrorImageImportConflict             = "IMAGE_IMPORT_CONFLICT"
	ErrorImageManifestLegacyProjection   = "IMAGE_MANIFEST_LEGACY_PROJECTION"
	ErrorImageManifestInvalid            = "IMAGE_MANIFEST_INVALID"
	ErrorUpdateSourcesUnavailable        = "UPDATE_SOURCES_UNAVAILABLE"
	ErrorUpdateDownloadFailed            = "UPDATE_DOWNLOAD_FAILED"
	ErrorUpdateNotPublished              = "UPDATE_NOT_PUBLISHED"
	ErrorUpdateCatalogIncompatible       = "UPDATE_CATALOG_INCOMPATIBLE"
	ErrorOfficialCatalogPackageRequired  = "OFFICIAL_CATALOG_PACKAGE_REQUIRED"
	ErrorOfficialCatalogNeedsNewerServer = "OFFICIAL_CATALOG_NEEDS_NEWER_SERVER"
)
