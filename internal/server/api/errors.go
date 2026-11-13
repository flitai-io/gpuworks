package api

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// API product error codes. Their English fallbacks stay at the call site so
// old clients receive an actionable error string; the UI translates the stable
// code and params from common.json.
const (
	ErrorAgentUpgradeRefused      = "AGENT_UPGRADE_REFUSED"
	ErrorModelCacheRetryBlocked   = "MODEL_CACHE_RETRY_BLOCKED"
	ErrorNodeLabelsInvalid        = "NODE_LABELS_INVALID"
	ErrorNodeRPCUnavailable       = "NODE_RPC_UNAVAILABLE"
	ErrorNodeOffline              = messages.ErrorNodeOffline
	ErrorNodeSchedulable          = "NODE_SCHEDULABLE"
	ErrorNodeRuntimeInvalid       = "NODE_RUNTIME_INVALID"
	ErrorNodeRuntimeKube          = "NODE_RUNTIME_KUBE_NODE" // a Kubernetes node's runtime is its cluster's, not a choice
	ErrorNodeDeleteForceInvalid   = "NODE_DELETE_FORCE_INVALID"
	ErrorNodeAcceleratorInvalid   = "NODE_ACCELERATOR_INVALID"
	ErrorNodeAcceleratorNotOnHost = "NODE_ACCELERATOR_NOT_ON_HOST"
	// ErrorCardNotOwned answers an operator making schedulable a card that is
	// not the node's to use (lent to a virtual machine, without a driver).
	ErrorCardNotOwned = "CARD_NOT_OWNED"
	// ErrorCardChecksUnsupported answers a request about a card's checks to a
	// node whose agent predates them.
	ErrorCardChecksUnsupported                  = "CARD_CHECKS_UNSUPPORTED"
	ErrorNodeAcceleratorHasInstances            = "NODE_ACCELERATOR_HAS_INSTANCES"
	ErrorNodeDeleteHasInstances                 = "NODE_DELETE_HAS_INSTANCES"
	ErrorNodeDeleteCacheBusy                    = "NODE_DELETE_CACHE_BUSY"
	ErrorNodeDeleteSSHRequired                  = "NODE_DELETE_SSH_REQUIRED"
	ErrorNodeUninstallUnconfirmed               = "NODE_UNINSTALL_UNCONFIRMED"
	ErrorNodeDeleteCleanupFailed                = "NODE_DELETE_CLEANUP_FAILED"
	ErrorAuthNotConfigured                      = "AUTH_NOT_CONFIGURED"
	ErrorInvalidCredentials                     = "INVALID_CREDENTIALS"
	ErrorLoginThrottled                         = "LOGIN_THROTTLED"
	ErrorPasswordLength                         = "PASSWORD_LENGTH"
	ErrorPasswordCharacters                     = "PASSWORD_CHARACTERS"
	ErrorPasswordGuessable                      = "PASSWORD_GUESSABLE"
	ErrorPasswordUsername                       = "PASSWORD_USERNAME"
	ErrorPasswordUnchanged                      = "PASSWORD_UNCHANGED"
	ErrorCurrentPasswordInvalid                 = "CURRENT_PASSWORD_INVALID"
	ErrorAuthenticationRequired                 = "AUTHENTICATION_REQUIRED"
	ErrorManagementOriginRejected               = "MANAGEMENT_ORIGIN_REJECTED"
	ErrorAdministratorRequired                  = "ADMINISTRATOR_REQUIRED"
	ErrorHostAddressRequired                    = "HOST_ADDRESS_REQUIRED"
	ErrorHostAddressInvalid                     = "HOST_ADDRESS_INVALID"
	ErrorServerOutboundPolicyInvalid            = "SERVER_OUTBOUND_POLICY_INVALID"
	ErrorServerOutboundPolicyConflict           = "SERVER_OUTBOUND_POLICY_CONFLICT"
	ErrorServerOutboundPolicyReadOnly           = "SERVER_OUTBOUND_POLICY_READ_ONLY"
	ErrorServerOutboundPolicyUnavailable        = "SERVER_OUTBOUND_POLICY_UNAVAILABLE"
	ErrorServerListenInvalid                    = "SERVER_LISTEN_INVALID"
	ErrorServerListenNotLocal                   = "SERVER_LISTEN_NOT_LOCAL"
	ErrorServerListenLockout                    = "SERVER_LISTEN_LOCKOUT"
	ErrorServerListenReadOnly                   = "SERVER_LISTEN_READ_ONLY"
	ErrorServerRestartManual                    = "SERVER_RESTART_MANUAL"
	ErrorNodePrecheckUnavailable                = "NODE_PRECHECK_UNAVAILABLE"
	ErrorNodeInstallerUnavailable               = "NODE_INSTALLER_UNAVAILABLE"
	ErrorNodeInstallHostAlreadyNode             = "NODE_INSTALL_HOST_ALREADY_NODE"
	ErrorNodeInstallExistingAgent               = "NODE_INSTALL_EXISTING_AGENT"
	ErrorNodeInstallExistingAgentUnknown        = "NODE_INSTALL_EXISTING_AGENT_UNKNOWN"
	ErrorNodeInstallExistingAgentSelf           = "NODE_INSTALL_EXISTING_AGENT_SELF"
	ErrorNodeInstallExistingAgentSameDir        = "NODE_INSTALL_EXISTING_AGENT_SAME_DIR"
	ErrorNodeInstallDirUntraversable            = "NODE_INSTALL_DIR_UNTRAVERSABLE"
	ErrorNodeStoreUnavailable                   = "NODE_STORE_UNAVAILABLE"
	ErrorNodeConnectionDiagnosticsInput         = "NODE_CONNECTION_DIAGNOSTICS_INPUT"
	ErrorNodeConnectionSettingsUnavailable      = "NODE_CONNECTION_SETTINGS_UNAVAILABLE"
	ErrorNodeRecoveryUnavailable                = "NODE_RECOVERY_UNAVAILABLE"
	ErrorPlatformSSHKeyMissing                  = "PLATFORM_SSH_KEY_MISSING"
	ErrorAIEndpointNotConfigured                = "AI_ENDPOINT_NOT_CONFIGURED"
	ErrorAITaskInvalid                          = "AI_TASK_INVALID"
	ErrorAITargetRequired                       = "AI_TARGET_REQUIRED"
	ErrorAIAskBusy                              = "AI_ASK_BUSY"
	ErrorPlatformTokenReadOnly                  = "PLATFORM_TOKEN_READ_ONLY"
	ErrorPlatformTokenNotAllowed                = "PLATFORM_TOKEN_NOT_ALLOWED"
	ErrorPlatformTokenScopeInvalid              = "PLATFORM_TOKEN_SCOPE_INVALID"
	ErrorPlatformTokenNameInvalid               = "PLATFORM_TOKEN_NAME_INVALID"
	ErrorAPIKeyNameRequired                     = "API_KEY_NAME_REQUIRED"
	ErrorAPIKeyScopeInvalid                     = "API_KEY_SCOPE_INVALID"
	ErrorAPIKeyDeploymentUnknown                = "API_KEY_DEPLOYMENT_UNKNOWN"
	ErrorAPIKeyNotFound                         = "API_KEY_NOT_FOUND"
	ErrorPlatformTokenExpiryInvalid             = "PLATFORM_TOKEN_EXPIRY_INVALID"
	ErrorPlatformTokenIDInvalid                 = "PLATFORM_TOKEN_ID_INVALID"
	ErrorMCPDisabled                            = "MCP_DISABLED"
	ErrorMetricRequired                         = "METRIC_REQUIRED"
	ErrorMetricUnknown                          = "METRIC_UNKNOWN"
	ErrorLogsFilterInvalid                      = "LOGS_FILTER_INVALID"
	ErrorQueryWindowInvalid                     = "QUERY_WINDOW_INVALID"
	ErrorLogsContextInvalid                     = "LOGS_CONTEXT_INVALID"
	ErrorLogsFilterInstancesConflict            = "LOGS_FILTER_INSTANCES_CONFLICT"
	ErrorLogsFilterInstancesTooMany             = "LOGS_FILTER_INSTANCES_TOO_MANY"
	ErrorTerminalTargetUnsupported              = "TERMINAL_TARGET_UNSUPPORTED"
	ErrorProviderActionInvalid                  = "PROVIDER_ACTION_INVALID"
	ErrorDigestImageUnsupported                 = "DIGEST_IMAGE_UNSUPPORTED"
	ErrorDeploymentNotActive                    = "DEPLOYMENT_NOT_ACTIVE"
	ErrorDeploymentDeleting                     = "DEPLOYMENT_DELETING"
	ErrorServedNameInUse                        = messages.ErrorServedNameInUse
	ErrorRestartStrategyInvalid                 = "RESTART_STRATEGY_INVALID"
	ErrorReplicasTooFew                         = messages.ErrorReplicasTooFew
	ErrorReplicasBodyShape                      = "REPLICAS_BODY_SHAPE"
	ErrorReplicasRemoveInvalid                  = messages.ErrorReplicasRemoveInvalid
	ErrorReplicasChanged                        = "REPLICAS_CHANGED"
	ErrorReplicasRemoveTooMany                  = messages.ErrorReplicasRemoveTooMany
	ErrorUnitUnknown                            = "UNIT_UNKNOWN"
	ErrorUnitNotFailed                          = "UNIT_NOT_FAILED"
	ErrorUnitRetryNotActive                     = "UNIT_RETRY_NOT_ACTIVE"
	ErrorReplicasNegative                       = messages.ErrorReplicasNegative
	ErrorRoutingPolicyInvalid                   = messages.ErrorRoutingPolicyInvalid
	ErrorDistributedReplicaCount                = messages.ErrorDistributedReplicaCount
	ErrorSleepNotEnabled                        = messages.ErrorSleepNotEnabled
	ErrorSleepEngineUnsupported                 = messages.ErrorSleepEngineUnsupported
	ErrorSleepInvalidLevel                      = "SLEEP_INVALID_LEVEL"
	ErrorSleepInvalidMode                       = "SLEEP_INVALID_MODE"
	ErrorSleepInvalidWakeOnRequest              = "SLEEP_INVALID_WAKE_ON_REQUEST"
	ErrorWakeInsufficientGPUMemory              = "WAKE_INSUFFICIENT_GPU_MEMORY"
	ErrorWakeGPUHeldOutside                     = "WAKE_GPU_HELD_OUTSIDE"
	ErrorSleepHostMemoryInsufficient            = messages.ErrorSleepHostMemoryInsufficient
	ErrorNodeReplacementInvalid                 = "NODE_REPLACEMENT_INVALID"
	ErrorReplicasPlaceOnTooMany                 = messages.ErrorReplicasPlaceOnTooMany
	ErrorReplicasPlaceOnRefused                 = messages.ErrorReplicasPlaceOnRefused
	ErrorDistributedNodeCount                   = messages.ErrorDistributedNodeCount
	ErrorDistributedTPRequired                  = messages.ErrorDistributedTPRequired
	ErrorMasterPortInvalid                      = messages.ErrorMasterPortInvalid
	ErrorDistributedDuplicateNode               = messages.ErrorDistributedDuplicateNode
	ErrorDistributedNodeOffline                 = messages.ErrorDistributedNodeOffline
	ErrorDistributedGPUCount                    = messages.ErrorDistributedGPUCount
	ErrorDistributedParallelIndivisible         = messages.ErrorDistributedParallelIndivisible
	ErrorDistributedPartitionBackendUnavailable = messages.ErrorDistributedPartitionBackendUnavailable
	ErrorModelSourceIDRequired                  = "MODEL_SOURCE_ID_REQUIRED"
	ErrorQuickRegisterFieldsRequired            = "QUICK_REGISTER_FIELDS_REQUIRED"
	ErrorQuickRegisterSourceUnsupported         = "QUICK_REGISTER_SOURCE_UNSUPPORTED"
	ErrorModelIDInvalid                         = "MODEL_ID_INVALID"
	ErrorDeploymentIDInvalid                    = "DEPLOYMENT_ID_INVALID"
	ErrorEventCursorInvalid                     = "EVENT_CURSOR_INVALID"
	ErrorEventLimitInvalid                      = "EVENT_LIMIT_INVALID"
	ErrorListPageInvalid                        = "LIST_PAGE_INVALID"
	ErrorListFilterInvalid                      = "LIST_FILTER_INVALID"
	ErrorModelInUse                             = "MODEL_IN_USE"
	ErrorModelInferenceConfigInvalid            = "MODEL_INFERENCE_CONFIG_INVALID"
	ErrorModelSourceIDInvalid                   = "MODEL_SOURCE_ID_INVALID"
	ErrorModelSourceIDFixed                     = "MODEL_SOURCE_ID_FIXED"
	ErrorModelSourceIDTaken                     = "MODEL_SOURCE_ID_TAKEN"
	ErrorModelTransferActive                    = "MODEL_TRANSFER_ACTIVE"
	ErrorTemplateWeightModelsQueryInvalid       = "TEMPLATE_WEIGHT_MODELS_QUERY_INVALID"
	ErrorLocalModelPublicDownload               = "LOCAL_MODEL_PUBLIC_DOWNLOAD"
	ErrorPeerSyncActive                         = "PEER_SYNC_ACTIVE"
	ErrorModelSyncUnavailable                   = "MODEL_SYNC_UNAVAILABLE"
	ErrorAcquisitionCandidateStale              = messages.ErrorAcquisitionCandidateStale
	ErrorAcquisitionImageRequired               = messages.ErrorAcquisitionImageRequired
	ErrorSettingRemoved                         = "SETTING_REMOVED"
	ErrorPullSettingInvalid                     = "PULL_SETTING_INVALID"
	// ErrorCredentialLoginRejected: {host} refused the stored login.
	ErrorCredentialLoginRejected = "CREDENTIAL_LOGIN_REJECTED"
	// ErrorCredentialLoginFailed: the management node could not try the
	// login at {host} ({detail}).
	ErrorCredentialLoginFailed = "CREDENTIAL_LOGIN_FAILED"
	// ErrorSourceProbeFailed: the node could not run a connectivity test
	// ({detail}); an agent older than the server cannot.
	ErrorSourceProbeFailed     = "SOURCE_PROBE_FAILED"
	ErrorRuntimeRestartFailed  = "RUNTIME_RESTART_FAILED"
	ErrorImageSourceRemoved    = "IMAGE_SOURCE_REMOVED"
	ErrorImageSourceForbidden  = messages.ErrorImageSourceForbidden
	ErrorWeightsFetchForbidden = messages.ErrorWeightsFetchForbidden
	// ErrorSettingHasOwnRoute refuses the generic settings route for a
	// setting with a route of its own.
	ErrorSettingHasOwnRoute          = "SETTING_HAS_OWN_ROUTE"
	ErrorSecuritySettingsUnavailable = "SECURITY_SETTINGS_UNAVAILABLE"
	// Telemetry export setting (telemetry_export.go).
	ErrorTelemetryEndpointInvalid         = "TELEMETRY_ENDPOINT_INVALID"
	ErrorTelemetryHeaderInvalid           = "TELEMETRY_HEADER_INVALID"
	ErrorTelemetryHeaderValueRequired     = "TELEMETRY_HEADER_VALUE_REQUIRED"
	ErrorTelemetrySampleRatioInvalid      = "TELEMETRY_SAMPLE_RATIO_INVALID"
	ErrorTelemetryTraceLinkInvalid        = "TELEMETRY_TRACE_LINK_INVALID"
	ErrorNetworkNotFound                  = "NETWORK_NOT_FOUND"
	ErrorNetworkDedicatedUnavailable      = "NETWORK_DEDICATED_UNAVAILABLE"
	ErrorNetworkNameRequired              = "NETWORK_NAME_REQUIRED"
	ErrorNetworkNameTaken                 = "NETWORK_NAME_TAKEN"
	ErrorNetworkRoleInvalid               = "NETWORK_ROLE_INVALID"
	ErrorNetworkTypeInvalid               = "NETWORK_TYPE_INVALID"
	ErrorNetworkTypeRoleInvalid           = "NETWORK_TYPE_ROLE_INVALID"
	ErrorNetworkSelectorInvalid           = "NETWORK_SELECTOR_INVALID"
	ErrorNetworkAttachmentTargetInvalid   = "NETWORK_ATTACHMENT_TARGET_INVALID"
	ErrorNetworkAttachmentExists          = "NETWORK_ATTACHMENT_EXISTS"
	ErrorNetworkAttachmentNotFound        = "NETWORK_ATTACHMENT_NOT_FOUND"
	ErrorNetworkOverrideInvalid           = "NETWORK_OVERRIDE_INVALID"
	ErrorNetworkHCALast                   = "NETWORK_HCA_LAST"
	ErrorNetworkHCANotRDMA                = "NETWORK_HCA_NOT_RDMA"
	ErrorNetworkHCANodeInvalid            = "NETWORK_HCA_NODE_INVALID"
	ErrorNetworkHCAInvalid                = "NETWORK_HCA_INVALID"
	ErrorBenchmarkServerOriginUnavailable = "BENCHMARK_SERVER_ORIGIN_UNAVAILABLE"
	ErrorServerURLRequired                = "SERVER_URL_REQUIRED"
	ErrorServerURLInvalid                 = "SERVER_URL_INVALID"
	ErrorServerDialListenInvalid          = "SERVER_DIAL_LISTEN_INVALID"
	ErrorInstallDirInvalid                = "INSTALL_DIR_INVALID"
	ErrorInstanceRestartRequired          = "INSTANCE_RESTART_REQUIRED"
	ErrorManagementRolesNodesRequired     = "MANAGEMENT_ROLES_NODES_REQUIRED"
	ErrorManagementRolesConflict          = "MANAGEMENT_ROLES_CONFLICT"
	ErrorManagementRolesNodeNotFound      = "MANAGEMENT_ROLES_NODE_NOT_FOUND"
	ErrorComputeNetworkNone               = messages.ErrorComputeNetworkNone
	ErrorComputeNetworkMixed              = messages.ErrorComputeNetworkMixed
	ErrorComputeNetworkMismatch           = messages.ErrorComputeNetworkMismatch
	ErrorNetworkTestUnsupported           = "NETWORK_TEST_UNSUPPORTED"
	ErrorNetworkTestNodeCount             = "NETWORK_TEST_NODE_COUNT"
	ErrorNetworkTestNodeInvalid           = "NETWORK_TEST_NODE_INVALID"
	ErrorNetworkTestScopeInvalid          = "NETWORK_TEST_SCOPE_INVALID"
	ErrorNetworkTestRunning               = "NETWORK_TEST_RUNNING"
	ErrorNetworkTestKindInvalid           = "NETWORK_TEST_KIND_INVALID"
	ErrorNetworkTestPatternInvalid        = "NETWORK_TEST_PATTERN_INVALID"
	ErrorNetworkTestTooLong               = "NETWORK_TEST_TOO_LONG"
	ErrorLocalStorageDisabled             = "LOCAL_STORAGE_DISABLED"
	ErrorLocalModelFieldsRequired         = "LOCAL_MODEL_FIELDS_REQUIRED"
	ErrorModelPathRequired                = "MODEL_PATH_REQUIRED"
	ErrorModelFilePathOccupied            = messages.ErrorModelFilePathOccupied
	ErrorLocalModelMetadataRefresh        = "LOCAL_MODEL_METADATA_REFRESH"
	ErrorModelMetadataUnavailable         = "MODEL_METADATA_UNAVAILABLE"
	// ErrorModelMetadataUnreachable: the management node reached none of
	// {hosts}, the addresses the site rules give it for {source}.
	ErrorModelMetadataUnreachable               = "MODEL_METADATA_UNREACHABLE"
	ErrorModelMetadataForbidden                 = messages.ErrorModelMetadataForbidden
	ErrorModelSourceUnavailable                 = messages.ErrorModelSourceUnavailable
	ErrorPreflightMinimumNodes                  = messages.ErrorPreflightMinimumNodes
	ErrorPreflightMaximumNodes                  = messages.ErrorPreflightMaximumNodes
	ErrorPreflightNodeRequired                  = messages.ErrorPreflightNodeRequired
	ErrorPreflightNodeWhitespace                = messages.ErrorPreflightNodeWhitespace
	ErrorPreflightDuplicateNode                 = messages.ErrorPreflightDuplicateNode
	ErrorPreflightPortInvalid                   = messages.ErrorPreflightPortInvalid
	ErrorPreflightDurationInvalid               = messages.ErrorPreflightDurationInvalid
	ErrorPreflightNodeInvalid                   = messages.ErrorPreflightNodeInvalid
	ErrorPreflightNodeOffline                   = messages.ErrorPreflightNodeOffline
	ErrorImageSeriesPatchInvalid                = "IMAGE_SERIES_PATCH_INVALID"
	ErrorOfficialImageSeriesManaged             = "OFFICIAL_IMAGE_SERIES_MANAGED"
	ErrorImageCommonArgFlagRequired             = "IMAGE_COMMON_ARG_FLAG_REQUIRED"
	ErrorImageCommonEnvKeyRequired              = "IMAGE_COMMON_ENV_KEY_REQUIRED"
	ErrorImageSeriesPatchEmpty                  = "IMAGE_SERIES_PATCH_EMPTY"
	ErrorImageVersionPatchInvalid               = "IMAGE_VERSION_PATCH_INVALID"
	ErrorImageVersionPatchEmpty                 = "IMAGE_VERSION_PATCH_EMPTY"
	ErrorImageInUse                             = "IMAGE_IN_USE"
	ErrorOfficialImageDeleteForbidden           = "OFFICIAL_IMAGE_DELETE_FORBIDDEN"
	ErrorImageSyncUnavailable                   = "IMAGE_SYNC_UNAVAILABLE"
	ErrorImageRetryUnavailable                  = "IMAGE_RETRY_UNAVAILABLE"
	ErrorImageInspectionUnavailable             = "IMAGE_INSPECTION_UNAVAILABLE"
	ErrorImageImportTooLarge                    = "IMAGE_IMPORT_TOO_LARGE"
	ErrorImageFrameworkVersionRequired          = "IMAGE_FRAMEWORK_VERSION_REQUIRED"
	ErrorImageSeriesVersionRequired             = "IMAGE_SERIES_VERSION_REQUIRED"
	ErrorCatalogPackageDigestInvalid            = "CATALOG_PACKAGE_DIGEST_INVALID"
	ErrorCatalogPackageRequiredForFiles         = "CATALOG_PACKAGE_REQUIRED_FOR_FILES"
	ErrorImageImportInvalid                     = "IMAGE_IMPORT_INVALID"
	ErrorImageImportUnsupported                 = "IMAGE_IMPORT_UNSUPPORTED"
	ErrorImageImportConflict                    = "IMAGE_IMPORT_CONFLICT"
	ErrorImageManifestInvalid                   = "IMAGE_MANIFEST_INVALID"
	ErrorUpdateSourcesUnavailable               = "UPDATE_SOURCES_UNAVAILABLE"
	ErrorUpdateDownloadFailed                   = "UPDATE_DOWNLOAD_FAILED"
	ErrorUpdateNotPublished                     = "UPDATE_NOT_PUBLISHED"
	ErrorUpdateCatalogIncompatible              = "UPDATE_CATALOG_INCOMPATIBLE"
	ErrorOfficialCatalogPackageRequired         = "OFFICIAL_CATALOG_PACKAGE_REQUIRED"
	ErrorOfficialCatalogNeedsNewerServer        = "OFFICIAL_CATALOG_NEEDS_NEWER_SERVER"
	ErrorServerUpgradeUnavailable               = "SERVER_UPGRADE_UNAVAILABLE"
	ErrorServerUpgradeArchiveInvalid            = "SERVER_UPGRADE_ARCHIVE_INVALID"
	ErrorServerAlreadyAtRelease                 = "SERVER_ALREADY_AT_RELEASE"
	ErrorServerNewerThanRelease                 = "SERVER_NEWER_THAN_RELEASE"
	ErrorUpdateChannelReadOnly                  = "UPDATE_CHANNEL_READ_ONLY"
	ErrorUpdateAutoCheckReadOnly                = "UPDATE_AUTO_CHECK_READ_ONLY"
	ErrorUpdateAutoCheckInvalid                 = "UPDATE_AUTO_CHECK_INVALID"
	ErrorUpdateChannelInvalid                   = "UPDATE_CHANNEL_INVALID"
	ErrorUsageReportReadOnly                    = "USAGE_REPORT_READ_ONLY"
	ErrorUsageReportInvalid                     = "USAGE_REPORT_INVALID"
	ErrorUsageReportUnavailable                 = "USAGE_REPORT_UNAVAILABLE"
	ErrorExperienceUnavailable                  = "EXPERIENCE_UNAVAILABLE"
	ErrorExperienceInvalid                      = "EXPERIENCE_INVALID"
	ErrorExperienceFailed                       = "EXPERIENCE_FAILED"
	ErrorNoticeIDsInvalid                       = "NOTICE_IDS_INVALID"
	ErrorImageRefRequired                       = "IMAGE_REF_REQUIRED"
	ErrorImageTagRequired                       = "IMAGE_TAG_REQUIRED"
	ErrorImagePurposeInvalid                    = "IMAGE_PURPOSE_INVALID"
	ErrorImageEngineRequired                    = "IMAGE_ENGINE_REQUIRED"
	ErrorStorageEnabledRequired                 = "STORAGE_ENABLED_REQUIRED"
	ErrorStorageConfigChanged                   = "STORAGE_CONFIG_CHANGED"
	ErrorStorageInUse                           = "STORAGE_IN_USE"
	ErrorStorageIsDefault                       = "STORAGE_IS_DEFAULT"
	ErrorStorageExportTaken                     = "STORAGE_EXPORT_ALREADY_REGISTERED"
	ErrorStorageNameTaken                       = "STORAGE_NAME_TAKEN"
	ErrorStorageDisabled                        = messages.ErrorStorageDisabled
	ErrorStorageNotWritable                     = messages.ErrorStorageNotWritable
	ErrorStorageTargetsRequired                 = "STORAGE_TARGETS_REQUIRED"
	ErrorStorageNameInvalid                     = "STORAGE_NAME_INVALID"
	ErrorStorageExportInvalid                   = "STORAGE_EXPORT_INVALID"
	ErrorStorageOptionsInvalid                  = "STORAGE_OPTIONS_INVALID"
	ErrorStorageTypeInvalid                     = "STORAGE_TYPE_INVALID"
	ErrorStoragePathInvalid                     = "STORAGE_PATH_INVALID"
	ErrorStoragePathDenied                      = "STORAGE_PATH_DENIED"
	ErrorStoragePathRequired                    = "STORAGE_PATH_REQUIRED"
	ErrorStorageCacheLimitNegative              = "STORAGE_CACHE_LIMIT_NEGATIVE"
	ErrorStorageImportCountInvalid              = "STORAGE_IMPORT_COUNT_INVALID"
	ErrorStorageProbeFailed                     = "STORAGE_PROBE_FAILED"
	ErrorStorageUnavailableOnNode               = messages.ErrorStorageUnavailableOnNode
	ErrorStorageScanFailed                      = "STORAGE_SCAN_FAILED"
	ErrorModelMissingInStorage                  = "MODEL_MISSING_IN_STORAGE"
	ErrorStorageSharedByOtherControlPlanes      = "STORAGE_SHARED_BY_OTHER_CONTROL_PLANES"
	ErrorBenchmarkGPUCountNegative              = "BENCHMARK_GPU_COUNT_NEGATIVE"
	ErrorBenchmarkDetailCapTooLarge             = "BENCHMARK_DETAIL_CAP_TOO_LARGE"
	ErrorBenchmarkTargetInactive                = "BENCHMARK_TARGET_INACTIVE"
	ErrorBenchmarkChatRequired                  = "BENCHMARK_CHAT_REQUIRED"
	ErrorBenchmarkLeaderNotRunning              = "BENCHMARK_LEADER_NOT_RUNNING"
	ErrorBenchmarkInstanceOrigin                = "BENCHMARK_INSTANCE_ORIGIN"
	ErrorBenchmarkExternalURLRequired           = "BENCHMARK_EXTERNAL_URL_REQUIRED"
	ErrorBenchmarkServerActive                  = "BENCHMARK_SERVER_ACTIVE"
	ErrorBenchmarkRunConflict                   = "BENCHMARK_RUN_CONFLICT"
	ErrorBenchmarkAgentRPCUnavailable           = "BENCHMARK_AGENT_RPC_UNAVAILABLE"
	ErrorBenchmarkCancelUnconfirmed             = "BENCHMARK_CANCEL_UNCONFIRMED"
	ErrorBenchmarkURLRequired                   = "BENCHMARK_URL_REQUIRED"
	ErrorBenchmarkModelsEmpty                   = "BENCHMARK_MODELS_EMPTY"
	ErrorBenchmarkDatasetEmpty                  = "BENCHMARK_DATASET_EMPTY"
	ErrorBenchmarkArtifactDirUnavailable        = "BENCHMARK_ARTIFACT_DIR_UNAVAILABLE"
	ErrorBenchmarkTokenizerEmpty                = "BENCHMARK_TOKENIZER_EMPTY"
	ErrorBenchmarkTokenizerUnsupported          = "BENCHMARK_TOKENIZER_UNSUPPORTED"
	ErrorBenchmarkTokenizerSourceInvalid        = "BENCHMARK_TOKENIZER_SOURCE_INVALID"
	ErrorBenchmarkTargetSnapshotFailed          = "BENCHMARK_TARGET_SNAPSHOT_FAILED"
	ErrorBenchmarkDatasetRetired                = "BENCHMARK_DATASET_RETIRED"
	ErrorBenchmarkAgenticInvalid                = "BENCHMARK_AGENTIC_CONFIG_INVALID"
	ErrorBenchmarkAgenticTokenizer              = "BENCHMARK_AGENTIC_TOKENIZER_REQUIRED"
	ErrorBenchmarkParameterInvalid              = "BENCHMARK_PARAMETER_INVALID"
	ErrorBenchmarkLoadValueInvalid              = "BENCHMARK_LOAD_VALUE_INVALID"
	ErrorBenchmarkOpenLoopMode                  = "BENCHMARK_OPEN_LOOP_MODE"
	ErrorBenchmarkAgentArrivalUnsupported       = "BENCHMARK_AGENT_ARRIVAL_UNSUPPORTED"
	ErrorBenchmarkAgentInteractivityUnsupported = "BENCHMARK_AGENT_INTERACTIVITY_UNSUPPORTED"
	ErrorBenchmarkAgentEvidenceUnsupported      = "BENCHMARK_AGENT_EVIDENCE_UNSUPPORTED"
	ErrorSettingJSONInvalid                     = "SETTING_JSON_INVALID"
	ErrorCommunityCatalogJSONInvalid            = "COMMUNITY_CATALOG_JSON_INVALID"
	ErrorCommunityCatalogSchemaInvalid          = "COMMUNITY_CATALOG_SCHEMA_INVALID"
	ErrorNetworkRewriteInvalid                  = "NETWORK_REWRITE_INVALID"
	ErrorDeploymentNodeInvalid                  = messages.ErrorDeploymentNodeInvalid
	ErrorAutomaticPlacementUnavailable          = messages.ErrorAutomaticPlacementUnavailable
	ErrorAutomaticPlacementStale                = messages.ErrorAutomaticPlacementStale
	ErrorTargetVendorInvalid                    = messages.ErrorTargetVendorInvalid
	ErrorBenchmarkProbeFailed                   = "BENCHMARK_PROBE_FAILED"
	ErrorBenchmarkEndpointNotFound              = "BENCHMARK_ENDPOINT_NOT_FOUND"
	ErrorHTTPDirectoryUnreachable               = "HTTP_DIRECTORY_UNREACHABLE"
	ErrorHTTPDirectoryNotAModel                 = "HTTP_DIRECTORY_NOT_A_MODEL"
	ErrorHTTPDirectoryTooLarge                  = "HTTP_DIRECTORY_TOO_LARGE"
	ErrorModelSourceRequiresNewerAgent          = messages.ErrorModelSourceRequiresNewerAgent
	ErrorModelSourceURLRequired                 = "MODEL_SOURCE_URL_REQUIRED"
	ErrorModelSourceURLInvalid                  = "MODEL_SOURCE_URL_INVALID"
	ErrorImageArchiveURLRequired                = "IMAGE_ARCHIVE_URL_REQUIRED"
	ErrorImageArchiveURLInvalid                 = "IMAGE_ARCHIVE_URL_INVALID"
	ErrorImageArchiveURLUnreachable             = "IMAGE_ARCHIVE_URL_UNREACHABLE"
	ErrorImageArchiveURLUpstreamStatus          = "IMAGE_ARCHIVE_URL_UPSTREAM_STATUS"
	ErrorImageArchiveURLPolicyDenied            = "IMAGE_ARCHIVE_URL_POLICY_DENIED"
	ErrorImageArchiveURLPolicyBlocked           = "IMAGE_ARCHIVE_URL_POLICY_BLOCKED"
	ErrorImageArchiveURLNotDeclared             = "IMAGE_ARCHIVE_URL_NOT_DECLARED"
	ErrorMonitorRangeInvalid                    = "MONITOR_RANGE_INVALID"
	ErrorMonitorGroupInvalid                    = "MONITOR_GROUP_INVALID"
	ErrorMonitorAggregationInvalid              = "MONITOR_AGGREGATION_INVALID"
	ErrorMonitorSeriesTooLarge                  = "MONITOR_SERIES_TOO_LARGE"
	ErrorMonitorRequestsViewInvalid             = "MONITOR_REQUESTS_VIEW_INVALID"
	ErrorMonitorRequestsQueryInvalid            = "MONITOR_REQUESTS_QUERY_INVALID"
	// ErrorMonitorDeploymentDeleted: the deployment ({deployment_id}) was
	// deleted; {name} and {served_name} are the names it left.
	ErrorMonitorDeploymentDeleted = "MONITOR_DEPLOYMENT_DELETED"
	// ErrorLaunchConfigConflict is produced by deploy.Materialize, not by this
	// package. It is spelled out here because this file is the inventory every
	// client and the locale check read to learn which codes the API can return,
	// and an alias would be invisible to both. The two constants are asserted
	// equal by TestLaunchConfigConflictCodeMatchesItsProducer.
	ErrorLaunchConfigConflict = "LAUNCH_CONFIG_CONFLICT"
	// ErrorLaunchConfigUnsetConflict is likewise produced by deploy.Materialize.
	ErrorLaunchConfigUnsetConflict = "LAUNCH_CONFIG_UNSET_CONFLICT"
	// The docker argument codes are produced by deploy.ValidateContainerDockerArgs
	// and spelled out here for the same inventory reason; the pairs are asserted
	// equal by TestDockerArgCodesMatchTheirProducer.
	ErrorDockerArgPlatformManaged    = "DOCKER_ARG_PLATFORM_MANAGED"
	ErrorDockerArgUseShmField        = "DOCKER_ARG_USE_SHM_FIELD"
	ErrorDockerArgUseEnvField        = "DOCKER_ARG_USE_ENV_FIELD"
	ErrorDockerArgUseMountField      = "DOCKER_ARG_USE_MOUNT_FIELD"
	ErrorDockerArgUseEntrypointField = "DOCKER_ARG_USE_ENTRYPOINT_FIELD"
	ErrorShmSizeWithHostIPC          = "SHM_SIZE_WITH_HOST_IPC"
	// The data-parallel codes are produced by each engine's topology
	// validation in deploy and spelled out here for the same inventory reason;
	// TestDataParallelCodesMatchTheirProducer asserts the pairs equal.
	ErrorDataParallelGPUCount         = "DATA_PARALLEL_GPU_COUNT"
	ErrorDataParallelAttentionDivisor = "DATA_PARALLEL_ATTENTION_DIVISOR"
	ErrorFieldInvalid                 = messages.ErrorFieldInvalid
	ErrorEngineInvalid                = messages.ErrorEngineInvalid
	ErrorExtraArgsInvalid             = messages.ErrorExtraArgsInvalid
	// ErrorExtraArgsJSONInvalid answers a JSON-valued engine flag typed into
	// extra_args with a value that is not JSON ({flag}, {value}); it is
	// produced by deploy.ValidateExtraArgsJSON.
	ErrorExtraArgsJSONInvalid = "EXTRA_ARGS_JSON_INVALID"
	// The Ascend sleep-mode codes are produced by deploy's vLLM launch
	// preparation and spelled out here for the same inventory reason;
	// TestAscendSleepCodesMatchTheirProducer asserts the pairs equal.
	ErrorAscendSleepAdditionalConfigInvalid = "ASCEND_SLEEP_ADDITIONAL_CONFIG_INVALID"
	ErrorAscendSleepWeightNZConflict        = "ASCEND_SLEEP_WEIGHT_NZ_CONFLICT"
	ErrorAscendSleepExpandableSegments      = "ASCEND_SLEEP_EXPANDABLE_SEGMENTS"
	ErrorMountContainerPath                 = messages.ErrorMountContainerPath
	ErrorMountHostPath                      = messages.ErrorMountHostPath
	ErrorMountTargetNested                  = messages.ErrorMountTargetNested
	// ErrorQuickParamJSON and ErrorQuickParamPath are likewise produced by
	// deploy's launch validation, on json- and path-typed parameters.
	ErrorAcceleratorFeatureUnsupported      = messages.ErrorAcceleratorFeatureUnsupported
	ErrorAcceleratorDeviceSelectionRequired = messages.ErrorAcceleratorDeviceSelectionRequired
	ErrorQuickParamJSON                     = "QUICK_PARAM_JSON_INVALID"
	ErrorQuickParamPath                     = "QUICK_PARAM_PATH_INVALID"
)

// ErrorAIDefaultRequired answers a patch that would clear the default mark on
// the only stored AI service. The console shows the switch on every row, and
// clearing the last mark would leave the assistant with services stored and none
// of them in use - a state the console cannot show and the operator cannot
// repair except by marking another row first.
const ErrorAIDefaultRequired = "AI_DEFAULT_REQUIRED"

func productError(code, fallback string, params map[string]any) error {
	return messages.NewProductError(code, fallback, params)
}

func wrapProductError(code, fallback string, params map[string]any, cause error) error {
	return messages.WrapProductError(code, fallback, params, cause)
}

func writeProductError(w http.ResponseWriter, status int, code, fallback string, params map[string]any) {
	writeErr(w, status, productError(code, fallback, params))
}

// Automatic placement of multi-node and PD deployments (placement_mode
// "auto"; deployments_auto_units.go).
const (
	ErrorUnitPlacementNone        = messages.ErrorUnitPlacementNone
	ErrorAutoPlacementTPRequired  = messages.ErrorAutoPlacementTPRequired
	ErrorAutoPlacementPDMultiNode = messages.ErrorAutoPlacementPDMultiNode
	// ErrorPlacementModeShape: PUT /deployments/{id}/placement-mode on a
	// single-node deployment, which states its placement in its node
	// requirements.
	ErrorPlacementModeShape = "PLACEMENT_MODE_SHAPE"
)

// Deployment edit codes (GET /deployments/{id}/draft, PUT /deployments/{id}).
const (
	// ErrorDeploymentFieldNotEditable: an edit changes a field an existing
	// deployment cannot change ({field}); cloning the deployment changes it.
	ErrorDeploymentFieldNotEditable = "DEPLOYMENT_FIELD_NOT_EDITABLE"
	// ErrorDeploymentChanged: the deployment's configuration changed since
	// the draft the edit was made on was read.
	ErrorDeploymentChanged = "DEPLOYMENT_CHANGED"
	// ErrorDeploymentEditApplyInvalid: the edit asks to reach the instances
	// in a way ({apply}) its plan does not offer ({options}).
	ErrorDeploymentEditApplyInvalid = "DEPLOYMENT_EDIT_APPLY_INVALID"
)

// dGPU quota codes: why a deployment's dGPU quota could not be changed
// (PUT /deployments/{id}/dgpu).
const (
	// ErrorDGPUQuotaInvalid: the requested quota is not one a slice can have
	// ({field}, {value}).
	ErrorDGPUQuotaInvalid = "DGPU_QUOTA_INVALID"
	// ErrorDGPUQuotaNotPartitioned: the deployment runs on whole cards; it
	// has no dGPU quota to change.
	ErrorDGPUQuotaNotPartitioned = "DGPU_QUOTA_NOT_PARTITIONED"
	// ErrorDGPUQuotaNotEditable: the deployment places its ranks one by one
	// ({shape}: multi_node or pd); its instances are not replaced as one.
	ErrorDGPUQuotaNotEditable = "DGPU_QUOTA_NOT_EDITABLE"
	// ErrorDGPUQuotaAsleep: a replica is asleep or ordered to sleep; the
	// replacement would wake it, so it is woken first.
	ErrorDGPUQuotaAsleep = "DGPU_QUOTA_ASLEEP"
	// ErrorDGPUQuotaNoRoom: GPU {gpu} of {node} has {free_gib} GiB for the
	// deployment's slices once their claims are handed back; at the new
	// quota they need {need_gib} GiB.
	ErrorDGPUQuotaNoRoom = "DGPU_QUOTA_NO_ROOM"
)

// Node requirement codes planning raises when it checks where a deployment
// may run.
const (
	ErrorNodeRequirementsInvalid     = messages.ErrorNodeRequirementsInvalid
	ErrorNodeRequirementsUnmet       = messages.ErrorNodeRequirementsUnmet
	ErrorPlacementStrategyInvalid    = messages.ErrorPlacementStrategyInvalid
	ErrorDeploymentNeverFits         = messages.ErrorDeploymentNeverFits
	ErrorNodeRequirementsNotEditable = messages.ErrorNodeRequirementsNotEditable
)

// Replica codes planning raises when it checks a deployment's replicas.
const (
	ErrorReplicasTooMany             = messages.ErrorReplicasTooMany
	ErrorReplicaUnitRanks            = messages.ErrorReplicaUnitRanks
	ErrorReplicaUnitNode             = messages.ErrorReplicaUnitNode
	ErrorReplicaUnitNodeOffline      = messages.ErrorReplicaUnitNodeOffline
	ErrorReplicaUnitGPUs             = messages.ErrorReplicaUnitGPUs
	ErrorReplicaUnitGPUsInsufficient = messages.ErrorReplicaUnitGPUsInsufficient
	ErrorReplicaUnitRequirements     = messages.ErrorReplicaUnitRequirements
	ErrorReplicaLeaderShared         = messages.ErrorReplicaLeaderShared
	ErrorInstanceNotServing          = messages.ErrorInstanceNotServing
	ErrorSleepRoleCannotEmpty        = messages.ErrorSleepRoleCannotEmpty
)

// containsProductCode reports whether err is, or wraps, the product error code.
func containsProductCode(err error, code string) bool {
	var product *messages.ProductError
	return err != nil && errors.As(err, &product) && product.Code() == code
}

// Storage files codes (ADR 0053; storage_files*.go): why browsing, a transfer
// or a change to a storage's files was refused.
const (
	ErrorStorageFilePathInvalid    = "STORAGE_FILE_PATH_INVALID"
	ErrorStorageFileNodeRequired   = "STORAGE_FILE_NODE_REQUIRED"
	ErrorStorageFileRegionReadonly = "STORAGE_FILE_REGION_READONLY"
	ErrorStorageFileNotFound       = "STORAGE_FILE_NOT_FOUND"
	ErrorStorageFileExists         = "STORAGE_FILE_EXISTS"
	// ErrorStorageFileAgentUnsupported: the agent on {node} predates storage
	// files.
	ErrorStorageFileAgentUnsupported = "STORAGE_FILE_AGENT_UNSUPPORTED"
	ErrorStorageFileExtractFailed    = "STORAGE_FILE_EXTRACT_FAILED"
	// ErrorStorageFileDataLaneUnavailable: {node} has no data lane attached
	// for a transfer now; retryable.
	ErrorStorageFileDataLaneUnavailable = "STORAGE_FILE_DATA_LANE_UNAVAILABLE"
	// ErrorStorageFileOperationFailed: the agent on {node} failed for a reason
	// without a code of its own, or the transfer broke.
	ErrorStorageFileOperationFailed = "STORAGE_FILE_OPERATION_FAILED"
)

// Batch codes (GET /batches, POST /batches, the Batch API's console side).
const (
	// ErrorBatchNotFound: no such batch, file or output file for the caller.
	ErrorBatchNotFound = "BATCH_NOT_FOUND"
	// ErrorBatchNotCancellable: the batch has finished or is assembling its
	// results ({status}).
	ErrorBatchNotCancellable = "BATCH_NOT_CANCELLABLE"
	// ErrorBatchFileInvalid: the uploaded file is not a valid batch input:
	// {line} of it (0 for the file as a whole), {reason} the check that failed
	// and {detail} its English text.
	ErrorBatchFileInvalid = "BATCH_FILE_INVALID"
	// ErrorBatchFileTooLarge: the file is over the {limit_mb} megabytes allowed.
	ErrorBatchFileTooLarge = "BATCH_FILE_TOO_LARGE"
	// ErrorBatchRequestInvalid: a field of the request ({field}) is not valid.
	ErrorBatchRequestInvalid = "BATCH_REQUEST_INVALID"
	// ErrorBatchModelUnknown: no active deployment serves the {model} the batch
	// was asked to send its requests to. Models a file's requests name are
	// refused with messages.ErrorBatchFileModelsUnknown instead.
	ErrorBatchModelUnknown = "BATCH_MODEL_UNKNOWN"
	// ErrorBatchModelEndpoint: the deployments serving the {model} the batch
	// was asked to send its requests to do not serve its {endpoint}.
	ErrorBatchModelEndpoint = "BATCH_MODEL_ENDPOINT"
)

// WarningBatchModelSleepHeld is the warning on a created batch whose {model}
// sleeps by an operator's or a schedule's order, which requests do not wake.
const WarningBatchModelSleepHeld = "BATCH_MODEL_SLEEP_HELD"

// WarningBatchModelWaits is the warning on a created batch whose {model} is
// {state} (offline, failed or stopped): its requests wait until it serves.
const WarningBatchModelWaits = "BATCH_MODEL_WAITS"

// ErrorServerStorageFull answers any request whose write failed because the
// server's disk is full (store.IsStorageFull), whichever handler it reached.
const ErrorServerStorageFull = "SERVER_STORAGE_FULL"

// Kubernetes cluster codes (/kube/clusters, /kube/enroll).
const (
	// ErrorKubeClusterInvalid: a cluster parameter cannot be used
	// ({field}, {reason}).
	ErrorKubeClusterInvalid = "KUBE_CLUSTER_INVALID"
	// ErrorKubeClusterNameTaken: another cluster already has {name}.
	ErrorKubeClusterNameTaken = "KUBE_CLUSTER_NAME_TAKEN"
	// ErrorKubeClusterHasNodes: the cluster still has nodes; remove them first.
	ErrorKubeClusterHasNodes = "KUBE_CLUSTER_HAS_NODES"
	// ErrorKubeEnrollRefused: the enrollment token is unknown, revoked or
	// belongs to another cluster ({reason}: unknown_token | other_cluster).
	ErrorKubeEnrollRefused = "KUBE_ENROLL_REFUSED"
	// ErrorKubeClusterSourceTaken: an entry was already made from cluster
	// {cluster} of the cloud account.
	ErrorKubeClusterSourceTaken = "KUBE_CLUSTER_SOURCE_TAKEN"
	// ErrorKubeClusterNoSource: the entry was not made from a cloud
	// account's cluster, so the server has no way into the cluster; apply
	// the manifest with kubectl.
	ErrorKubeClusterNoSource = "KUBE_CLUSTER_NO_SOURCE"
	// ErrorKubeClusterOperationFailed: applying to or removing from the
	// cluster failed at {step} (kubeconfig | api_server): {reason}.
	ErrorKubeClusterOperationFailed = "KUBE_CLUSTER_OPERATION_FAILED"
	// ErrorProviderInstanceInCluster: the instance is a node of the
	// account's Kubernetes cluster {cluster}; connect the cluster instead.
	ErrorProviderInstanceInCluster = "PROVIDER_INSTANCE_IN_CLUSTER"
)
