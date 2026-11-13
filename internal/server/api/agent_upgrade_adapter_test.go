package api

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// upgradeAPITestAdapter connects HTTP contract tests to the real domain without
// making the API implementation depend on that domain or the app assembly.
type upgradeAPITestAdapter struct{ service *agentupgrade.Service }

var _ AgentUpgradeService = upgradeAPITestAdapter{}

func (a upgradeAPITestAdapter) Preview(ctx context.Context, uuids []string) (dto.AgentUpgradePreview, error) {
	p, err := a.service.Preview(ctx, uuids)
	return testAgentUpgradePreview(p), err
}
func (a upgradeAPITestAdapter) Create(ctx context.Context, req dto.AgentUpgradeCreateRequest, actor string) (dto.AgentUpgradeRun, error) {
	v, err := a.service.Create(ctx, agentupgrade.CreateRequest{NodeUUIDs: req.NodeUUIDs, ExpectedTargetVersion: req.ExpectedTargetVersion, ExpectedManifestFingerprint: req.ExpectedManifestFingerprint, IdempotencyKey: req.IdempotencyKey, SourceRunID: req.SourceRunID, RequestedBy: actor})
	return testAgentUpgradeRun(v), err
}
func (a upgradeAPITestAdapter) Get(ctx context.Context, id uint) (dto.AgentUpgradeRun, error) {
	v, err := a.service.Get(ctx, id)
	return testAgentUpgradeRun(v), err
}
func (a upgradeAPITestAdapter) List(ctx context.Context, page, size int) (dto.AgentUpgradeRunPage, error) {
	p, err := a.service.List(ctx, page, size)
	out := dto.AgentUpgradeRunPage{Items: []dto.AgentUpgradeRun{}, Total: p.Total, Page: p.Page, PageSize: p.PageSize, BlockedByItem: testAgentUpgradeBlocker(p.BlockedByItem)}
	for _, v := range p.Items {
		out.Items = append(out.Items, testAgentUpgradeRun(v))
	}
	return out, err
}
func (a upgradeAPITestAdapter) Items(ctx context.Context, id uint, page, size int, state string) (dto.AgentUpgradeItemPage, error) {
	run, err := a.service.Get(ctx, id)
	if err != nil {
		return dto.AgentUpgradeItemPage{}, err
	}
	p, err := a.service.Items(ctx, id, page, size, state)
	out := dto.AgentUpgradeItemPage{Items: []dto.AgentUpgradeItem{}, Total: p.Total, Page: p.Page, PageSize: p.PageSize}
	for _, v := range p.Items {
		out.Items = append(out.Items, testAgentUpgradeItem(v, run.Record.TargetVersion))
	}
	return out, err
}
func (a upgradeAPITestAdapter) Stop(ctx context.Context, id uint, actor string) (dto.AgentUpgradeRun, error) {
	v, err := a.service.Stop(ctx, id, actor)
	return testAgentUpgradeRun(v), err
}
func (a upgradeAPITestAdapter) ContinuePreview(ctx context.Context, id uint) (dto.AgentUpgradePreview, error) {
	p, err := a.service.ContinuePreview(ctx, id)
	return testAgentUpgradePreview(p), err
}
func (a upgradeAPITestAdapter) Release(ctx context.Context, id uint, uuid, actor, note string) error {
	return a.service.Release(ctx, id, uuid, actor, note)
}
func (a upgradeAPITestAdapter) UpgradeNode(ctx context.Context, uuid, actor string) (dto.AgentUpgradeRun, error) {
	v, err := a.service.UpgradeNode(ctx, uuid, actor)
	return testAgentUpgradeRun(v), err
}
func (a upgradeAPITestAdapter) BeginServerUpgrade() (func(bool), error) {
	return a.service.BeginServerUpgrade()
}

func testAgentUpgradeBlocker(b *agentupgrade.Blocker) *dto.AgentUpgradeBlocker {
	if b == nil {
		return nil
	}
	return &dto.AgentUpgradeBlocker{RunID: b.RunID, NodeUUID: b.NodeUUID, Name: b.Name, Reason: b.Reason}
}
func testAgentUpgradePreview(p agentupgrade.Preview) dto.AgentUpgradePreview {
	out := dto.AgentUpgradePreview{TargetVersion: p.TargetVersion, ManifestFingerprint: p.ManifestFingerprint, NodeUUIDs: append([]string{}, p.NodeUUIDs...), Nodes: []dto.AgentUpgradeCandidate{}, Counts: p.Counts, BlockedByItem: testAgentUpgradeBlocker(p.BlockedByItem)}
	if out.Counts == nil {
		out.Counts = map[string]int{}
	}
	for _, n := range p.Nodes {
		change := ""
		if n.Eligibility == "eligible" {
			change = distribution.VersionChange(n.FromVersion, p.TargetVersion)
		}
		out.Nodes = append(out.Nodes, dto.AgentUpgradeCandidate{VersionChange: change, Downgrade: change == "downgrade", NodeUUID: n.NodeUUID, Name: n.Name, FromVersion: n.FromVersion, OS: n.OS, Arch: n.Arch, Transport: n.Transport, Eligibility: n.Eligibility, Reason: n.Reason})
	}
	return out
}
func testAgentUpgradeRun(v agentupgrade.RunView) dto.AgentUpgradeRun {
	r, c := v.Record, v.Counts
	return dto.AgentUpgradeRun{ID: r.ID, RequestedBy: r.RequestedBy, CreatedAt: r.CreatedAt, TargetVersion: r.TargetVersion, SourceRunID: r.SourceRunID, StopRequestedAt: r.StopRequestedAt, StopRequestedBy: r.StopRequestedBy, PauseReason: r.PauseReason, InterruptedReason: r.InterruptedReason, State: v.State, Counts: dto.AgentUpgradeCounts{Total: c.Total, Processed: c.Processed, Succeeded: c.Succeeded, Failed: c.Failed, Skipped: c.Skipped, Cancelled: c.Cancelled, Interrupted: c.Interrupted, Unconfirmed: c.Unconfirmed, Queued: c.Queued, Active: c.Active}, BlockedByItem: testAgentUpgradeBlocker(v.BlockedByItem)}
}
func testAgentUpgradeItem(v agentupgrade.ItemView, target string) dto.AgentUpgradeItem {
	i := v.Record
	return dto.AgentUpgradeItem{RunID: i.RunID, NodeUUID: i.NodeUUID, Name: i.Name, FromVersion: i.FromVersion, TargetVersion: target, CurrentVersion: v.CurrentVersion, ObservedVersion: i.ObservedVersion, OS: i.OS, Arch: i.Arch, Transport: i.Transport, Phase: i.Phase, Reason: i.Reason, Error: i.Error, StartedAt: i.StartedAt, DeadlineAt: i.DeadlineAt, FinishedAt: i.FinishedAt, RegisteredAt: i.RegisteredAt, VerifiedAt: i.VerifiedAt, VerifiedBy: i.VerifiedBy, VerificationCode: i.VerificationCode, VerificationNote: i.VerificationNote}
}

func (a upgradeAPITestAdapter) Fleet(ctx context.Context, nodes []store.Node) (AgentFleetStatus, error) {
	f, err := a.service.Fleet(ctx, nodes)
	return AgentFleetStatus(f), err
}
