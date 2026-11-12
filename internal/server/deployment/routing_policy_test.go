package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func TestValidateReplicaShapeNormalizesRoutingPolicy(t *testing.T) {
	req := &dto.DeployRequest{RoutingPolicy: " failover "}
	if err := ValidateReplicaShape(req); err != nil {
		t.Fatal(err)
	}
	if req.RoutingPolicy != deploy.Failover {
		t.Fatalf("routing policy=%q, want %q", req.RoutingPolicy, deploy.Failover)
	}
	defaultReq := &dto.DeployRequest{}
	if err := ValidateReplicaShape(defaultReq); err != nil {
		t.Fatal(err)
	}
	if defaultReq.RoutingPolicy != deploy.LeastRequest {
		t.Fatalf("empty routing policy=%q, want %q", defaultReq.RoutingPolicy, deploy.LeastRequest)
	}
}
