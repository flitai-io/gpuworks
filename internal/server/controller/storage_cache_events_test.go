package controller

import (
	"fmt"
	"strconv"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func seedWaitingCacheFleet(t *testing.T, f *storageControllerFixture) {
	t.Helper()
	rows := make([]store.StorageCacheOperation, 1000)
	for i := range rows {
		rows[i] = store.StorageCacheOperation{StorageID: f.cached.ID, NodeID: uint(i + 1), Generation: 1, Mode: "trim", State: "waiting"}
	}
	if err := f.st.DB().CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
}

func queuedCacheKeys(c *Controller) map[string]bool {
	c.cacheQ.mu.Lock()
	defer c.cacheQ.mu.Unlock()
	out := map[string]bool{}
	for key := range c.cacheQ.pending {
		out[key] = true
	}
	return out
}

func TestCacheReferenceEventsDoNotWakeUnrelatedFleet(t *testing.T) {
	for _, topic := range []string{"deployment", "instance"} {
		t.Run(topic, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			seedWaitingCacheFleet(t, f)
			dep := f.deployment(t, f.cached)
			inst := &store.Instance{UUID: "changed-reader", DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstStopped}
			if err := f.st.DB().Create(inst).Error; err != nil {
				t.Fatal(err)
			}
			writes := 0
			if err := f.st.DB().Callback().Update().Before("gorm:update").Register("count-cache-event-writes", func(*gorm.DB) { writes++ }); err != nil {
				t.Fatal(err)
			}
			key := inst.UUID
			if topic == "deployment" {
				key = strconv.FormatUint(uint64(dep.ID), 10)
			}
			f.c.cacheEvent(bus.Event{Topic: topic, Key: key, Reason: "state:stopped"})
			keys := queuedCacheKeys(f.c)
			if len(keys) != 1 || !keys[cacheKey(f.cached.ID, f.node.ID)] {
				t.Fatalf("unrelated cache operations were queued: %v", keys)
			}
			if writes != 0 {
				t.Fatalf("reference notification wrote %d rows", writes)
			}
		})
	}
}

func TestCacheDeploymentEventIncludesPlacementAndExistingReaders(t *testing.T) {
	f := newStorageControllerFixture(t)
	seedWaitingCacheFleet(t, f)
	dep := f.deployment(t, f.cached)
	if err := f.st.DB().Model(dep).Update("node_ids", fmt.Sprintf("[%d,3]", f.node.ID)).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().Create(&store.Instance{UUID: "old-placement-reader", DeploymentID: dep.ID, NodeID: f.other.ID, State: proto.InstRunning}).Error; err != nil {
		t.Fatal(err)
	}
	f.c.cacheEvent(bus.Event{Topic: "deployment", Key: strconv.FormatUint(uint64(dep.ID), 10), Reason: "placement"})
	keys := queuedCacheKeys(f.c)
	if len(keys) != 3 || !keys[cacheKey(f.cached.ID, f.node.ID)] || !keys[cacheKey(f.cached.ID, f.other.ID)] || !keys[cacheKey(f.cached.ID, 3)] {
		t.Fatalf("placement or live readers missing: %v", keys)
	}
}

func TestCacheDeletedReferenceRetriesFromItsNodeReport(t *testing.T) {
	for _, topic := range []string{"deployment", "instance"} {
		t.Run(topic, func(t *testing.T) {
			f := newStorageControllerFixture(t)
			seedWaitingCacheFleet(t, f)
			dep := f.deployment(t, f.cached)
			inst := &store.Instance{UUID: "deleted-reader", DeploymentID: dep.ID, NodeID: f.node.ID, State: proto.InstStopped}
			if err := f.st.DB().Create(inst).Error; err != nil {
				t.Fatal(err)
			}
			key := inst.UUID
			if topic == "instance" {
				if err := f.st.DeleteInstance(inst.UUID); err != nil {
					t.Fatal(err)
				}
			} else {
				key = strconv.FormatUint(uint64(dep.ID), 10)
				if err := f.st.DeleteDeployment(dep.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.c.cacheEvent(bus.Event{Topic: topic, Key: key, Reason: "deleted"})
			if keys := queuedCacheKeys(f.c); len(keys) != 0 {
				t.Fatalf("deleted reference woke unrelated fleet: %v", keys)
			}
			f.c.cacheEvent(bus.Event{Topic: "node", Key: f.node.UUID, Reason: store.NodeChangeStatus})
			reported := "reported/" + f.node.UUID
			if !queuedCacheKeys(f.c)[reported] {
				t.Fatal("node report did not enqueue recovery")
			}
			if err := f.c.cacheWork(t.Context(), reported); err != nil {
				t.Fatal(err)
			}
			keys := queuedCacheKeys(f.c)
			if len(keys) != 2 || !keys[cacheKey(f.cached.ID, f.node.ID)] {
				t.Fatalf("node report did not recover just its waiting operation: %v", keys)
			}
		})
	}
}

func TestCacheWaitingQueryExcludesFinishedOperations(t *testing.T) {
	f := newStorageControllerFixture(t)
	rows := []store.StorageCacheOperation{
		{StorageID: f.cached.ID, NodeID: f.node.ID, State: "waiting"},
		{StorageID: f.cached.ID, NodeID: f.other.ID, State: "pending"},
		{StorageID: f.cached.ID, NodeID: 3, State: "completed"},
		{StorageID: f.cached.ID, NodeID: 4, State: "failed"},
		{StorageID: f.cached.ID, NodeID: 5, State: "running"},
	}
	if err := f.st.DB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ids  []uint
		want int
	}{{"startup", nil, 2}, {"one node", []uint{f.node.ID}, 1}, {"empty selection", []uint{}, 0}, {"finished node", []uint{3}, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.st.WaitingCacheOperations(tc.ids)
			if err != nil || len(got) != tc.want {
				t.Fatalf("operations=%+v error=%v", got, err)
			}
		})
	}
}
