package provider

import "gpuworks.ai/gpuworks/pkg/localization"

// Resource is a kind of machine an external platform hands over to become
// compute nodes: an instance an agent is installed on, or a Kubernetes
// cluster whose GPU nodes join through a DaemonSet.
type Resource string

const (
	ResourceVM         Resource = "vm"
	ResourceContainer  Resource = "container"
	ResourceHost       Resource = "host"
	ResourceKubernetes Resource = "kubernetes"
)

// KindResource is one resource a provider kind connects, with the vendor's
// own product name for it when it has one (ECS, ACK).
type KindResource struct {
	Type    Resource `json:"type"`
	Product string   `json:"product,omitempty"`
}

// kindResources is what each known kind connects, in the order the console
// lists it: instances first, then clusters. A kind lists Kubernetes exactly
// when its driver is a ClusterProvider.
var kindResources = map[string][]KindResource{
	"aliyun":       {{ResourceVM, "ECS"}, {ResourceKubernetes, "ACK"}},
	"autodl":       {{ResourceContainer, ""}},
	"compshare":    {{ResourceVM, ""}, {ResourceContainer, ""}},
	"digitalocean": {{ResourceVM, "Droplet"}, {ResourceKubernetes, "DOKS"}},
	"runpod":       {{ResourceContainer, "Pod"}},
	"vast":         {{ResourceContainer, ""}},
	"zstack":       {{ResourceHost, ""}},
}

// ResourcesOf is what a kind connects; empty for a kind that declares
// nothing.
func ResourcesOf(kind string) []KindResource {
	return append([]KindResource{}, kindResources[kind]...)
}

// InstanceResource says which of a kind's resources an instance is. The
// instance's own kind ("vm" or "container") decides when the kind connects
// that; a kind with one instance resource (a ZStack Cloud host) needs no
// telling. Empty when neither settles it.
func InstanceResource(kind, instanceKind string) Resource {
	var only Resource
	instances := 0
	for _, r := range kindResources[kind] {
		if r.Type == ResourceKubernetes {
			continue
		}
		if string(r.Type) == instanceKind {
			return r.Type
		}
		only = r.Type
		instances++
	}
	if instances == 1 {
		return only
	}
	return ""
}

// Cluster types: the vendor's product a managed cluster is, which decides
// what the cluster can do and is how its owner knows it.
const (
	ClusterTypeACKManaged    = "ack_managed"
	ClusterTypeACKDedicated  = "ack_dedicated"
	ClusterTypeACKEdge       = "ack_edge"
	ClusterTypeACKLingjun    = "ack_lingjun"
	ClusterTypeACKRegistered = "ack_registered"
	ClusterTypeDOKS          = "doks"
)

var knownClusterTypes = []string{
	ClusterTypeACKManaged, ClusterTypeACKDedicated, ClusterTypeACKEdge, ClusterTypeACKLingjun, ClusterTypeACKRegistered,
	ClusterTypeDOKS,
}

// ClusterTypeLabel is a cluster type's name; nil for a type without one.
func ClusterTypeLabel(clusterType string) localization.Text {
	if _, known := providerLocales[localization.English].ClusterTypes[clusterType]; !known {
		return nil
	}
	label := make(localization.Text, len(requiredProviderLocales))
	for _, language := range requiredProviderLocales {
		label[language] = providerLocales[language].ClusterTypes[clusterType]
	}
	return label
}
