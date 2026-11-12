package app

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMonitorServiceStateKeepsReadyReplicaAboveHistoricalFailure(t *testing.T) {
	fx := newMonitorFixtureBase(t)
	if _, err := fx.st.CreateInstance(&store.Instance{
		UUID: "monitor-old-failed", DeploymentID: fx.dep.ID, NodeID: fx.node.ID,
		State: proto.InstFailed, DesiredStateJSON: `{"desired_state":"stopped"}`,
	}); err != nil {
		t.Fatal(err)
	}
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)
	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 1 {
		t.Fatalf("services = %+v", out.Services)
	}
	row := out.Services[0]
	if row.Replicas.Ready != 1 || row.Replicas.Desired != 1 || row.State != "running" {
		t.Fatalf("ready service with a retired failed instance = state %q, replicas %+v; want running, 1/1", row.State, row.Replicas)
	}
	if err := fx.st.SetDeploymentState(fx.dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	svc = newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)
	out, err = svc.Overview(context.Background(), time.Hour)
	if err != nil || len(out.Services) != 1 || out.Services[0].State != "stopped" {
		t.Fatalf("stopped intent with an old running snapshot = %+v, error %v", out.Services, err)
	}
}

func TestMonitorServiceStateStartsWhileModelIsDownloadingWithoutInstances(t *testing.T) {
	fx := newMonitorFixtureBase(t)
	if err := fx.st.SetNodeState(fx.node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	model, err := fx.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "monitor/downloading", Name: "downloading"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := fx.st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: fx.node.ID, Path: "/models/downloading", State: proto.MFDownloading,
		Acquisition: proto.ModelAcquireDownload, TransferID: "monitor-download", SizeBytes: 100, Downloaded: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := fx.st.CreateDeployment(&store.Deployment{
		Name: "monitor-downloading", ModelID: model.ID, Engine: "vllm", Replicas: 1,
		NodeID: fx.node.ID, SaveToStorageID: file.StorageID, State: store.DeployActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(preparation deployment.ModelPreparation) dto.MonitorServiceRow {
		t.Helper()
		svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, preparation)
		out, err := svc.Overview(context.Background(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range out.Services {
			if row.DeploymentID == dep.ID {
				return row
			}
		}
		t.Fatalf("downloading deployment absent from monitor: %+v", out.Services)
		return dto.MonitorServiceRow{}
	}
	if row := read(nil); row.Replicas.Ready != 0 || row.Replicas.Desired != 1 || row.State != "starting" {
		t.Fatalf("downloading service = state %q, replicas %+v; want starting, 0/1", row.State, row.Replicas)
	}
	file.State = proto.MFFailed
	if _, err := fx.st.UpsertModelFile(file); err != nil {
		t.Fatal(err)
	}
	if row := read(nil); row.State != "failed" {
		t.Fatalf("failed download = %q, want failed", row.State)
	}
	dep, err = fx.st.CreateDeployment(&store.Deployment{
		Name: "monitor-unpinned", ModelID: model.ID, Engine: "vllm", Replicas: 1,
		SaveToStorageID: file.StorageID, State: store.DeployActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row := read(monitorPreparationNodes{fx.node.ID}); row.State != "failed" {
		t.Fatalf("failed download on controller-chosen node = %q, want failed", row.State)
	}
}

func TestMonitorServiceStatePreservesAnExpiredSchedulingRefusal(t *testing.T) {
	fx := newMonitorFixtureBase(t)
	dep, err := fx.st.CreateDeployment(&store.Deployment{
		Name: "monitor-stalled", Engine: "vllm", Replicas: 1, State: store.DeployActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.st.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, "DEPLOYMENT_UNSCHEDULABLE", map[string]any{"reason": "no_gpu"})
	if err := fx.st.DB().Model(&store.InstanceEvent{}).Where("deployment_id = ?", dep.ID).
		Update("created_at", fx.now.Add(-48*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)
	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range out.Services {
		if row.DeploymentID == dep.ID {
			if row.State != "failed" {
				t.Fatalf("expired scheduling refusal = %q, want failed", row.State)
			}
			return
		}
	}
	t.Fatalf("stalled deployment missing from monitor: %+v", out.Services)
}

// The monitor can read preparation on dynamically selected nodes but must
// never invoke the preparation service's mutation method.
type monitorPreparationNodes []uint

func (monitorPreparationNodes) ModelCopyObservation(*store.ModelFile) (proto.ModelFileStatus, bool) {
	return proto.ModelFileStatus{}, false
}
func (monitorPreparationNodes) CachePhase(store.FileRef) string             { return "" }
func (p monitorPreparationNodes) PreparationNodes(*store.Deployment) []uint { return p }
func (monitorPreparationNodes) RetryCacheCopy(context.Context, store.FileRef) error {
	panic("monitoring must not mutate preparation")
}

// A replica under a sleep order its engine has not carried out yet is already
// refused by the gateway, so the monitor must not count it ready: the page
// would otherwise show 1/1 ready beside a service the gateway answers 503 for.
func TestMonitorServiceStateCountsAnOrderedReplicaAsNotReady(t *testing.T) {
	fx := newMonitorFixtureBase(t)
	if err := fx.st.SetNodeState(fx.node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	read := func() dto.MonitorServiceRow {
		t.Helper()
		svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)
		out, err := svc.Overview(context.Background(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Services) != 1 {
			t.Fatalf("services = %+v", out.Services)
		}
		return out.Services[0]
	}
	row := read()
	if row.Replicas.Ready != 1 || row.State != "running" {
		t.Fatalf("before sleep = state %q, replicas %+v; want running, 1/1", row.State, row.Replicas)
	}
	// Ordered with no reported level: the engine is still at level 0.
	if err := fx.st.OrderInstancesSleep(fx.dep.ID, []string{fx.inst.UUID}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	row = read()
	if row.Replicas.Ready != 0 {
		t.Fatalf("ordered replica = %+v; want 0 ready: the gateway already refuses it", row.Replicas)
	}
	if row.State != "asleep" {
		t.Fatalf("ordered replica = state %q; want asleep", row.State)
	}
}
