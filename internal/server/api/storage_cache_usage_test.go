package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCacheUsageCountsDownloadedBytesButKeepsTransferBudget(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("usage-node", store.HashToken("usage"), "usage-node")
	if err != nil {
		t.Fatal(err)
	}
	const gib = int64(1 << 30)
	cache, err := st.CreateStorage(&store.Storage{Name: "usage-cache", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Export: "nfs:/usage", CacheMaxBytes: 20 * gib}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{PathUsage: []proto.PathUsage{{Kind: proto.PathKindStorageCache, StorageID: cache.ID, Path: cache.CachePath, TotalBytes: 1000 * gib, AvailableBytes: 800 * gib}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: cache.ID, State: proto.StorageStateReady, CachePath: cache.CachePath}}}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Name: "usage-model", Source: "local", SourceID: "usage-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{StorageID: cache.ID, ModelID: model.ID, Path: "model", State: proto.MFPresent, SizeBytes: 50 * gib}); err != nil {
		t.Fatal(err)
	}
	signer, token := roleTestToken(t, st, "member")
	router := Router(Deps{Store: st, AuthSigner: signer})
	read := func() *dto.StorageNodeCache {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/storages/%d?all_nodes=1", cache.ID), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var detail dto.Storage
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if len(detail.Nodes.All) != 1 || detail.Nodes.All[0].Cache == nil {
			t.Fatalf("missing cache: %s", rec.Body.String())
		}
		return detail.Nodes.All[0].Cache
	}
	for _, state := range []string{proto.MFDownloading, proto.MFSyncing, proto.MFFailed, proto.MFPresent, proto.MFRegistered} {
		t.Run(state, func(t *testing.T) {
			if _, err := st.UpsertModelFile(&store.ModelFile{StorageID: cache.ID, NodeID: node.ID, ModelID: model.ID, Path: cache.CachePath + "/model", State: state, SizeBytes: 50 * gib, Downloaded: 10 * gib}); err != nil {
				t.Fatal(err)
			}
			got := read()
			if proto.ModelFileStateReady(state) {
				if got.UsedBytes != 50*gib || got.ReclaimableBytes != 50*gib || got.ProtectedBytes != 0 || got.State != "pending" {
					t.Fatalf("ready usage: %+v", got)
				}
			} else if got.UsedBytes != 10*gib || got.ProtectedBytes != 10*gib || got.ReclaimableBytes != 0 || got.State != "blocked" || got.Reason != "cache_full_in_use" {
				t.Fatalf("transferred usage or reserved budget lost: %+v", got)
			}
		})
	}
	// A protected transfer can hold no downloaded bytes yet. Clear still
	// cannot claim to have removed that entry.
	if _, err := st.PrepareModelCopy(store.FileRef{StorageID: cache.ID, NodeID: node.ID, ModelID: model.ID}, cache.CachePath+"/model"); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestCacheOperations(cache.ID, []uint{node.ID}, "clear", true); err != nil {
		t.Fatal(err)
	}
	op, err := st.CacheOperation(cache.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetCacheOperationResult(op, "completed", "protected_entries_remain"); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.UsedBytes != 0 || got.ProtectedBytes != 0 || got.ReclaimableBytes != 0 || got.State != "blocked" {
		t.Fatalf("zero-byte protected entry: %+v", got)
	}
}

func TestCacheUsageFleetProjectionUsesFixedQueries(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cache, err := st.CreateStorage(&store.Storage{Name: "fleet-usage", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Export: "nfs:/fleet-usage"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Name: "fleet-model", Source: "local", SourceID: "fleet-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{StorageID: cache.ID, ModelID: model.ID, Path: "model", State: proto.MFPresent, SizeBytes: 50}); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	nodes := make([]store.Node, 1000)
	files := make([]store.ModelFile, 1000)
	statuses := make([]dto.StorageNodeStatus, 1000)
	for i := range nodes {
		id := uint(i + 1)
		nodes[i] = store.Node{ID: id, UUID: fmt.Sprintf("fleet-%d", i), State: store.NodeStateOnline, PathUsageObservedAt: &observed}
		files[i] = store.ModelFile{NodeID: id, StorageID: cache.ID, ModelID: model.ID, Path: cache.CachePath + "/model", State: proto.MFDownloading, SizeBytes: 50, Downloaded: 10}
		statuses[i] = dto.StorageNodeStatus{NodeUUID: nodes[i].UUID, State: proto.StorageStateReady, CachePath: cache.CachePath}
	}
	if err := st.DB().CreateInBatches(files, 100).Error; err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	name := "cache_usage_projection_count"
	if err := st.DB().Callback().Query().After("gorm:query").Register(name, func(*gorm.DB) { queries.Add(1) }); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Query().Remove(name)
	a := &apiHandlers{d: Deps{Store: st}}
	var singleQueries int64
	for _, count := range []int{1, 1000} {
		queries.Store(0)
		out := dto.Storage{Nodes: dto.StorageNodes{All: append([]dto.StorageNodeStatus(nil), statuses[:count]...)}}
		a.fillStorageCaches(&out, cache, nodes[:count])
		if count == 1 {
			singleQueries = queries.Load()
		} else if queries.Load() != singleQueries {
			t.Fatalf("one node used %d queries; fleet used %d", singleQueries, queries.Load())
		}
		for _, node := range out.Nodes.All {
			if node.Cache == nil || node.Cache.UsedBytes != 10 || node.Cache.ProtectedBytes != 10 || node.Cache.ReclaimableBytes != 0 {
				t.Fatalf("wrong node usage: %+v", node)
			}
		}
	}
}

func TestCacheUsageExcludesHistoricalDirectory(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("directory-usage", store.HashToken("directory-usage"), "directory-usage")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "directory-usage", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Export: "nfs:/directory-usage", CacheMaxBytes: 100}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, dir := range []string{"/historical-cache", cache.CachePath} {
		model, err := st.UpsertModel(&store.Model{Name: dir, Source: "local", SourceID: dir})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{StorageID: cache.ID, ModelID: model.ID, Path: fmt.Sprintf("model-%d", i), State: proto.MFPresent, SizeBytes: 60}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{StorageID: cache.ID, NodeID: node.ID, ModelID: model.ID, Path: dir + "/model", State: proto.MFPresent, SizeBytes: int64(60 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{PathUsage: []proto.PathUsage{{Kind: proto.PathKindStorageCache, StorageID: cache.ID, Path: cache.CachePath, TotalBytes: 1000, AvailableBytes: 800}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: cache.ID, State: proto.StorageStateReady, CachePath: cache.CachePath}}}); err != nil {
		t.Fatal(err)
	}
	signer, token := roleTestToken(t, st, "member")
	router := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/storages/%d?all_nodes=1", cache.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var detail dto.Storage
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Nodes.All) != 1 || detail.Nodes.All[0].Cache == nil {
		t.Fatalf("cache projection missing: %s", rec.Body.String())
	}
	got := detail.Nodes.All[0].Cache
	if got.UsedBytes != 61 || got.ReclaimableBytes != 61 || got.ProtectedBytes != 0 || got.State != "satisfied" {
		t.Fatalf("historical cache charged to current directory: %+v", got)
	}
}
