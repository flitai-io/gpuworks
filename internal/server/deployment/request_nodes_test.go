package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A node a request names is held to the request's requirements: a node inside
// them is accepted, one outside them refused.
func TestRequestNodesAreHeldToTheRequirements(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("labels", store.HashToken("token"), "labels"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeLabels("labels", map[string]string{"pool": "inference"}); err != nil {
		t.Fatal(err)
	}
	s := New(Deps{Store: st})
	reqs := map[string][]string{"pool": {"inference"}}
	req := dto.DeployRequest{NodeUUID: "labels", PlacementMode: "manual", NodeRequirements: reqs}
	if err := s.validateRequestNodes(&req, reqs); err != nil {
		t.Fatal(err)
	}
	if err := s.validateRequestNodes(&req, map[string][]string{"pool": {"wrong"}}); err == nil {
		t.Fatal("a node outside the requirements was accepted")
	}
}
