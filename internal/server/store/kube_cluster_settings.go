package store

import (
	"errors"

	"gorm.io/gorm"
)

// KubeLiveSettings are the settings of a Kubernetes cluster entry that reach
// its running agents without the manifest: the server hands them over on
// every connection and again whenever they change (hub.PushKubeSettings), so
// editing one needs no apply and restarts nothing.
type KubeLiveSettings struct {
	IdleReturnSeconds int
	RuntimeClass      string
	EnginePorts       string
}

// KubeClusterLiveSettings reads the live settings of the entry with the given
// id by primary key; ErrNotFound when the entry is gone. It selects three
// columns, never the enrollment token.
func (s *Store) KubeClusterLiveSettings(id uint) (KubeLiveSettings, error) {
	var c KubeCluster
	err := s.reader().Select("id", "idle_return_seconds", "runtime_class", "engine_ports").Where("id = ?", id).Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return KubeLiveSettings{}, ErrNotFound
	}
	return KubeLiveSettings{IdleReturnSeconds: c.IdleReturnSeconds, RuntimeClass: c.RuntimeClass, EnginePorts: c.EnginePorts}, err
}

// KubeNodeUUIDs lists the nodes of an entry, by the index on the node's
// cluster id. A caller that needs the online ones asks the hub's session
// table, which is current where the node's stored state may lag a connect.
func (s *Store) KubeNodeUUIDs(clusterID uint) ([]string, error) {
	var uuids []string
	err := s.reader().Model(&Node{}).Where("kube_cluster_id = ?", clusterID).Order("id").Pluck("uuid", &uuids).Error
	return uuids, err
}
