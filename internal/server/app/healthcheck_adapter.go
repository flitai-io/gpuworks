package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/healthcheck"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

// healthSource lists the deployments with the health check on, and what
// each is checked with: a chat request when its model serves chat, else an
// embedding when it serves embeddings. A deployment serving neither is not
// checked.
type healthSource struct {
	store *store.Store
}

func (s healthSource) Targets(context.Context) ([]healthcheck.Target, error) {
	deps, err := s.store.HealthCheckedDeployments()
	if err != nil {
		return nil, err
	}
	models := map[uint]*store.Model{}
	out := make([]healthcheck.Target, 0, len(deps))
	for _, d := range deps {
		model, seen := models[d.ModelID]
		if !seen {
			model, _ = s.store.GetModel(d.ModelID)
			models[d.ModelID] = model
		}
		kind, path, ok := healthCheckOf(model)
		if !ok {
			continue
		}
		out = append(out, healthcheck.Target{
			DeploymentID: d.ID, Kind: kind, Path: path,
			Threshold: time.Duration(d.HealthCheckThresholdSec) * time.Second,
			Active:    d.State == store.DeployActive,
		})
	}
	return out, nil
}

// healthCheckOf is how a deployment of model is checked, false when it
// serves none of chat, text completion and embeddings.
func healthCheckOf(model *store.Model) (healthcheck.Kind, string, bool) {
	if model == nil {
		return "", "", false
	}
	op, ok := inferenceapi.ModelContract(model).CheckOperation()
	if !ok {
		return "", "", false
	}
	switch op.Type {
	case inferenceapi.OperationEmbeddings:
		return healthcheck.KindEmbeddings, op.Endpoint, true
	case inferenceapi.OperationCompletions:
		return healthcheck.KindCompletions, op.Endpoint, true
	case inferenceapi.OperationDecide:
		return healthcheck.KindDecision, op.Endpoint, true
	}
	return healthcheck.KindChat, op.Endpoint, true
}

// healthSender sends checks through the gateway, unaccounted.
type healthSender struct {
	proxy *proxy.Proxy
}

func (s healthSender) Probe(ctx context.Context, id uint, path string, payload map[string]any) healthcheck.Reply {
	r := s.proxy.Probe(ctx, id, path, payload)
	return healthcheck.Reply{NoReplica: r.NoReplica, HTTPStatus: r.HTTPStatus, InstanceUUID: r.InstanceUUID, Message: r.Message}
}

// templateProbe is how saving a deployment as a template tests the
// operations it serves: the health check's own unaccounted gateway request.
func templateProbe(px *proxy.Proxy) templateexport.Prober {
	return func(ctx context.Context, id uint, instanceUUID, path string, payload map[string]any) templateexport.ProbeReply {
		r := px.ProbeForInstance(ctx, id, instanceUUID, path, payload)
		return templateexport.ProbeReply{NoReplica: r.NoReplica, HTTPStatus: r.HTTPStatus, InstanceUUID: r.InstanceUUID, Message: r.Message}
	}
}
