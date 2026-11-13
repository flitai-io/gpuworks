package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// countingClusterDriver is an account whose cluster listing can fail and
// whose calls are counted.
type countingClusterDriver struct {
	clusterTestDriver
	mu    sync.Mutex
	calls int
	err   error
}

func (*countingClusterDriver) Name() string { return "counting-cluster-provider" }

func (d *countingClusterDriver) ListClusters(ctx context.Context, creds Credentials) ([]Cluster, error) {
	d.mu.Lock()
	d.calls++
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.clusterTestDriver.ListClusters(ctx, creds)
}

func (d *countingClusterDriver) listed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func TestCachedClustersAsksTheCloudOncePerMaxAge(t *testing.T) {
	drv := &countingClusterDriver{}
	svc, p := clusterService(t, drv)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		clusters, err := svc.CachedClusters(ctx, p.ID, time.Minute)
		if err != nil || len(clusters) != 1 || clusters[0].ExternalID != "c-1" {
			t.Fatalf("CachedClusters = %+v, %v", clusters, err)
		}
	}
	if drv.listed() != 1 {
		t.Fatalf("the cloud was asked %d times for four reads inside the age", drv.listed())
	}
	// Older than the caller allows: listed again.
	if _, err := svc.CachedClusters(ctx, p.ID, 0); err != nil || drv.listed() != 2 {
		t.Fatalf("a zero max age must list again: calls %d, err %v", drv.listed(), err)
	}
	// The live listing is not the cache's business.
	if _, err := svc.Clusters(ctx, p.ID); err != nil || drv.listed() != 3 {
		t.Fatalf("Clusters is always live: calls %d, err %v", drv.listed(), err)
	}
}

// A cloud that fails is not asked again on every poll, and a caller that gave
// up (its context ended) leaves nothing behind that would speak for the cloud.
func TestCachedClustersRemembersAFailureBrieflyButNotACancelledCaller(t *testing.T) {
	drv := &countingClusterDriver{err: errors.New("aliyun: Forbidden.RAM")}
	svc, p := clusterService(t, drv)
	for i := 0; i < 3; i++ {
		if _, err := svc.CachedClusters(context.Background(), p.ID, time.Minute); err == nil || err.Error() != "aliyun: Forbidden.RAM" {
			t.Fatalf("CachedClusters = %v, want the cloud's error each time", err)
		}
	}
	if drv.listed() != 1 {
		t.Fatalf("a failing cloud was asked %d times; the failure is remembered for %s", drv.listed(), failedClusterListTTL)
	}

	other := &countingClusterDriver{}
	other2svc, p2 := clusterService(t, other)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	other.mu.Lock()
	other.err = context.Canceled
	other.mu.Unlock()
	if _, err := other2svc.CachedClusters(cancelled, p2.ID, time.Minute); err == nil {
		t.Fatal("a cancelled caller got an answer")
	}
	other.mu.Lock()
	other.err = nil
	other.mu.Unlock()
	clusters, err := other2svc.CachedClusters(context.Background(), p2.ID, time.Minute)
	if err != nil || len(clusters) != 1 {
		t.Fatalf("after a cancelled caller the next one must get a real answer: %+v, %v", clusters, err)
	}
}
