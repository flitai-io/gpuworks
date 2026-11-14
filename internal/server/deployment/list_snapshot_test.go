package deployment

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listsFixture is a store with one online node, a model whose download is
// under way there, a deployment running one instance that is pulling, a
// Lists over it with a clock the test moves, and a count of the reads of the
// deployments table.
type listsFixture struct {
	st       *store.Store
	lists    *Lists
	now      time.Time
	node     *store.Node
	dep      *store.Deployment
	file     *store.ModelFile
	depReads atomic.Int64
	instance string
	modelID  uint
}

func newListsFixture(t *testing.T) *listsFixture {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &listsFixture{st: st, now: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), instance: "lists-inst"}
	f.node, err = st.RegisterNode("lists-node", store.HashToken("lists-node"), "lists-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(f.node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/lists", Revision: "main", Name: "org/lists"})
	if err != nil {
		t.Fatal(err)
	}
	f.modelID = model.ID
	f.file, err = st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: f.node.ID, State: proto.MFDownloading, Path: "/m", SizeBytes: 100, Downloaded: 10})
	if err != nil {
		t.Fatal(err)
	}
	f.dep, err = st.CreateDeployment(&store.Deployment{Name: "lists", ModelID: model.ID, NodeID: f.node.ID, Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: f.instance, DeploymentID: f.dep.ID, NodeID: f.node.ID, State: proto.InstPulling, BytesDone: 1}); err != nil {
		t.Fatal(err)
	}
	f.lists = NewLists(st, nil, func() time.Time { return f.now })
	if err := st.DB().Callback().Query().After("gorm:query").Register("test:lists_dep_reads", func(tx *gorm.DB) {
		if tx.Statement.Table == "deployments" {
			f.depReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove("test:lists_dep_reads") })
	return f
}

// rows reads the lists and reports whether the read rebuilt them.
func (f *listsFixture) rows(t *testing.T) (*ListRows, bool) {
	t.Helper()
	before := f.depReads.Load()
	rows, err := f.lists.Rows()
	if err != nil {
		t.Fatal(err)
	}
	return rows, f.depReads.Load() > before
}

// With nothing changed, reads after the first take the snapshot: no
// deployment or instance row, no join is read again.
func TestListsServeUnchangedReadsFromTheSnapshot(t *testing.T) {
	f := newListsFixture(t)
	first, rebuilt := f.rows(t)
	if !rebuilt {
		t.Fatal("the first read did not build the rows")
	}
	for range 5 {
		again, rebuilt := f.rows(t)
		if rebuilt || again != first {
			t.Fatalf("an unchanged read rebuilt the rows (rebuilt %v, same %v)", rebuilt, again == first)
		}
	}
	if deps := first.Deployments(); len(deps) != 1 || deps[0].ID != f.dep.ID {
		t.Fatalf("deployments = %+v", deps)
	}
	if insts := first.AllInstances(); len(insts) != 1 || insts[0].UUID != f.instance {
		t.Fatalf("instances = %+v", insts)
	}
}

// Every change a list is read from and that is announced is on the very
// next read: a deployment created, an instance's state, a scheduling event
// (which publishes nothing on the bus), a model file's state, a node's name
// or state, the pending timeout setting, a deployment deleted.
func TestListsSeeEveryAnnouncedChangeAtOnce(t *testing.T) {
	f := newListsFixture(t)
	var created *store.Deployment
	for _, tc := range []struct {
		name   string
		change func() error
		seen   func(*ListRows) bool
	}{
		{"deployment created", func() error {
			var err error
			created, err = f.st.CreateDeployment(&store.Deployment{Name: "second", ModelID: f.modelID, NodeID: f.node.ID, Replicas: 1, State: store.DeployActive})
			return err
		}, func(r *ListRows) bool { return created != nil && r.Deployment(created.ID) != nil }},
		{"instance state", func() error {
			return f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: f.instance, State: proto.InstRunning})
		}, func(r *ListRows) bool { return r.Instances(f.dep.ID)[0].State == proto.InstRunning }},
		{"scheduling event", func() error {
			f.st.AddDeploymentEventCode(created.ID, store.EventKindSchedule, "DEPLOYMENT_UNSCHEDULABLE", map[string]any{"reason": "no_gpu"})
			return nil
		}, func(r *ListRows) bool { _, ok := r.FirstScheduleEvent(created.ID); return ok }},
		{"model file state", func() error {
			return f.st.UpdateModelFileStatus(f.node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(f.file.ID), State: proto.MFPresent, BytesTotal: 100, BytesDone: 100})
		}, func(r *ListRows) bool { return r.ModelFile(f.modelID, f.node.ID).State == proto.MFPresent }},
		{"node renamed", func() error { _, err := f.st.RenameNode(f.node.UUID, "renamed"); return err },
			func(r *ListRows) bool { return r.Node(f.node.ID).Name == "renamed" }},
		{"node offline", func() error { return f.st.SetNodeState(f.node.UUID, store.NodeStateOffline) },
			func(r *ListRows) bool { return r.Node(f.node.ID).State == store.NodeStateOffline }},
		{"pending timeout", func() error {
			return f.st.SetSetting(store.SettingDeploymentPolicy, `{"pending_timeout_sec":42}`)
		}, func(r *ListRows) bool { return r.PendingTimeout() == 42*time.Second }},
		{"deployment deleted", func() error { return f.st.DeleteDeployment(created.ID) },
			func(r *ListRows) bool { return r.Deployment(created.ID) == nil }},
	} {
		f.rows(t) // current before the change
		if err := tc.change(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got, _ := f.rows(t); !tc.seen(got) {
			t.Errorf("%s: not on the next read", tc.name)
		}
	}
}

// Progress - an instance pulling, a model downloading - is reported far more
// often than anything changes and announces no change: it does not rebuild
// the snapshot, and the rows a page shows (Fresh) carry it all the same.
func TestListsReadProgressAgainForThePage(t *testing.T) {
	f := newListsFixture(t)
	f.rows(t)
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: f.instance, State: proto.InstPulling, BytesDone: 77, BytesTotal: 100, Message: "pulling 77%"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateModelFileStatus(f.node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(f.file.ID), State: proto.MFDownloading, BytesTotal: 100, BytesDone: 60}); err != nil {
		t.Fatal(err)
	}
	rows, rebuilt := f.rows(t)
	if rebuilt {
		t.Fatal("progress rebuilt the snapshot")
	}
	page, err := rows.Fresh([]uint{f.dep.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Instances(f.dep.ID); len(got) != 1 || got[0].BytesDone != 77 || got[0].Message != "pulling 77%" {
		t.Fatalf("the page's instance = %+v, want its latest progress", got)
	}
	if mf := page.ModelFile(f.modelID, f.node.ID); mf == nil || mf.Downloaded != 60 {
		t.Fatalf("the page's model file = %+v, want 60 bytes downloaded", mf)
	}
	progress := RecordedModelProgress(page, f.dep)
	if len(progress) != 1 || progress[0].Downloaded != 60 {
		t.Fatalf("the page's model progress = %+v, want 60 bytes downloaded", progress)
	}
	// The snapshot itself is unchanged: another reader's page reads again.
	if got := rows.Instances(f.dep.ID)[0].BytesDone; got != 1 {
		t.Fatalf("Fresh changed the shared rows: bytes %d", got)
	}
	// Rows built for one request are current already: Fresh reads nothing.
	own, err := NewListRows(f.st, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := own.Fresh([]uint{f.dep.ID}); err != nil || again != own {
		t.Fatalf("Fresh on unshared rows = %p, %v; want the same rows", again, err)
	}
}

// A write nothing announces is on a list once the snapshot is listsMaxAge
// old, and not before.
func TestListsSeeUnannouncedWritesWithinTheirMaximumAge(t *testing.T) {
	f := newListsFixture(t)
	f.rows(t)
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.dep.ID).Update("name", "behind-its-back").Error; err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(listsMaxAge - time.Millisecond)
	if got, _ := f.rows(t); got.Deployment(f.dep.ID).Name == "behind-its-back" {
		t.Fatal("an unannounced write was seen before the maximum age: the snapshot was not used")
	}
	f.now = f.now.Add(time.Millisecond)
	if got, _ := f.rows(t); got.Deployment(f.dep.ID).Name != "behind-its-back" {
		t.Fatal("an unannounced write is not seen at the maximum age")
	}
}

// Requests read the shared rows at once while the fleet changes; run with
// -race. Readers that miss together share rebuilds.
func TestListsConcurrentReadersShareTheRows(t *testing.T) {
	f := newListsFixture(t)
	f.rows(t)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 15 {
				rows, err := f.lists.Rows()
				if err != nil {
					t.Error(err)
					return
				}
				page, err := rows.Fresh([]uint{f.dep.ID})
				if err != nil {
					t.Error(err)
					return
				}
				_ = rows.NodeByUUID(f.node.UUID)
				_ = page.NodeByUUID(f.node.UUID)
				_ = RecordedModelProgress(page, f.dep)
				deps := rows.Deployments()
				deps[0].Name = "mine"
				if i == 0 && j%5 == 0 {
					if _, err := f.st.RenameNode(f.node.UUID, fmt.Sprintf("round-%d", j)); err != nil {
						t.Error(err)
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := f.depReads.Load(); n > 6 {
		t.Fatalf("12 readers x 15 reads with 3 changes read the deployments %d times, want at most 6", n)
	}
	if rows, _ := f.rows(t); rows.Deployment(f.dep.ID).Name == "mine" {
		t.Fatal("a caller's copy changed the shared rows")
	}
}

// Fresh reads a page's model files on the nodes the page is prepared and
// running on; the same model's rows on other nodes stay as the snapshot read
// them, so a shared model's other deployments are not lost from the page.
func TestListsFreshKeepsTheModelsOtherNodes(t *testing.T) {
	f := newListsFixture(t)
	other, err := f.st.RegisterNode("lists-other", store.HashToken("lists-other"), "lists-other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: f.modelID, NodeID: other.ID, State: proto.MFPresent, Path: "/m", SizeBytes: 100, Downloaded: 100}); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.rows(t)
	page, err := rows.Fresh([]uint{f.dep.ID})
	if err != nil {
		t.Fatal(err)
	}
	if mf := page.ModelFile(f.modelID, other.ID); mf == nil || mf.State != proto.MFPresent {
		t.Fatalf("the model's row on another node = %+v, want it kept", mf)
	}
	if mf := page.ModelFile(f.modelID, f.node.ID); mf == nil || mf.State != proto.MFDownloading {
		t.Fatalf("the model's row on the page's node = %+v", mf)
	}
}
