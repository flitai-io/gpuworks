package api

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentEffectiveStatesMatchesListWithoutPerDeploymentQueries(t *testing.T) {
	measure := func(extra int) int64 {
		t.Helper()
		fixture := newListFixture(t, 0)
		deployments, err := fixture.st.ListDeployments()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < extra; i++ {
			if _, err := fixture.st.CreateDeployment(&store.Deployment{
				Name: fmt.Sprintf("preparing-%d", i), ModelID: deployments[0].ModelID, NodeID: deployments[0].NodeID,
			}); err != nil {
				t.Fatal(err)
			}
		}
		var listed []dto.Deployment
		if err := unmarshalList(fixture.get(t, fixture.handler.listDeployments, "/api/v1/deployments"), &listed); err != nil {
			t.Fatal(err)
		}
		want := make(map[uint]string, len(listed))
		for _, deployment := range listed {
			want[deployment.ID] = deployment.State
		}
		deployments, err = fixture.st.ListDeployments()
		if err != nil {
			t.Fatal(err)
		}
		instances, err := fixture.st.ListInstances()
		if err != nil {
			t.Fatal(err)
		}
		var queries atomic.Int64
		tally := func(*gorm.DB) { queries.Add(1) }
		if err := fixture.st.DB().Callback().Query().Before("gorm:query").Register("test:runtime_queries", tally); err != nil {
			t.Fatal(err)
		}
		if err := fixture.st.DB().Callback().Row().Before("gorm:row").Register("test:runtime_rows", tally); err != nil {
			t.Fatal(err)
		}
		got, err := deployment.EffectiveStates(fixture.st, nil, deployments, instances)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("monitor inventory states = %v, deployment list states = %v", got, want)
		}
		return queries.Load()
	}
	small, large := measure(0), measure(40)
	if small == 0 || large != small {
		t.Fatalf("effective state queries grew with deployments: small=%d large=%d", small, large)
	}
}

func TestDeploymentEffectiveStatesNeedsNoReadsForStartingInstancesOrStoppedIntent(t *testing.T) {
	states, err := deployment.EffectiveStates(nil, nil,
		[]store.Deployment{{ID: 1, State: store.DeployActive}, {ID: 2, State: store.DeployStopped}},
		[]store.Instance{{DeploymentID: 1, State: proto.InstStarting}, {DeploymentID: 2, State: proto.InstRunning}},
	)
	if err != nil || states[1] != "starting" || states[2] != "stopped" {
		t.Fatalf("states = %v, error = %v", states, err)
	}
}
