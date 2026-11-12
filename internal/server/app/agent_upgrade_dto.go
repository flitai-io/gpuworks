package app

import (
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/distribution"
)

func toAgentUpgradeBlocker(b *agentupgrade.Blocker) *dto.AgentUpgradeBlocker {
	if b == nil {
		return nil
	}
	return &dto.AgentUpgradeBlocker{RunID: b.RunID, NodeUUID: b.NodeUUID, Name: b.Name, Reason: b.Reason}
}
func toAgentUpgradePreview(p agentupgrade.Preview) dto.AgentUpgradePreview {
	out := dto.AgentUpgradePreview{TargetVersion: p.TargetVersion, ManifestFingerprint: p.ManifestFingerprint, NodeUUIDs: append([]string{}, p.NodeUUIDs...), Nodes: []dto.AgentUpgradeCandidate{}, Counts: p.Counts, BlockedByItem: toAgentUpgradeBlocker(p.BlockedByItem)}
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
func toAgentUpgradeRun(v agentupgrade.RunView) dto.AgentUpgradeRun {
	r, c := v.Record, v.Counts
	return dto.AgentUpgradeRun{ID: r.ID, RequestedBy: r.RequestedBy, CreatedAt: r.CreatedAt, TargetVersion: r.TargetVersion, SourceRunID: r.SourceRunID, StopRequestedAt: r.StopRequestedAt, StopRequestedBy: r.StopRequestedBy, PauseReason: r.PauseReason, InterruptedReason: r.InterruptedReason, State: v.State, Counts: dto.AgentUpgradeCounts{Total: c.Total, Processed: c.Processed, Succeeded: c.Succeeded, Failed: c.Failed, Skipped: c.Skipped, Cancelled: c.Cancelled, Interrupted: c.Interrupted, Unconfirmed: c.Unconfirmed, Queued: c.Queued, Active: c.Active}, BlockedByItem: toAgentUpgradeBlocker(v.BlockedByItem)}
}
func toAgentUpgradeItem(v agentupgrade.ItemView, target string) dto.AgentUpgradeItem {
	i := v.Record
	return dto.AgentUpgradeItem{RunID: i.RunID, NodeUUID: i.NodeUUID, Name: i.Name, FromVersion: i.FromVersion, TargetVersion: target, CurrentVersion: v.CurrentVersion, ObservedVersion: i.ObservedVersion, OS: i.OS, Arch: i.Arch, Transport: i.Transport, Phase: i.Phase, Reason: i.Reason, Error: i.Error, StartedAt: i.StartedAt, DeadlineAt: i.DeadlineAt, FinishedAt: i.FinishedAt, RegisteredAt: i.RegisteredAt, VerifiedAt: i.VerifiedAt, VerifiedBy: i.VerifiedBy, VerificationCode: i.VerificationCode, VerificationNote: i.VerificationNote}
}
