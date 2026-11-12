package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type agentUpgradeAPI struct{ service *agentupgrade.Service }

var _ api.AgentUpgradeService = agentUpgradeAPI{}

func (a agentUpgradeAPI) Fleet(ctx context.Context, nodes []store.Node) (api.AgentFleetStatus, error) {
	f, err := a.service.Fleet(ctx, nodes)
	return api.AgentFleetStatus(f), err
}

func (a agentUpgradeAPI) Preview(ctx context.Context, uuids []string) (dto.AgentUpgradePreview, error) {
	p, err := a.service.Preview(ctx, uuids)
	return toAgentUpgradePreview(p), err
}
func (a agentUpgradeAPI) Create(ctx context.Context, req dto.AgentUpgradeCreateRequest, actor string) (dto.AgentUpgradeRun, error) {
	v, err := a.service.Create(ctx, agentupgrade.CreateRequest{NodeUUIDs: req.NodeUUIDs, ExpectedTargetVersion: req.ExpectedTargetVersion, ExpectedManifestFingerprint: req.ExpectedManifestFingerprint, IdempotencyKey: req.IdempotencyKey, SourceRunID: req.SourceRunID, RequestedBy: actor})
	return toAgentUpgradeRun(v), err
}
func (a agentUpgradeAPI) Get(ctx context.Context, id uint) (dto.AgentUpgradeRun, error) {
	v, err := a.service.Get(ctx, id)
	return toAgentUpgradeRun(v), err
}
func (a agentUpgradeAPI) List(ctx context.Context, page, size int) (dto.AgentUpgradeRunPage, error) {
	p, err := a.service.List(ctx, page, size)
	out := dto.AgentUpgradeRunPage{Items: []dto.AgentUpgradeRun{}, Total: p.Total, Page: p.Page, PageSize: p.PageSize, BlockedByItem: toAgentUpgradeBlocker(p.BlockedByItem)}
	for _, v := range p.Items {
		out.Items = append(out.Items, toAgentUpgradeRun(v))
	}
	return out, err
}
func (a agentUpgradeAPI) Items(ctx context.Context, id uint, page, size int, state string) (dto.AgentUpgradeItemPage, error) {
	run, err := a.service.Get(ctx, id)
	if err != nil {
		return dto.AgentUpgradeItemPage{}, err
	}
	p, err := a.service.Items(ctx, id, page, size, state)
	out := dto.AgentUpgradeItemPage{Items: []dto.AgentUpgradeItem{}, Total: p.Total, Page: p.Page, PageSize: p.PageSize}
	for _, v := range p.Items {
		out.Items = append(out.Items, toAgentUpgradeItem(v, run.Record.TargetVersion))
	}
	return out, err
}
func (a agentUpgradeAPI) Stop(ctx context.Context, id uint, actor string) (dto.AgentUpgradeRun, error) {
	v, err := a.service.Stop(ctx, id, actor)
	return toAgentUpgradeRun(v), err
}
func (a agentUpgradeAPI) ContinuePreview(ctx context.Context, id uint) (dto.AgentUpgradePreview, error) {
	p, err := a.service.ContinuePreview(ctx, id)
	return toAgentUpgradePreview(p), err
}
func (a agentUpgradeAPI) Release(ctx context.Context, id uint, uuid, actor, note string) error {
	return a.service.Release(ctx, id, uuid, actor, note)
}
func (a agentUpgradeAPI) UpgradeNode(ctx context.Context, uuid, actor string) (dto.AgentUpgradeRun, error) {
	v, err := a.service.UpgradeNode(ctx, uuid, actor)
	return toAgentUpgradeRun(v), err
}
func (a agentUpgradeAPI) BeginServerUpgrade() (func(bool), error) {
	return a.service.BeginServerUpgrade()
}
