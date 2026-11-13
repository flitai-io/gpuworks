package store

import (
	"errors"

	"gorm.io/gorm"
)

// ErrKubeClusterNameTaken refuses naming an entry after another one.
var ErrKubeClusterNameTaken = errors.New("another cluster entry has this name")

// kubeClusterEditable are the columns UpdateKubeCluster writes. The
// namespace, the server URL and the state directory stay as the entry was
// made: changing any of them would leave the cluster's agents, or their
// state on the hosts, behind.
var kubeClusterEditable = []string{
	"name", "node_selector", "agent_image", "agent_image_source", "image_pull_secret",
	"runtime_class", "containerd_socket", "api_server", "engine_ports",
	"idle_return_seconds", "priority", "allow_preemption",
}

// UpdateKubeCluster writes c's editable settings onto the entry with c's
// UUID and returns the entry as stored.
func (s *Store) UpdateKubeCluster(c KubeCluster) (*KubeCluster, error) {
	out, _, err := s.UpdateKubeClusterLeaving(c, KubeLeave{})
	return out, err
}

// UpdateKubeClusterLeaving is UpdateKubeCluster that also applies leave (the
// instances to stop and the nodes to cordon) in the same transaction: a save
// that fails (ErrNotFound, ErrKubeClusterNameTaken, any store error) stops and
// cordons nothing. Events are published after the commit. It returns the
// instances asked to stop.
func (s *Store) UpdateKubeClusterLeaving(c KubeCluster, leave KubeLeave) (*KubeCluster, []StoppedInstance, error) {
	var out KubeCluster
	var rows []Instance
	var stopped []StoppedInstance
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("uuid = ?", c.UUID).First(&out).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var n int64
		if err := tx.Model(&KubeCluster{}).Where("name = ? AND id <> ?", c.Name, out.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrKubeClusterNameTaken
		}
		if err := tx.Model(&out).Select(kubeClusterEditable).Updates(&KubeCluster{
			Name: c.Name, NodeSelector: c.NodeSelector, AgentImage: c.AgentImage, AgentImageSource: c.AgentImageSource,
			ImagePullSecret: c.ImagePullSecret, RuntimeClass: c.RuntimeClass, ContainerdSocket: c.ContainerdSocket,
			APIServer: c.APIServer, EnginePorts: c.EnginePorts, IdleReturnSeconds: c.IdleReturnSeconds,
			Priority: c.Priority, AllowPreemption: c.AllowPreemption,
		}).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", out.ID).First(&out).Error; err != nil {
			return err
		}
		var err error
		rows, stopped, err = applyKubeLeaveTx(tx, leave)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	s.announceKubeLeave(leave, rows)
	return &out, stopped, nil
}
