package kubeapi

import (
	"net/url"
	"sort"
	"strings"
)

// ObjectMeta is the metadata the agent reads and writes.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
	DeletionTimestamp *string           `json:"deletionTimestamp,omitempty"`
}

// OwnerReference ties a pod to the DaemonSet, so deleting the DaemonSet
// deletes it.
type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

// Pod is a pod as created and observed.
type Pod struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
	Status     PodStatus  `json:"status,omitempty"`
}

// PodSpec is the part of a pod spec the agent sets.
type PodSpec struct {
	NodeName                      string            `json:"nodeName,omitempty"`
	NodeSelector                  map[string]string `json:"nodeSelector,omitempty"`
	SchedulerName                 string            `json:"schedulerName,omitempty"`
	RestartPolicy                 string            `json:"restartPolicy,omitempty"`
	HostNetwork                   bool              `json:"hostNetwork,omitempty"`
	HostIPC                       bool              `json:"hostIPC,omitempty"`
	HostPID                       bool              `json:"hostPID,omitempty"`
	DNSPolicy                     string            `json:"dnsPolicy,omitempty"`
	RuntimeClassName              *string           `json:"runtimeClassName,omitempty"`
	ImagePullSecrets              []LocalRef        `json:"imagePullSecrets,omitempty"`
	PriorityClassName             string            `json:"priorityClassName,omitempty"`
	TerminationGracePeriodSeconds *int64            `json:"terminationGracePeriodSeconds,omitempty"`
	Tolerations                   []Toleration      `json:"tolerations,omitempty"`
	Containers                    []Container       `json:"containers"`
	Volumes                       []Volume          `json:"volumes,omitempty"`
	SecurityContext               *PodSecurity      `json:"securityContext,omitempty"`
	// Affinity is only read: the DaemonSet controller pins each of its
	// pods to a node by a matchFields term before the scheduler binds it.
	Affinity *Affinity `json:"affinity,omitempty"`
}

// Affinity is the part of a pod's affinity the agent reads.
type Affinity struct {
	NodeAffinity *NodeAffinity `json:"nodeAffinity,omitempty"`
}

// NodeAffinity holds the required node selector terms.
type NodeAffinity struct {
	Required *NodeSelector `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// NodeSelector is a list of node selector terms, any of which matches.
type NodeSelector struct {
	Terms []NodeSelectorTerm `json:"nodeSelectorTerms"`
}

// NodeSelectorTerm is one term; the agent reads its field requirements.
type NodeSelectorTerm struct {
	MatchFields []NodeSelectorRequirement `json:"matchFields,omitempty"`
}

// NodeSelectorRequirement is "Key Operator Values".
type NodeSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// TargetNode is the node a pod is bound to, or for a pod the scheduler has
// not bound yet the node a DaemonSet pinned it to ("" when neither).
func (p Pod) TargetNode() string {
	if p.Spec.NodeName != "" {
		return p.Spec.NodeName
	}
	if p.Spec.Affinity == nil || p.Spec.Affinity.NodeAffinity == nil || p.Spec.Affinity.NodeAffinity.Required == nil {
		return ""
	}
	for _, term := range p.Spec.Affinity.NodeAffinity.Required.Terms {
		for _, f := range term.MatchFields {
			if f.Key == "metadata.name" && f.Operator == "In" && len(f.Values) == 1 {
				return f.Values[0]
			}
		}
	}
	return ""
}

// LocalRef names an object in the pod's namespace.
type LocalRef struct {
	Name string `json:"name"`
}

// PodSecurity is the pod-level security context.
type PodSecurity struct {
	SupplementalGroups []int64 `json:"supplementalGroups,omitempty"`
}

// Toleration lets a pod onto a tainted node.
type Toleration struct {
	Operator string `json:"operator,omitempty"`
}

// Container is one container of a pod.
type Container struct {
	Name            string           `json:"name"`
	Image           string           `json:"image"`
	ImagePullPolicy string           `json:"imagePullPolicy,omitempty"`
	Command         []string         `json:"command,omitempty"`
	Args            []string         `json:"args,omitempty"`
	Env             []EnvVar         `json:"env,omitempty"`
	VolumeMounts    []VolumeMount    `json:"volumeMounts,omitempty"`
	SecurityContext *SecurityContext `json:"securityContext,omitempty"`
	Resources       *Resources       `json:"resources,omitempty"`
}

// EnvVar is one environment variable with a literal value.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// VolumeMount mounts a pod volume into a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Volume is a hostPath or emptyDir volume.
type Volume struct {
	Name     string    `json:"name"`
	HostPath *HostPath `json:"hostPath,omitempty"`
	EmptyDir *EmptyDir `json:"emptyDir,omitempty"`
}

// HostPath is a host directory or file.
type HostPath struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

// EmptyDir is scratch space; Medium "Memory" is a tmpfs.
type EmptyDir struct {
	Medium    string `json:"medium,omitempty"`
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// SecurityContext is the container security context.
type SecurityContext struct {
	Privileged   *bool         `json:"privileged,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities adds Linux capabilities.
type Capabilities struct {
	Add []string `json:"add,omitempty"`
}

// Resources are requests and limits, by resource name.
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// PodStatus is what the kubelet reports.
type PodStatus struct {
	Phase             string            `json:"phase,omitempty"` // Pending | Running | Succeeded | Failed | Unknown
	Reason            string            `json:"reason,omitempty"`
	Message           string            `json:"message,omitempty"`
	Conditions        []PodCondition    `json:"conditions,omitempty"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
}

// PodCondition is one of a pod's conditions. A pod no scheduler could place
// is Pending with PodScheduled False, reason Unschedulable and the
// scheduler's message.
type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Condition returns the pod's condition of this type.
func (s PodStatus) Condition(typ string) (PodCondition, bool) {
	for _, c := range s.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return PodCondition{}, false
}

// ContainerStatus is one container's state.
type ContainerStatus struct {
	Name        string         `json:"name"`
	ContainerID string         `json:"containerID,omitempty"` // containerd://<id>
	ImageID     string         `json:"imageID,omitempty"`     // the image the container runs: "repo@sha256:<manifest>" or CRI's image id
	Ready       bool           `json:"ready"`
	State       ContainerState `json:"state"`
}

// ContainerState holds exactly one of its fields.
type ContainerState struct {
	Waiting    *StateWaiting    `json:"waiting,omitempty"`
	Running    *StateRunning    `json:"running,omitempty"`
	Terminated *StateTerminated `json:"terminated,omitempty"`
}

// StateWaiting is a container not yet running.
type StateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// StateRunning is a running container.
type StateRunning struct {
	StartedAt string `json:"startedAt,omitempty"`
}

// StateTerminated is an exited container.
type StateTerminated struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}

// PodList is a list of pods.
type PodList struct {
	Items []Pod `json:"items"`
}

// Node is a node as the agent reads it.
type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable,omitempty"`
	} `json:"spec"`
	Status struct {
		Capacity    map[string]string `json:"capacity,omitempty"`
		Allocatable map[string]string `json:"allocatable,omitempty"`
		NodeInfo    struct {
			ContainerRuntimeVersion string `json:"containerRuntimeVersion,omitempty"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

// Namespace is read for its UID: kube-system's identifies the cluster.
type Namespace struct {
	Metadata ObjectMeta `json:"metadata"`
}

// DaemonSet is read for its UID and image and patched to upgrade.
type DaemonSet struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// PodsPath is a namespace's pod collection.
func PodsPath(ns string) string { return "/api/v1/namespaces/" + url.PathEscape(ns) + "/pods" }

// AllPodsPath is every namespace's pod collection (the cluster-wide list
// needs a ClusterRole, unlike PodsPath).
const AllPodsPath = "/api/v1/pods"

// PodPath is one pod.
func PodPath(ns, name string) string { return PodsPath(ns) + "/" + url.PathEscape(name) }

// NodePath is one node.
func NodePath(name string) string { return "/api/v1/nodes/" + url.PathEscape(name) }

// NamespacePath is one namespace.
func NamespacePath(name string) string { return "/api/v1/namespaces/" + url.PathEscape(name) }

// DaemonSetPath is one DaemonSet.
func DaemonSetPath(ns, name string) string {
	return "/apis/apps/v1/namespaces/" + url.PathEscape(ns) + "/daemonsets/" + url.PathEscape(name)
}

// RuntimeClassPath is one RuntimeClass.
func RuntimeClassPath(name string) string {
	return "/apis/node.k8s.io/v1/runtimeclasses/" + url.PathEscape(name)
}

// ListQuery builds a list query that the API server answers from its watch
// cache (resourceVersion=0) rather than from etcd: every node's agent lists
// its own pods regularly, and a stale-by-seconds answer is what it needs.
func ListQuery(labelSelector string, fields map[string]string) string {
	q := url.Values{"resourceVersion": {"0"}}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	if len(fields) > 0 {
		parts := make([]string, 0, len(fields))
		for k, v := range fields {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		q.Set("fieldSelector", strings.Join(parts, ","))
	}
	return "?" + q.Encode()
}
