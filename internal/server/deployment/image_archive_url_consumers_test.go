package deployment

import (
	"context"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The archive URL and the registry refs below are declared together on one
// image, which is the shape every consumer of registry_sources has to cope
// with once an operator imports an image from a URL.
const (
	consumerArchiveURL = "https://containerimages.example/team-runner--gpu3.tar"
	consumerCanonical  = "registry.example/team/runner:v1"
)

// An archive URL in an image's registry_sources is a file to download, not a
// reference to pull: the evaluation's pull forecast never lists it, and the
// registry sources keep their order.
func TestForecastPullRefsLeaveOutTheArchiveURL(t *testing.T) {
	st, _ := imageWithSources(t, "file:api_forecast_url_test?mode=memory&cache=shared", []string{"quay.io/team/runner:v1", consumerArchiveURL})
	node, err := st.RegisterNode("archive-forecast", store.HashToken("token"), "archive-forecast")
	if err != nil {
		t.Fatal(err)
	}
	h := Service{d: Deps{Store: st}}
	facts := h.evaluateNodeAcquisition(context.Background(), []string{node.UUID}, consumerCanonical, nil, nil, store.SourceRules{}, nil)
	if len(facts) != 1 || facts[0].ImagePlan != "pull" {
		t.Fatalf("forecast = %+v", facts)
	}
	if want := []string{"quay.io/team/runner:v1", consumerCanonical}; !reflect.DeepEqual(facts[0].ImagePullRefs, want) {
		t.Fatalf("pull refs = %v, want %v", facts[0].ImagePullRefs, want)
	}
}
