package store

import (
	"errors"

	"gorm.io/gorm"
)

// ErrKubeClusterSourceTaken: another entry was already made from the same
// cloud account's cluster.
var ErrKubeClusterSourceTaken = errors.New("an entry for this cluster already exists")

// addCloudKubernetesSources adds where a cluster entry came from
// (KubeCluster.ProviderID, ProviderClusterID) and which managed cluster a
// cloud instance belongs to (ProviderInstance.ManagedCluster).
func addCloudKubernetesSources(db *gorm.DB) error {
	if db.Migrator().HasTable(&KubeCluster{}) {
		for _, col := range []string{"ProviderID", "ProviderClusterID"} {
			if !db.Migrator().HasColumn(&KubeCluster{}, col) {
				if err := db.Migrator().AddColumn(&KubeCluster{}, col); err != nil {
					return err
				}
			}
		}
		if !db.Migrator().HasIndex(&KubeCluster{}, "idx_kube_cluster_source") {
			if err := db.Migrator().CreateIndex(&KubeCluster{}, "idx_kube_cluster_source"); err != nil {
				return err
			}
		}
	}
	if db.Migrator().HasTable(&ProviderInstance{}) && !db.Migrator().HasColumn(&ProviderInstance{}, "ManagedCluster") {
		return db.Migrator().AddColumn(&ProviderInstance{}, "ManagedCluster")
	}
	return nil
}

// KubeClustersFromProvider returns the entries made from a cloud account's
// clusters, by the cloud's cluster ID.
func (s *Store) KubeClustersFromProvider(providerID uint) (map[string]KubeCluster, error) {
	var rows []KubeCluster
	if err := s.db.Where("provider_id = ?", providerID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]KubeCluster, len(rows))
	for _, c := range rows {
		out[c.ProviderClusterID] = c
	}
	return out, nil
}

// KubeClusterAccounts maps each cluster entry made from a cloud account to
// that account's id; entries made by hand are absent. It reads only the two
// id columns, never an entry's enrollment token or manifest settings.
func (s *Store) KubeClusterAccounts() (map[uint]uint, error) {
	var rows []struct {
		ID         uint
		ProviderID uint
	}
	if err := s.reader().Model(&KubeCluster{}).Select("id, provider_id").Where("provider_id IS NOT NULL").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]uint, len(rows))
	for _, r := range rows {
		out[r.ID] = r.ProviderID
	}
	return out, nil
}
