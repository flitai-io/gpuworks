package messages

import "gpuworks.ai/gpuworks/internal/proto"

// Pull product error codes: where a node may pull an image or download model
// weights from, decided in
// package planning and passed on by the api layer.
const (
	// ErrorImageSourceForbidden: the pull source rules that apply to {node}
	// leave nothing to pull the image from ({route} is where they live).
	ErrorImageSourceForbidden = "IMAGE_SOURCE_FORBIDDEN"
	// ErrorWeightsFetchForbidden: the pull source rules that apply to {node}
	// leave nowhere to download {model}'s weights from ({route} is where
	// they live).
	ErrorWeightsFetchForbidden = "WEIGHTS_FETCH_FORBIDDEN"
	// ErrorModelMetadataForbidden: the site rules chosen for the management
	// node forbid every address of {source} ({route}).
	ErrorModelMetadataForbidden = "MODEL_METADATA_FORBIDDEN"
)

// What a model's own site answered to a read of its repository
// (modelmeta.Answer), on a registration, a metadata refresh and the deploy
// form's test of a template weight source.
const (
	// ErrorModelRepoNotFound: {host} answered {status} for the repository
	// {repo} of {source}: it has none by that id.
	ErrorModelRepoNotFound = "MODEL_REPO_NOT_FOUND"
	// ErrorModelRevisionNotFound: {host} has the repository {repo} and
	// answered {status} for its files at {revision}.
	ErrorModelRevisionNotFound = "MODEL_REVISION_NOT_FOUND"
	// ErrorModelRepoAccessDenied: {host} answered {status} (401 or 403) for
	// {repo}: private or gated, or, on Hugging Face asked without a token,
	// not there at all.
	ErrorModelRepoAccessDenied = "MODEL_REPO_ACCESS_DENIED"
)

// The deploy form's test of one weight source a template spec declares.
const (
	ErrorWeightSourceProbeQueryInvalid = "WEIGHT_SOURCE_PROBE_QUERY_INVALID"
	// ErrorWeightSourceNotDeclared: the spec does not declare the weight
	// source {repo} of {provider} a test was asked for.
	ErrorWeightSourceNotDeclared = "WEIGHT_SOURCE_NOT_DECLARED"
)

// Node-local archive imports take a new absolute path for each attempt.
const (
	ErrorImageArchivePathInvalid  = "IMAGE_ARCHIVE_PATH_INVALID"
	ErrorImageArchivePathRequired = "IMAGE_ARCHIVE_PATH_REQUIRED"

	ErrorImageArchivePreviewFailed = proto.ErrorArchivePreview
	ErrorImageArchiveLoadFailed    = proto.ErrorArchiveLoad
)
