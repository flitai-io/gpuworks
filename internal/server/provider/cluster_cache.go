package provider

import (
	"context"
	"sync"
	"time"
)

// failedClusterListTTL is how long a failed listing is remembered, so a cloud
// that is down is asked about once in a while and not on every poll.
const failedClusterListTTL = 30 * time.Second

// clusterListCache keeps each account's last cluster listing for callers that
// need a fact about the account's clusters on a hot path (the cluster entries'
// list is polled by every open page) and can live with it being a little old.
type clusterListCache struct {
	mu      sync.Mutex
	entries map[uint]*clusterListEntry
}

type clusterListEntry struct {
	mu       sync.Mutex // held while the listing is fetched, so concurrent callers share it
	at       time.Time
	clusters []Cluster
	err      error
}

func (c *clusterListCache) entry(providerID uint) *clusterListEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[uint]*clusterListEntry{}
	}
	e := c.entries[providerID]
	if e == nil {
		e = &clusterListEntry{}
		c.entries[providerID] = e
	}
	return e
}

// CachedClusters is Clusters answered from the account's last listing when it
// is younger than maxAge; otherwise it lists again and remembers the answer. A
// failed listing is remembered briefly, error included.
func (s *Service) CachedClusters(ctx context.Context, providerID uint, maxAge time.Duration) ([]Cluster, error) {
	e := s.clusterLists.entry(providerID)
	e.mu.Lock()
	defer e.mu.Unlock()
	age := time.Since(e.at)
	if !e.at.IsZero() && ((e.err == nil && age < maxAge) || (e.err != nil && age < min(maxAge, failedClusterListTTL))) {
		return e.clusters, e.err
	}
	clusters, err := s.Clusters(ctx, providerID)
	if ctx.Err() == nil { // a caller that gave up says nothing about the cloud
		e.at, e.clusters, e.err = time.Now(), clusters, err
	}
	return clusters, err
}
