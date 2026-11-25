package deploy

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

type alwaysFailDriver struct{ *testenv.FakeDriver }

func (d *alwaysFailDriver) Run(context.Context, runtime.WorkloadSpec) (string, error) {
	return "", context.Canceled
}

func TestFailedDeploymentRetryIsBounded(t *testing.T) {
	env := testenv.New(t)
	env.SetControllerRetryPolicy(100*time.Millisecond, 3)
	failed := &alwaysFailDriver{FakeDriver: testenv.NewFakeDriver("docker")}
	env.AddNode("retry-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(failed))
	env.WaitNodeOnline(t, "retry-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "x/y", Revision: "main", Name: "y", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "y")}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().CreateDeployment(&store.Deployment{Name: "failing", ModelID: model.ID, NodeID: nodes[0].ID, ServedName: "y", Replicas: 1, State: "active"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	instances, err := env.Store().ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) > 3 {
		t.Fatalf("runaway instance creation: %d rows", len(instances))
	}
	for _, instance := range instances {
		if instance.State == proto.InstRunning {
			t.Errorf("failed runtime produced running instance: %+v", instance)
		}
	}
}
