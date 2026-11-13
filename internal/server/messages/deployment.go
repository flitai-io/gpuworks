package messages

// Product error codes of the deployment use cases (internal/server/deployment):
// creating and evaluating a deployment, and the placement, model, image and
// storage checks they run. The API package aliases the ones its own handlers
// answer with too.
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
	// ErrorAutoPlacementTPRequired: an automatic multi-node request states no
	// placement.tp, so how many free GPUs each node needs is unknown.
	ErrorAutoPlacementTPRequired = "AUTO_PLACEMENT_TP_REQUIRED"
)
