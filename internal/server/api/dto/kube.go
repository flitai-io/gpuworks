package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// KubeCluster is a Kubernetes cluster whose GPU nodes join through one
// DaemonSet. ApplyCommand carries the enrollment token and is present for
// administrators only.
type KubeCluster struct {
	ID         uint   `json:"id"`
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	ServerURL  string `json:"server_url"`
	AgentImage string `json:"agent_image"` // empty: the published image from AgentImageSource
	// AgentImageSource is the registry of the published agent image the
	// entry uses without an image of its own; EffectiveAgentImage is the
	// image its manifest names.
	AgentImageSource    string `json:"agent_image_source"`
	EffectiveAgentImage string `json:"effective_agent_image"`
	NodeSelector        string `json:"node_selector"`
	RuntimeClass        string `json:"runtime_class"`     // empty: the cluster's "nvidia" class when it has one, else the default runtime
	ImagePullSecret     string `json:"image_pull_secret"` // empty: the agent image is pulled without credentials
	ContainerdSocket    string `json:"containerd_socket"` // empty: the agents find it
	StateDir            string `json:"state_dir"`
	APIServer           string `json:"api_server"` // empty: the agents use the cluster's "kubernetes" Service
	EnginePorts         string `json:"engine_ports"`
	IdleReturnSeconds   int    `json:"idle_return_seconds"` // -1 keeps borrowed cards
	// Priority and AllowPreemption are the hold and instance pods'
	// PriorityClass value and whether the scheduler may evict lower-priority
	// pods for them.
	Priority        int    `json:"priority"`
	AllowPreemption bool   `json:"allow_preemption"`
	ClusterUID      string `json:"cluster_uid"` // empty until the first node enrolls
	Revoked         bool   `json:"revoked"`
	NodeCount       int    `json:"node_count"`
	// Vendors counts the joined nodes by the vendor of their cards, most
	// first; every agent finds out its own node's.
	Vendors      []KubeVendorNodes `json:"vendors"`
	ApplyCommand string            `json:"apply_command,omitempty"`
	// RemoveCommands take the entry's agents, and the pods they started, out
	// of the cluster with kubectl, in order; for administrators. The server
	// does the same itself for an entry it can reach (POST …/remove).
	RemoveCommands []string `json:"remove_commands,omitempty"`
	// Stalled: no node has joined for longer than the dictionary's
	// stalled_after_seconds (GET /kube/defaults), so it is worth looking at
	// why. Never for a revoked cluster or one a node joined.
	Stalled bool `json:"stalled"`
	// CanApply and CanRemove say whether the server can apply the manifest
	// to the cluster, or take it out again (POST .../apply, .../remove).
	// OperationBlocked is why not: no_source (made by hand, so the server has
	// no way into it), private_api (the cloud says the API server has no
	// public address) or revoked (only applying is refused; removing still
	// works).
	// PoolIDs are the cloud node pools an entry made from a cloud cluster
	// holds, sorted; the DaemonSet runs on the nodes of those pools. Empty for
	// an entry made by hand, which selects nodes by NodeSelector.
	PoolIDs []string `json:"pool_ids"`
	// NamespaceSharedWith names the other entries, sorted, that use this
	// entry's namespace and may be of the same Kubernetes cluster (no node
	// has joined one of them yet, or both are of the same cluster). In one
	// cluster the entry applied later takes the other's agents over, so the
	// console warns; entries known to be of different clusters are not
	// listed.
	NamespaceSharedWith []string `json:"namespace_shared_with"`
	CanApply            bool     `json:"can_apply"`
	CanRemove           bool     `json:"can_remove"`
	OperationBlocked    string   `json:"operation_blocked,omitempty"`
	// Source is the cloud account cluster the entry was made from; the
	// server can then apply the manifest itself (POST …/apply).
	Source    *KubeClusterSource `json:"source,omitempty"`
	CreatedAt time.Time          `json:"created_at"`
}

// KubeDefaults is what the cluster form needs from the server: the values an
// omitted field takes, the one value that means "never return", and the
// vendors with what applies to each. A client shows the defaults as hints and
// sends only what the user typed.
type KubeDefaults struct {
	Namespace           string `json:"namespace"`
	StateDir            string `json:"state_dir"`
	EnginePorts         string `json:"engine_ports"`
	IdleReturnSeconds   int    `json:"idle_return_seconds"`
	IdleReturnKeep      int    `json:"idle_return_keep"` // the idle_return_seconds value that keeps borrowed cards
	AgentImage          string `json:"agent_image"`
	StalledAfterSeconds int    `json:"stalled_after_seconds"`
	// AgentImageSource is the registry an entry made by hand pulls the
	// published agent image from when it names no image of its own;
	// AgentImageSources lists them all, the default first.
	AgentImageSource  string                 `json:"agent_image_source"`
	AgentImageSources []KubeAgentImageSource `json:"agent_image_sources"`
	// Priority is the PriorityClass value of the hold and instance pods (0 to
	// PriorityMax, larger first; ordinary pods are 0); AllowPreemption lets
	// the scheduler evict lower-priority pods for them, off by default.
	Priority        int  `json:"priority"`
	PriorityMax     int  `json:"priority_max"`
	AllowPreemption bool `json:"allow_preemption"`
}

// KubeAgentImageSource is a registry the agent image is published to.
type KubeAgentImageSource struct {
	ID    string `json:"id"`    // "dockerhub", "swr", "quay"
	Image string `json:"image"` // the image there, at the server's runtime revision
}

// KubeVendorNodes is how many of an entry's nodes have one vendor's cards.
type KubeVendorNodes struct {
	Vendor string `json:"vendor"` // "" for nodes that reported no cards
	Nodes  int    `json:"nodes"`
}

// KubeClusterRequest creates a cluster. Empty optional fields take the
// defaults the response shows.
type KubeClusterRequest struct {
	Name       string `json:"name"`
	ServerURL  string `json:"server_url"`
	Namespace  string `json:"namespace,omitempty"`
	AgentImage string `json:"agent_image,omitempty"`
	// AgentImageSource is the registry of the published agent image when
	// AgentImage is empty; empty is Docker Hub for an entry made by hand, and
	// for one made from a cloud cluster the registry its region reaches.
	AgentImageSource  string `json:"agent_image_source,omitempty"`
	NodeSelector      string `json:"node_selector,omitempty"`
	RuntimeClass      string `json:"runtime_class,omitempty"`
	ImagePullSecret   string `json:"image_pull_secret,omitempty"`
	ContainerdSocket  string `json:"containerd_socket,omitempty"`
	StateDir          string `json:"state_dir,omitempty"`
	APIServer         string `json:"api_server,omitempty"`
	EnginePorts       string `json:"engine_ports,omitempty"`
	IdleReturnSeconds *int   `json:"idle_return_seconds,omitempty"`
	// Priority and AllowPreemption set the hold and instance pods'
	// PriorityClass; omitted take GET /kube/defaults.
	Priority        *int  `json:"priority,omitempty"`
	AllowPreemption *bool `json:"allow_preemption,omitempty"`
	// ProviderID and ProviderClusterID name the cloud account cluster the
	// entry is made from (GET /providers/{id}/clusters); both or neither.
	ProviderID        *uint  `json:"provider_id,omitempty"`
	ProviderClusterID string `json:"provider_cluster_id,omitempty"`
	// ProviderPoolIDs are the cluster's GPU node pools the entry holds:
	// required, and only, with a source. They must all be of one vendor.
	ProviderPoolIDs []string `json:"provider_pool_ids,omitempty"`
}

// KubeClusterPoolsRequest replaces the node pools an entry made from a cloud
// cluster holds.
type KubeClusterPoolsRequest struct {
	ProviderPoolIDs []string `json:"provider_pool_ids"`
	// ConfirmDropInstances accepts that nodes this change leaves out of the
	// entry's selection still run instances, and that those instances are
	// asked to stop first (see KubeSelectionDrop).
	ConfirmDropInstances bool `json:"confirm_drop_instances,omitempty"`
}

// KubeClusterPools is the answer to a node pools change: the entry as stored
// (the fields of KubeCluster, at the top level as before) and the instances
// that were asked to stop because their nodes left the selection.
type KubeClusterPools struct {
	KubeCluster
	StoppedInstances []KubeStoppedInstance `json:"stopped_instances"`
}

// KubeStoppedInstance is an instance a confirmed change of an entry's node
// selection asked to stop, because its node leaves the selection. The stop is
// requested, not finished: the agent keeps running until the manifest is
// applied again.
type KubeStoppedInstance struct {
	InstanceUUID string `json:"instance_uuid"`
	Deployment   string `json:"deployment"`
	NodeUUID     string `json:"node_uuid"`
}

// KubeSelectionDrop is a node a narrower node selection would leave out while
// it still runs instances, as the 409 KUBE_SELECTION_DROPS_INSTANCES lists
// them: its agent is removed when the manifest is applied again, and the
// instances keep running there unmanaged.
type KubeSelectionDrop struct {
	NodeUUID    string   `json:"node_uuid"`
	NodeName    string   `json:"node_name"`
	KubeNode    string   `json:"kube_node"`
	Deployments []string `json:"deployments"`
}

// KubeEnrollRequest is what an agent of a cluster's DaemonSet sends to trade
// the enrollment token for its node token.
type KubeEnrollRequest struct {
	Token      string `json:"token"`
	ClusterUID string `json:"cluster_uid"`
	NodeName   string `json:"node_name"`
}

// KubeEnrollResponse is the node the agent now is.
type KubeEnrollResponse struct {
	NodeUUID          string `json:"node_uuid"`
	NodeToken         string `json:"node_token"`
	IdleReturnSeconds int    `json:"idle_return_seconds"`
}

// KubeClusterSource is the cloud account cluster an entry was made from.
type KubeClusterSource struct {
	ProviderID   uint   `json:"provider_id"`
	ProviderName string `json:"provider_name"` // empty when the account is gone from the listing
	ProviderKind string `json:"provider_kind"`
	ClusterID    string `json:"cluster_id"`
}

// ProviderCluster is a managed Kubernetes cluster in a cloud account
// (provider.Cluster) and the entry made from it, if any.
type ProviderCluster struct {
	ExternalID string                `json:"external_id"`
	Name       string                `json:"name"`
	Region     string                `json:"region"`
	Version    string                `json:"version"`
	State      string                `json:"state"`
	Running    bool                  `json:"running"`
	PublicAPI  bool                  `json:"public_api"`
	GPUPools   []ProviderClusterPool `json:"gpu_pools"`
	Entry      *KubeClusterRef       `json:"entry,omitempty"`
	// AgentImageSource is the registry an entry made from the cluster pulls
	// the published agent image from unless the request names another: the
	// one its region reaches.
	AgentImageSource string `json:"agent_image_source"`
}

// ProviderClusterPool is one GPU node pool of a ProviderCluster.
type ProviderClusterPool struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	GPUModel string `json:"gpu_model"`
	GPUCount int    `json:"gpu_count"`
	Nodes    int    `json:"nodes"`
	Vendor   string `json:"vendor"`
	// InEntry: the cluster's entry holds this pool, so the DaemonSet runs on
	// its nodes.
	InEntry bool `json:"in_entry"`
}

// KubeClusterRef names a cluster entry.
type KubeClusterRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// KubeClusterOperationRequest asks for an apply or a removal.
type KubeClusterOperationRequest struct {
	// DryRun is required: an operation on a cluster is never taken by
	// default.
	DryRun *bool `json:"dry_run"`
}

// KubeClusterOperation is what an apply or a removal did to each object of
// the manifest. A failed operation carries the code, error and params of a
// product error beside the results it got to.
type KubeClusterOperation struct {
	DryRun  bool                      `json:"dry_run"`
	Results []KubeObjectResult        `json:"results"`
	Code    string                    `json:"code,omitempty"`
	Error   string                    `json:"error,omitempty"`
	Params  *KubeOperationErrorParams `json:"params,omitempty"`
}

// KubeOperationErrorParams says where an operation failed: "kubeconfig"
// (the cloud would not issue one, or it cannot be used here) or
// "api_server" (the cluster refused).
type KubeOperationErrorParams struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// KubeObjectResult is one object of an operation (kubeapply.Result).
type KubeObjectResult struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Action: create | update | delete | absent | kept | failed | not_tried | dependent.
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// KubeClusterUpdate edits an entry (PUT /kube/clusters/{uuid}): each field
// given replaces the entry's, omitted ones keep it, and an empty string
// clears an optional one. The namespace, server URL and state directory are
// fixed when the entry is made; an entry made from a cloud cluster selects
// its nodes by node pool (PUT …/pools), not by node selector.
type KubeClusterUpdate struct {
	Name              *string `json:"name,omitempty"`
	NodeSelector      *string `json:"node_selector,omitempty"`
	AgentImage        *string `json:"agent_image,omitempty"`
	AgentImageSource  *string `json:"agent_image_source,omitempty"`
	ImagePullSecret   *string `json:"image_pull_secret,omitempty"`
	RuntimeClass      *string `json:"runtime_class,omitempty"`
	ContainerdSocket  *string `json:"containerd_socket,omitempty"`
	APIServer         *string `json:"api_server,omitempty"`
	EnginePorts       *string `json:"engine_ports,omitempty"`
	IdleReturnSeconds *int    `json:"idle_return_seconds,omitempty"`
	Priority          *int    `json:"priority,omitempty"`
	AllowPreemption   *bool   `json:"allow_preemption,omitempty"`
	// ConfirmDropInstances accepts that nodes a narrower node_selector leaves
	// out still run instances (see KubeSelectionDrop), and that those
	// instances are asked to stop first; it is not a setting of the entry.
	ConfirmDropInstances bool `json:"confirm_drop_instances,omitempty"`
}

// KubeClusterEdit is the answer to an edit: the entry as stored, the fields
// that changed (their JSON names, sorted) and how the cluster catches up.
type KubeClusterEdit struct {
	Cluster KubeCluster `json:"cluster"`
	Changed []string    `json:"changed"`
	// Reapply: the entry's manifest changed, so the cluster runs the old one
	// until it is applied again (POST …/apply for an entry the server can
	// apply, the commands below otherwise).
	Reapply bool `json:"reapply"`
	// Commands bring a cluster the server cannot reach in line: one line
	// that deletes the entry's PriorityClass and applies the manifest, since
	// Kubernetes does not change a class's value or preemption policy in
	// place and an earlier edit of them may not have been applied. Only for
	// administrators of an entry made by hand that is not revoked; empty
	// when the manifest did not change.
	Commands []string `json:"commands"`
	// StoppedInstances are the instances a confirmed narrower node_selector
	// asked to stop because their nodes leave the selection; empty when none.
	StoppedInstances []KubeStoppedInstance `json:"stopped_instances"`
	// Warnings are what the edit allowed but the administrator should know,
	// such as engine_ports leaving the ports running instances hold outside
	// the new range (KUBE_ENGINE_PORTS_OUTSIDE); empty when none.
	Warnings []messages.Warning `json:"warnings"`
}
