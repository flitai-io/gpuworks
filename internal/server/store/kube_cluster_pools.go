package store

import (
	"errors"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// A cluster entry made from a cloud account's cluster selects its nodes by
// node pool, and may select several pools of the cluster (all of one vendor,
// see docs/adr/0063-apply-to-cloud-clusters.md). The selected pools are rows
// of kube_cluster_pools; an entry made by hand has none and keeps the
// free-form KubeCluster.NodeSelector.

// KubeClusterPool is one node pool a cluster entry selects: the DaemonSet
// runs on the nodes whose LabelKey label is one of the entry's PoolIDs.
type KubeClusterPool struct {
	ID            uint `gorm:"primaryKey"`
	KubeClusterID uint `gorm:"not null;uniqueIndex:idx_kube_cluster_pool,priority:1"`
	// PoolID is the cloud's id of the node pool, the label's value.
	PoolID string `gorm:"size:128;not null;uniqueIndex:idx_kube_cluster_pool,priority:2"`
	// LabelKey is the node label the cloud puts the pool id on
	// (alibabacloud.com/nodepool-id on ACK, doks.digitalocean.com/node-pool-id
	// on DOKS). It belongs to the account's cloud, not to the pool, so every
	// pool of an entry has the same one; it is kept per row so the manifest
	// renders without the account, which may have been deleted since.
	LabelKey string `gorm:"size:317;not null"`
}

// ErrKubeClusterPoolLabels: the pools of one entry do not share a label key,
// which only pools of different clouds would not.
var ErrKubeClusterPoolLabels = errors.New("the node pools of one entry must carry the same label key")

// createKubeClusterPools creates kube_cluster_pools and turns what existing
// entries of a cloud cluster selected, one "key=value" node selector naming
// their pool, into a pool row. An entry with another kind of selector stays as
// it is: it has no pool rows and keeps selecting by that selector.
func createKubeClusterPools(db *gorm.DB) error {
	if err := db.AutoMigrate(&KubeClusterPool{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable("kube_clusters") || !db.Migrator().HasColumn("kube_clusters", "provider_cluster_id") {
		return nil
	}
	var rows []struct {
		ID           uint
		NodeSelector string
	}
	if err := db.Raw(`SELECT id, node_selector FROM kube_clusters
		WHERE provider_id IS NOT NULL AND provider_cluster_id <> '' AND node_selector IS NOT NULL AND node_selector <> ''`).Scan(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		key, value, ok := singleSelectorPair(r.NodeSelector)
		if !ok {
			continue
		}
		var have int64
		if err := db.Model(&KubeClusterPool{}).Where("kube_cluster_id = ?", r.ID).Count(&have).Error; err != nil {
			return err
		}
		if have == 0 {
			if err := db.Create(&KubeClusterPool{KubeClusterID: r.ID, PoolID: value, LabelKey: key}).Error; err != nil {
				return err
			}
		}
		if err := db.Exec(`UPDATE kube_clusters SET node_selector = '' WHERE id = ?`, r.ID).Error; err != nil {
			return err
		}
	}
	return nil
}

// singleSelectorPair reads a node selector of exactly one "key=value" pair.
func singleSelectorPair(selector string) (key, value string, ok bool) {
	var parts []string
	for _, p := range strings.Split(selector, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) != 1 {
		return "", "", false
	}
	k, v, found := strings.Cut(parts[0], "=")
	k, v = strings.TrimSpace(k), strings.TrimSpace(v)
	if !found || k == "" || v == "" {
		return "", "", false
	}
	return k, v, true
}

// KubeClusterPools returns the pools each of the given entries selects,
// ordered by pool id. An entry without any is absent from the map.
func (s *Store) KubeClusterPools(clusterIDs ...uint) (map[uint][]KubeClusterPool, error) {
	out := map[uint][]KubeClusterPool{}
	if len(clusterIDs) == 0 {
		return out, nil
	}
	var rows []KubeClusterPool
	if err := s.db.Where("kube_cluster_id IN ?", clusterIDs).Order("pool_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.KubeClusterID] = append(out[r.KubeClusterID], r)
	}
	return out, nil
}

// SetKubeClusterPools replaces the pools an entry selects. The entry then
// selects by its pools alone: the free-form node selector an earlier entry may
// still carry is cleared.
func (s *Store) SetKubeClusterPools(uuid string, pools []KubeClusterPool) (*KubeCluster, error) {
	if err := checkPoolLabels(pools); err != nil {
		return nil, err
	}
	var c KubeCluster
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("uuid = ?", uuid).First(&c).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.Where("kube_cluster_id = ?", c.ID).Delete(&KubeClusterPool{}).Error; err != nil {
			return err
		}
		if err := insertKubeClusterPools(tx, c.ID, pools); err != nil {
			return err
		}
		c.NodeSelector = ""
		return tx.Model(&c).Update("node_selector", "").Error
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func insertKubeClusterPools(tx *gorm.DB, clusterID uint, pools []KubeClusterPool) error {
	seen := map[string]bool{}
	sorted := append([]KubeClusterPool(nil), pools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PoolID < sorted[j].PoolID })
	for _, p := range sorted {
		if seen[p.PoolID] {
			continue
		}
		seen[p.PoolID] = true
		if err := tx.Create(&KubeClusterPool{KubeClusterID: clusterID, PoolID: p.PoolID, LabelKey: p.LabelKey}).Error; err != nil {
			return err
		}
	}
	return nil
}

func checkPoolLabels(pools []KubeClusterPool) error {
	for _, p := range pools {
		if p.LabelKey != pools[0].LabelKey {
			return ErrKubeClusterPoolLabels
		}
	}
	return nil
}
