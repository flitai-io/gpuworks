package messages

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ProductError is a language-neutral API error. Fallback is an English message
// retained for older clients and scripts that only read the error field.
type ProductError struct {
	code     string
	params   map[string]any
	fallback string
	cause    error
}

// Product error codes shared by domain services and the API layer.
const (
	ErrorSleepEngineUnsupported             = "SLEEP_ENGINE_UNSUPPORTED"
	ErrorAgentUpgradeRequired               = "AGENT_UPGRADE_REQUIRED"
	ErrorLaunchFeaturesUnknown              = "LAUNCH_FEATURES_UNKNOWN"
	ErrorSleepTargetUnresolved              = "SLEEP_TARGET_UNRESOLVED"
	ErrorAcceleratorFeatureUnsupported      = "ACCELERATOR_FEATURE_UNSUPPORTED"
	ErrorAcceleratorDeviceSelectionRequired = proto.ErrorAcceleratorDeviceSelectionRequired
	// ErrorAcceleratorSMLimitUnsupported reports a compute/SM limit requested
	// on a vendor whose logical partition backend (if any) enforces GPU
	// memory only, such as AMD's amd-dgpu (ROCm enforces compute quotas in
	// hardware through HSA_CU_MASK, outside what a preloaded library can
	// verify; see docs/explanation/dgpu.md).
	ErrorAcceleratorSMLimitUnsupported = "ACCELERATOR_SM_LIMIT_UNSUPPORTED"

	// The experience environment's console (internal/experience) refuses
	// what would act on the host or leave the environment, and deployments
	// of any but its preloaded models ({models}).
	ErrorExperienceUnsupported       = "EXPERIENCE_UNSUPPORTED"
	ErrorExperienceModelNotPreloaded = "EXPERIENCE_MODEL_NOT_PRELOADED"
	// ErrorExperiencePortInUse: the experience environment's console port
	// ({port}) is held by another program.
	ErrorExperiencePortInUse = "EXPERIENCE_PORT_IN_USE"

	// Usage analytics queries (internal/server/analytics); params name
	// the field and say why.
	ErrorAnalyticsRangeInvalid = "ANALYTICS_RANGE_INVALID"
	ErrorAnalyticsRangeTooLong = "ANALYTICS_RANGE_TOO_LONG"
	ErrorAnalyticsTooManyCards = "ANALYTICS_TOO_MANY_CARDS"
	ErrorAnalyticsValueInvalid = "ANALYTICS_VALUE_INVALID"
	// ErrorCatalogFileValuedArgUnresolved reports a catalog spec that hands an
	// engine a file path no declaration inside the spec can supply - the
	// nano_v3_reasoning_parser.py defect, where the flag named a file that was
	// in no image, in no weights repository and in no asset, so the spec could
	// not start under any image. Raised only for the statically decidable
	// case; see inferencecatalog.validateFileValuedArguments for what the
	// check can and cannot see.
	ErrorCatalogFileValuedArgUnresolved = "CATALOG_FILE_VALUED_ARG_UNRESOLVED"
	// The CATALOG_METRICS_* codes report a spec's api.metrics declaration that
	// cannot be applied (inferenceapi/metrics.go): an endpoint that is not a
	// path, an inherit naming an engine without a built-in mapping, or a map
	// entry the collector could not honour. Params carry the entry's source
	// family and target metric.
	ErrorCatalogMetricsEndpointInvalid    = "CATALOG_METRICS_ENDPOINT_INVALID"
	ErrorCatalogMetricsInheritUnknown     = "CATALOG_METRICS_INHERIT_UNKNOWN"
	ErrorCatalogMetricsSourceInvalid      = "CATALOG_METRICS_SOURCE_INVALID"
	ErrorCatalogMetricsUnknownMetric      = "CATALOG_METRICS_UNKNOWN_METRIC"
	ErrorCatalogMetricsHistogramSelector  = "CATALOG_METRICS_HISTOGRAM_SELECTOR"
	ErrorCatalogMetricsHistogramScale     = "CATALOG_METRICS_HISTOGRAM_SCALE"
	ErrorCatalogMetricsNegativeScale      = "CATALOG_METRICS_NEGATIVE_SCALE"
	ErrorCatalogMetricsShadowedHistogram  = "CATALOG_METRICS_SHADOWED_HISTOGRAM"
	ErrorCatalogMetricsDuplicateSource    = "CATALOG_METRICS_DUPLICATE_SOURCE"
	ErrorCatalogMetricsFormatInvalid      = "CATALOG_METRICS_FORMAT_INVALID"
	ErrorCatalogMetricsJSONInherit        = "CATALOG_METRICS_JSON_INHERIT"
	ErrorCatalogMetricsJSONPathInvalid    = "CATALOG_METRICS_JSON_PATH_INVALID"
	ErrorCatalogMetricsJSONHistogram      = "CATALOG_METRICS_JSON_HISTOGRAM"
	ErrorCatalogMetricsJSONCompareCounter = "CATALOG_METRICS_JSON_COMPARE_COUNTER"
	// ErrorCatalogRequestStats* report an api.request_stats entry the gateway
	// could not honour. Params carry the entry's reply path and target stat.
	ErrorCatalogRequestStatsPathInvalid   = "CATALOG_REQUEST_STATS_PATH_INVALID"
	ErrorCatalogRequestStatsUnknownStat   = "CATALOG_REQUEST_STATS_UNKNOWN_STAT"
	ErrorCatalogRequestStatsNegativeScale = "CATALOG_REQUEST_STATS_NEGATIVE_SCALE"
	ErrorCatalogRequestStatsDuplicatePath = "CATALOG_REQUEST_STATS_DUPLICATE_PATH"
	// ErrorCatalogInvalid is the code of a template document that failed
	// parsing or validation without a code of its own; the template evaluate
	// endpoint reports it so every issue has a code. The English fallback
	// and params.detail carry the parser's own text.
	ErrorCatalogInvalid = "CATALOG_INVALID"
	// ErrorCatalogShmBytesInvalid reports a catalog spec whose shm_bytes is
	// negative or above 16 TiB. Params: model, spec, shm_bytes, max_bytes.
	ErrorCatalogShmBytesInvalid = "CATALOG_SHM_BYTES_INVALID"
	// ErrorInferenceSpecTargetRequired reports a delete-spec request that did
	// not name both the owning model_id and the user spec to retire.
	ErrorInferenceSpecTargetRequired = "INFERENCE_SPEC_TARGET_REQUIRED"
	// ErrorInferenceTemplateModelRequired reports a request for a template's
	// editable YAML that did not name the model.
	ErrorInferenceTemplateModelRequired = "INFERENCE_TEMPLATE_MODEL_REQUIRED"
	// ErrorInferenceTemplateSpecSourceInvalid reports a spec_source filter that
	// is neither user nor upstream.
	ErrorInferenceTemplateSpecSourceInvalid = "INFERENCE_TEMPLATE_SPEC_SOURCE_INVALID"
	// ErrorMountTargetDuplicate reports two mounts claiming the same container
	// path. A deployer sees it when a hand-written mount row collides with one
	// the platform owns, such as the managed directory behind the KV disk tier.
	ErrorMountTargetDuplicate = "MOUNT_TARGET_DUPLICATE"
	// ErrorShmBelowKVCPUTier reports a deployment whose own /dev/shm size is
	// smaller than the CPU KV tier it enables. The engine keeps the whole tier
	// in /dev/shm, so such a container can never start.
	ErrorShmBelowKVCPUTier = "SHM_BELOW_KV_CPU_TIER"
	// ErrorAscendV1CompatPreStart reports a preparation hook whose ordering or
	// image edits cannot be composed safely with the V1 compatibility patch.
	ErrorAscendV1CompatPreStart = "ASCEND_V1_COMPAT_PRE_START"
	// ErrorAscendCompatPreStartArgv reports argv preparation on any Ascend
	// compatibility overlay whose script is the only preparation form it can add.
	ErrorAscendCompatPreStartArgv = "ASCEND_COMPAT_PRE_START_ARGV"
	// ErrorPreStartScriptTooLarge reports a merged preparation script above the
	// agent's 64 KiB wrapper limit.
	ErrorPreStartScriptTooLarge = "PRE_START_SCRIPT_TOO_LARGE"
	// ErrorNoStoragePath reports two nodes that share no network allowed to
	// carry storage sync, so one cannot copy an image or model from the other.
	ErrorNoStoragePath = "NO_STORAGE_PATH"
	// ErrorComputeNetworkAcceleratorRequired: a node's accelerators run
	// their collectives across hosts on the cards' own ports, and the group
	// shares a network of another type.
	ErrorComputeNetworkAcceleratorRequired = "COMPUTE_NETWORK_ACCELERATOR_REQUIRED"
	// ErrorComputeNetworkNoCards: a node is on an accelerator network
	// without a usable card on it.
	ErrorComputeNetworkNoCards = "COMPUTE_NETWORK_NO_CARDS"
	// ErrorComputeNetworkRuntime: a node's container runtime cannot give a
	// container what the group's network needs.
	ErrorComputeNetworkRuntime = "COMPUTE_NETWORK_RUNTIME"
)

// Catalog warning codes: a template that imports, yet will not behave as its
// author most likely expects. Params carry model_id and spec (the spec name).
const (
	// WarningCatalogCallableMetricsUndeclared: a callable spec declares no
	// api.metrics, so the monitoring center has no engine metrics unless the
	// engine happens to export vLLM or SGLang metric names.
	WarningCatalogCallableMetricsUndeclared = "CATALOG_CALLABLE_METRICS_UNDECLARED"
	// WarningCatalogHealthEndpointUndeclared: a callable spec declares no
	// api.health_endpoint, so readiness is probed on the default /health.
	WarningCatalogHealthEndpointUndeclared = "CATALOG_HEALTH_ENDPOINT_UNDECLARED"
	// WarningCatalogCallableServedNameFixed: a callable spec passes a literal
	// --served-model-name instead of ${SERVED_MODEL_NAME}, so an engine that
	// checks the request's model rejects the deployment's served name. Params
	// also carry name, the literal it passes.
	WarningCatalogCallableServedNameFixed = "CATALOG_CALLABLE_SERVED_NAME_FIXED"
	// WarningCatalogDisplayProfileRetired: a spec still sets the retired
	// display_profile or gpus.display_target keys; they were ignored, and
	// the spec should carry display_name_zh_cn / display_name_en instead.
	WarningCatalogDisplayProfileRetired = "CATALOG_DISPLAY_PROFILE_RETIRED"
	// WarningCatalogArgPassedThrough: a spec gives a parameter the platform
	// dictionary owns a value the dictionary cannot read (a newer engine
	// choice, another type). It is passed to the engine verbatim; the form
	// and the estimate do not see it. Params also carry flag, value and key.
	WarningCatalogArgPassedThrough = "CATALOG_ARG_PASSED_THROUGH"
)

// NewProductError constructs a structured product error while keeping empty
// parameter maps off the wire.
func NewProductError(code, fallback string, params map[string]any) *ProductError {
	if len(params) == 0 {
		params = nil
	}
	return &ProductError{code: code, fallback: fallback, params: params}
}

// WrapProductError preserves a sentinel or lower-level cause for errors.Is and
// errors.As while exposing a stable product error at the API boundary.
func WrapProductError(code, fallback string, params map[string]any, cause error) *ProductError {
	err := NewProductError(code, fallback, params)
	err.cause = cause
	return err
}

func (e *ProductError) Error() string          { return e.fallback }
func (e *ProductError) Code() string           { return e.code }
func (e *ProductError) Params() map[string]any { return e.params }
func (e *ProductError) Unwrap() error          { return e.cause }

// NoStoragePath is the product error for two nodes that share no network
// allowed to carry storage sync.
func NoStoragePath(source, target string) *ProductError {
	return NewProductError(ErrorNoStoragePath,
		"The source and target nodes share no network that may carry storage sync; download from the source instead, or let the management network carry storage sync on both nodes.",
		map[string]any{"source": source, "target": target})
}

// ErrPeerRelayBusy is temporary server admission pressure, never peer failure.
var ErrPeerRelayBusy = errors.New("peer relay is busy")

const ErrorPeerRelayBusy = "PEER_RELAY_BUSY"

func PeerRelayBusy() *ProductError {
	return WrapProductError(ErrorPeerRelayBusy, "Peer relay is busy; retry this transfer from the same source.", nil, ErrPeerRelayBusy)
}
