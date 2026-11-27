package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

type retractFixture struct {
	st    *Store
	node  *Node
	other *Node
	model *Model
}

func newRetractFixture(t *testing.T) *retractFixture {
	t.Helper()
	st := newTestStore(t)
	t.Cleanup(func() { st.Close() })
	node, err := st.RegisterNode("retract-node", HashToken("a"), "retract-node")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.RegisterNode("retract-other", HashToken("b"), "retract-other")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "org/retract", Name: "retract"})
	if err != nil {
		t.Fatal(err)
	}
	return &retractFixture{st: st, node: node, other: other, model: model}
}

func (f *retractFixture) row(t *testing.T, nodeID uint, state, acquisition, transferID string) *ModelFile {
	t.Helper()
	mf, err := f.st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, NodeID: nodeID, Path: "/models/org_retract", State: state, Acquisition: acquisition, TransferID: transferID})
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

// A node's report that a copy on its own disk is absent takes back a ready
// row, and nothing else: a row still being filled, a report for another
// transfer, and another node's report all leave the row as it was.
func TestAnAbsentReportTakesBackOnlyAReadyRowOfTheReportingNode(t *testing.T) {
	for name, tc := range map[string]struct {
		state      string
		reporter   func(*retractFixture) uint
		transferID string
		gone       bool
		rejected   bool
	}{
		"ready":               {proto.MFPresent, func(f *retractFixture) uint { return f.node.ID }, "sync-1", true, false},
		"still syncing":       {proto.MFSyncing, func(f *retractFixture) uint { return f.node.ID }, "sync-1", false, false},
		"an earlier transfer": {proto.MFPresent, func(f *retractFixture) uint { return f.node.ID }, "sync-0", false, false},
		"another node":        {proto.MFPresent, func(f *retractFixture) uint { return f.other.ID }, "sync-1", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetractFixture(t)
			row := f.row(t, f.node.ID, tc.state, proto.ModelAcquirePeer, "sync-1")
			err := f.st.UpdateModelFileStatus(tc.reporter(f), proto.ModelFileStatus{
				Key: ModelFileKey(row.ID), State: proto.MFAbsent, Acquisition: proto.ModelAcquirePeer, TransferID: tc.transferID,
			})
			if (err != nil) != tc.rejected {
				t.Fatalf("report error = %v, rejected want %v", err, tc.rejected)
			}
			after, err := f.st.ModelFileByID(row.ID)
			if tc.gone {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("row after the report = %+v, %v; want it gone", after, err)
				}
				return
			}
			if err != nil || after.State != tc.state {
				t.Fatalf("row after the report = %+v, %v; want it unchanged in %s", after, err, tc.state)
			}
		})
	}
}

// Only a node-local copy is one node's to take back, and only while the node
// reports its storage ready. A shared storage keeps one row for every node,
// and a storage whose mount is gone shows an empty directory where the model
// was: neither is the node's word that the files are gone.
func TestOnlyAReadyNodeLocalStorageLosesItsRowToAnAbsentReport(t *testing.T) {
	for name, tc := range map[string]struct {
		storageType string
		nodeState   string // "" = the node has not reported
		gone        bool
	}{
		"local, not reported": {proto.StorageLocal, "", true},
		"local, ready":        {proto.StorageLocal, proto.StorageStateReady, true},
		"local, failed":       {proto.StorageLocal, proto.StorageStateFailed, false},
		"local, unresponsive": {proto.StorageLocal, proto.StorageStateUnresponsive, false},
		"nfs, ready":          {proto.StorageNFS, proto.StorageStateReady, false},
		"shared dir, ready":   {proto.StorageSharedDir, proto.StorageStateReady, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetractFixture(t)
			storage, err := f.st.CreateStorage(&Storage{Name: "retract-" + name, Type: tc.storageType, Enabled: true, Path: "/models", AllNodes: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.nodeState != "" {
				report := proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storage.ID, State: tc.nodeState, Path: storage.Path, Writable: true}}}
				if err := f.st.ApplyStorageStatus(f.node.ID, report); err != nil {
					t.Fatal(err)
				}
			}
			row := &ModelFile{ModelID: f.model.ID, StorageID: storage.ID, NodeID: f.node.ID, Path: "/models/org_retract", State: proto.MFPresent, Acquisition: proto.ModelAcquirePeer, TransferID: "sync-1"}
			if proto.StorageShared(tc.storageType) {
				row.NodeID = 0 // one row for every node
			}
			// Written as the store holds it: a shared storage's row has no
			// node, and UpsertModelFile would also place a cache row there.
			if err := f.st.DB().Create(row).Error; err != nil {
				t.Fatal(err)
			}
			if row.NodeID != 0 {
				if err := f.st.UpdateModelFileStatus(f.node.ID, proto.ModelFileStatus{Key: ModelFileKey(row.ID), State: proto.MFAbsent, Acquisition: proto.ModelAcquirePeer, TransferID: "sync-1"}); err != nil {
					t.Fatal(err)
				}
			}
			after, err := f.st.ModelFileByID(row.ID)
			if tc.gone {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("row after the report = %+v, %v; want it gone", after, err)
				}
				return
			}
			if err != nil || after.State != proto.MFPresent {
				t.Fatalf("row after the report = %+v, %v; want it unchanged", after, err)
			}
		})
	}
}

// A launch refused with MODEL_PATH_MISSING takes back the rows the instance
// was launched to read at that path on its node's own disk. A registered
// path stays as the user's assertion, marked absent; a shared storage's row
// is not one node's to take back; a row read at another path is untouched.
func TestALaunchRefusedForAMissingDirectoryTakesBackWhatItRead(t *testing.T) {
	const mounted = "/models/org_retract"
	for name, tc := range map[string]struct {
		nodeLocal   bool
		acquisition string
		state       string
		reported    string
		code        string
		want        string // "" = gone
	}{
		"downloaded copy":        {true, proto.ModelAcquireDownload, proto.MFPresent, mounted, proto.ErrorModelPathMissing, ""},
		"registered path":        {true, proto.ModelAcquireRegistered, proto.MFRegistered, mounted, proto.ErrorModelPathMissing, proto.MFAbsent},
		"shared storage":         {false, proto.ModelAcquireDownload, proto.MFPresent, mounted, proto.ErrorModelPathMissing, proto.MFPresent},
		"another path":           {true, proto.ModelAcquireDownload, proto.MFPresent, "/models/elsewhere", proto.ErrorModelPathMissing, proto.MFPresent},
		"directory not answered": {true, proto.ModelAcquireDownload, proto.MFPresent, mounted, proto.ErrorModelPathUnavailable, proto.MFPresent},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetractFixture(t)
			nodeID := f.node.ID
			if !tc.nodeLocal {
				nodeID = 0
			}
			row := f.row(t, nodeID, tc.state, tc.acquisition, "")
			inst := &Instance{UUID: "retract-instance", NodeID: f.node.ID, State: proto.InstStarting}
			if err := f.st.CreateInstances([]*Instance{inst}, [][]InstanceModelFile{{{ModelID: f.model.ID, ModelFileID: row.ID, HostPath: mounted}}}); err != nil {
				t.Fatal(err)
			}
			if err := f.st.UpdateInstanceStatusFromNode(f.node.ID, proto.InstanceStatus{
				UUID: inst.UUID, State: proto.InstFailed, Phase: proto.PhaseContainerPrepare,
				RuntimeFailure: &proto.RuntimeFailure{Code: tc.code, Resource: tc.reported, Phase: proto.PhaseContainerPrepare},
			}); err != nil {
				t.Fatal(err)
			}
			after, err := f.st.ModelFileByID(row.ID)
			if tc.want == "" {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("row after the refusal = %+v, %v; want it gone", after, err)
				}
				return
			}
			if err != nil || after.State != tc.want {
				t.Fatalf("row after the refusal = %+v, %v; want %s", after, err, tc.want)
			}
		})
	}
}
