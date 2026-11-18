package messages

import "gpuworks.ai/gpuworks/internal/proto"

// Product error codes of the deployment use cases (internal/server/deployment):
// creating and evaluating a deployment, and the placement, model, image and
// storage checks they run. The API package aliases the ones its own handlers
// answer with too.
const (
	ErrorDistributedDegreeInvalid  = "DISTRIBUTED_DEGREE_INVALID"
	ErrorDistributedRecommendation = "DISTRIBUTED_PARALLELISM_UNAVAILABLE"
)

const (
	// ErrorServedNameInUse: another deployment serves under the name.
	ErrorServedNameInUse                        = "SERVED_NAME_IN_USE"
	ErrorNodeOffline                            = "NODE_OFFLINE"
	ErrorReplicasNegative                       = "REPLICAS_NEGATIVE"
	ErrorRoutingPolicyInvalid                   = "ROUTING_POLICY_INVALID"
	ErrorDistributedReplicaCount                = "DISTRIBUTED_REPLICA_COUNT"
	ErrorDistributedNodeCount                   = "DISTRIBUTED_NODE_COUNT"
	ErrorDistributedTPRequired                  = "DISTRIBUTED_TP_REQUIRED"
	ErrorMasterPortInvalid                      = "MASTER_PORT_INVALID"
	ErrorDistributedDuplicateNode               = "DISTRIBUTED_DUPLICATE_NODE"
	ErrorDistributedNodeOffline                 = "DISTRIBUTED_NODE_OFFLINE"
	ErrorDistributedGPUCount                    = "DISTRIBUTED_GPU_COUNT"
	ErrorDistributedParallelIndivisible         = "DISTRIBUTED_PARALLEL_INDIVISIBLE"
	ErrorDistributedPartitionBackendUnavailable = "DISTRIBUTED_PARTITION_BACKEND_UNAVAILABLE"
	ErrorAcquisitionCandidateStale              = "ACQUISITION_CANDIDATE_STALE"
	ErrorAcquisitionImageRequired               = "ACQUISITION_IMAGE_REQUIRED"
	ErrorComputeNetworkNone                     = "COMPUTE_NETWORK_NONE"
	ErrorComputeNetworkMixed                    = "COMPUTE_NETWORK_MIXED"
	ErrorComputeNetworkMismatch                 = "COMPUTE_NETWORK_MISMATCH"
	ErrorModelFilePathOccupied                  = "MODEL_FILE_PATH_OCCUPIED"
	ErrorModelSourceUnavailable                 = "MODEL_SOURCE_UNAVAILABLE"
	ErrorPreflightMinimumNodes                  = "PREFLIGHT_MINIMUM_NODES"
	ErrorPreflightMaximumNodes                  = "PREFLIGHT_MAXIMUM_NODES"
	ErrorPreflightNodeRequired                  = "PREFLIGHT_NODE_REQUIRED"
	ErrorPreflightNodeWhitespace                = "PREFLIGHT_NODE_WHITESPACE"
	ErrorPreflightDuplicateNode                 = "PREFLIGHT_DUPLICATE_NODE"
	ErrorPreflightPortInvalid                   = "PREFLIGHT_PORT_INVALID"
	ErrorPreflightDurationInvalid               = "PREFLIGHT_DURATION_INVALID"
	ErrorPreflightNodeInvalid                   = "PREFLIGHT_NODE_INVALID"
	ErrorPreflightNodeOffline                   = "PREFLIGHT_NODE_OFFLINE"
	ErrorStorageDisabled                        = "STORAGE_DISABLED"
	ErrorStorageNotWritable                     = "STORAGE_NOT_WRITABLE"
	ErrorStorageUnavailableOnNode               = "STORAGE_UNAVAILABLE_ON_NODE"
	ErrorDeploymentNodeInvalid                  = "DEPLOYMENT_NODE_INVALID"
	ErrorAutomaticPlacementUnavailable          = "AUTOMATIC_PLACEMENT_UNAVAILABLE"
	ErrorAutomaticPlacementStale                = "AUTOMATIC_PLACEMENT_STALE"
	ErrorTargetVendorInvalid                    = "TARGET_VENDOR_INVALID"
	ErrorModelSourceRequiresNewerAgent          = "MODEL_SOURCE_REQUIRES_NEWER_AGENT"
	// ErrorFieldInvalid carries a request error that is attributable to a form
	// field but has no more specific stable code; the detail param holds the
	// rule's English fallback.
	ErrorFieldInvalid = "FIELD_INVALID"
	// ErrorEngineInvalid answers an engine the registry does not know.
	ErrorEngineInvalid = "ENGINE_INVALID"
	// ErrorExtraArgsInvalid answers extra-args text the parser cannot read.
	ErrorExtraArgsInvalid = "EXTRA_ARGS_INVALID"
	// ErrorMountContainerPath and ErrorMountHostPath answer extra-mount rows
	// whose path is not an absolute non-root directory; ErrorMountTargetNested
	// answers two mount targets that contain each other.
	ErrorMountContainerPath = "MOUNT_CONTAINER_PATH"
	ErrorMountHostPath      = "MOUNT_HOST_PATH"
	ErrorMountTargetNested  = "MOUNT_TARGET_NESTED"
	// ErrorMountTargetReserved answers a mount target that overlaps a path
	// the platform mounts its own files at (/gpuworks: the peers file, the
	// pre-start wrapper). Params: target, reserved.
	ErrorMountTargetReserved = "MOUNT_TARGET_RESERVED"
	// ErrorMountModelNotFound answers an extra-mount row naming a model that
	// is not registered (any more); model_id is the id the row states.
	ErrorMountModelNotFound = "MOUNT_MODEL_NOT_FOUND"
	// ErrorMountVolumeInvalid answers an extra-mount row whose volume is not
	// a runtime volume name (proto.ValidVolumeName); resource is the volume.
	// The agent refuses such a mount with the same code.
	ErrorMountVolumeInvalid = proto.ErrorMountVolumeInvalid
	// ErrorAutoPlacementTPRequired: an automatic multi-node request states no
	// placement.tp, so how many free GPUs each node needs is unknown.
	ErrorAutoPlacementTPRequired = "AUTO_PLACEMENT_TP_REQUIRED"
	// ErrorTemplateModelMismatch: the request selects a model (model_id)
	// whose repository id serves none of the template spec's weight sources
	// (repo_id@revision is the source the spec resolved). The server refuses
	// the choice rather than substituting another model.
	ErrorTemplateModelMismatch = "TEMPLATE_MODEL_MISMATCH"
	// ErrorRankWeightsModelNotRankZero: the request names a model (model_id)
	// for a spec with rank-scoped weights, and the model is not rank 0's
	// subfolder (want) of the repository but subfolder: the named model is
	// what rank 0 mounts.
	ErrorRankWeightsModelNotRankZero = "RANK_WEIGHTS_MODEL_NOT_RANK_ZERO"
	// ErrorRankWeightsPDUnsupported: a spec with rank-scoped weights is
	// deployed as PD, whose roles' ranks do not number one group.
	ErrorRankWeightsPDUnsupported = "RANK_WEIGHTS_PD_UNSUPPORTED"
	// ErrorManualCommandRankBlind: a multi-node or PD request's manual
	// command references none of the variables that tell its launches apart,
	// so every rank would start as the same rank in the same role. missing
	// lists the prefixed variable names the command has to reference.
	ErrorManualCommandRankBlind = "MANUAL_COMMAND_RANK_BLIND"
	// ErrorCommandOverrideRankBlind: a multi-node or PD request overrides
	// Cmd outside manual mode. Such an override is one argv every rank would
	// run as written; only a manual command is expanded per rank.
	ErrorCommandOverrideRankBlind = "COMMAND_OVERRIDE_RANK_BLIND"
)
