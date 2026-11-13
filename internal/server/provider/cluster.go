package provider

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/kubeapply"
)

// Cluster is a managed Kubernetes cluster in a cloud account. Its GPU node
// pools are what a cluster entry can select; the cluster itself is never
// stored, only the entry made from it (store.KubeCluster.ProviderClusterID).
type Cluster struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Region     string `json:"region"`
	Version    string `json:"version,omitempty"`
	// State is the vendor's own word for it ("running", "provisioning"...).
	State string `json:"state"`
	// Running: the vendor reports the cluster usable.
	Running bool `json:"running"`
	// PublicAPI: the API server has an address outside the cloud's private
	// network, so the server can apply a manifest there. Without one the
	// operator applies it from inside.
	PublicAPI bool          `json:"public_api"`
	GPUPools  []ClusterPool `json:"gpu_pools"`
}

// ClusterPool is a node pool of GPU nodes.
type ClusterPool struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	GPUModel string `json:"gpu_model,omitempty"`
	GPUCount int    `json:"gpu_count,omitempty"` // per node
	Nodes    int    `json:"nodes"`
	// Vendor is the accelerator vendor of the pool's cards (accelerator
	// vendor names).
	Vendor string `json:"vendor"`
	// LabelKey is the node label the cloud puts the pool's ID on: every node
	// of the pool carries LabelKey=ID, which is how a cluster entry selects
	// the pools it holds (store.KubeClusterPool).
	LabelKey string `json:"label_key"`
}

// ClusterProvider is implemented by drivers whose accounts hold managed
// Kubernetes clusters.
type ClusterProvider interface {
	// ListClusters returns the account's clusters, complete or an error.
	ListClusters(ctx context.Context, creds Credentials) ([]Cluster, error)
	// ClusterKubeconfig issues a short-lived kubeconfig for one cluster,
	// used for a single operation and never stored.
	ClusterKubeconfig(ctx context.Context, creds Credentials, clusterID string) ([]byte, error)
}

func isClusterProvider(drv Driver) bool {
	_, ok := drv.(ClusterProvider)
	return ok
}

// ErrInstanceInCluster: the instance is a node of a managed Kubernetes
// cluster and joins through the cluster, not by an agent installed on it.
var ErrInstanceInCluster = errors.New("the instance is a node of a managed Kubernetes cluster")

// InstanceInClusterError is ErrInstanceInCluster for one cluster.
type InstanceInClusterError struct{ Cluster string }

func (e *InstanceInClusterError) Error() string {
	return ErrInstanceInCluster.Error() + " (" + e.Cluster + "): connect the cluster from its Kubernetes tab instead"
}

func (e *InstanceInClusterError) Unwrap() error { return ErrInstanceInCluster }

// refuseClusterNode refuses an operation on an instance the last sync listed
// as a node of a managed Kubernetes cluster: powering it off, releasing or
// resizing it is the cluster's business (its node pool), and doing it behind
// the cluster's back leaves the DaemonSet and the pool disagreeing
// (docs/adr/0063-apply-to-cloud-clusters.md).
func (s *Service) refuseClusterNode(providerID uint, externalID string) error {
	row, err := s.store.ProviderInstanceByExternalID(providerID, externalID)
	if err != nil {
		return err
	}
	if row != nil && row.ManagedCluster != "" {
		return &InstanceInClusterError{Cluster: row.ManagedCluster}
	}
	return nil
}

// ErrNoClusters: the provider kind has no managed Kubernetes clusters.
var ErrNoClusters = fmt.Errorf("%w: this provider has no Kubernetes clusters", ErrUnsupported)

// Clusters lists a provider account's managed Kubernetes clusters.
func (s *Service) Clusters(ctx context.Context, providerID uint) ([]Cluster, error) {
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return nil, err
	}
	cp, ok := drv.(ClusterProvider)
	if !ok {
		return nil, ErrNoClusters
	}
	return cp.ListClusters(ctx, creds)
}

// ClusterKubeconfig issues a short-lived kubeconfig for one of a provider
// account's clusters.
func (s *Service) ClusterKubeconfig(ctx context.Context, providerID uint, clusterID string) ([]byte, error) {
	_, drv, creds, err := s.driverCreds(providerID)
	if err != nil {
		return nil, err
	}
	cp, ok := drv.(ClusterProvider)
	if !ok {
		return nil, ErrNoClusters
	}
	return cp.ClusterKubeconfig(ctx, creds, clusterID)
}

// ClusterOperationError is a failure of an apply or removal, saying which
// step failed: the cloud issuing the kubeconfig, or the cluster's API server.
type ClusterOperationError struct {
	Step string // "kubeconfig" | "api_server"
	Err  error
}

func (e *ClusterOperationError) Error() string {
	if e.Step == "kubeconfig" {
		return "issue a kubeconfig for the cluster: " + e.Err.Error()
	}
	return "Kubernetes API server: " + e.Err.Error()
}

func (e *ClusterOperationError) Unwrap() error { return e.Err }

// clusterClient issues a kubeconfig for the cluster and connects with it.
func (s *Service) clusterClient(ctx context.Context, providerID uint, clusterID string) (*kubeapply.Client, error) {
	kubeconfig, err := s.ClusterKubeconfig(ctx, providerID, clusterID)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil, err
		}
		return nil, &ClusterOperationError{Step: "kubeconfig", Err: err}
	}
	c, err := kubeapply.NewClient(kubeconfig)
	if err != nil {
		return nil, &ClusterOperationError{Step: "kubeconfig", Err: err}
	}
	return c, nil
}

// ApplyToCluster applies a cluster entry's manifest to the provider's
// cluster with a kubeconfig the cloud issues now and nobody keeps (ADR
// 0054). A dry run has the API server check every change without making it.
func (s *Service) ApplyToCluster(ctx context.Context, providerID uint, clusterID, manifest string, dryRun bool) ([]kubeapply.Result, error) {
	objs, err := kubeapply.Objects(manifest)
	if err != nil {
		return nil, err
	}
	c, err := s.clusterClient(ctx, providerID, clusterID)
	if err != nil {
		return nil, err
	}
	results, err := c.Apply(ctx, objs, dryRun)
	if err != nil {
		return results, &ClusterOperationError{Step: "api_server", Err: err}
	}
	return results, nil
}

// RemoveFromCluster deletes a cluster entry's manifest objects from the
// provider's cluster, the same way ApplyToCluster applies them.
func (s *Service) RemoveFromCluster(ctx context.Context, providerID uint, clusterID, manifest string, dryRun bool) ([]kubeapply.Result, error) {
	objs, err := kubeapply.Objects(manifest)
	if err != nil {
		return nil, err
	}
	c, err := s.clusterClient(ctx, providerID, clusterID)
	if err != nil {
		return nil, err
	}
	results, err := c.Delete(ctx, objs, dryRun)
	if err != nil {
		return results, &ClusterOperationError{Step: "api_server", Err: err}
	}
	return results, nil
}
