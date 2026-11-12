package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

func observedPeerTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	value := t.UTC()
	return &value
}

func PeerTransferSnapshotFrom(s prepobs.PeerTransferSnapshot, id string) PeerTransferSnapshot {
	p := s.Policy
	out := PeerTransferSnapshot{
		ObservedAt: s.ObservedAt.UTC(), ServerStartedAt: observedPeerTime(s.StartedAt), Scope: "server_process",
		Policy:           PeerRelayPolicy{RateLimitBPS: p.RateLimitBytesPerSecond, BurstBytes: p.BurstBytes, MaxActive: p.MaxActive, MaxPerNode: p.MaxPerNode, NoProgressTimeoutSeconds: p.NoProgressTimeout.Seconds(), ConfirmationTimeoutSeconds: p.ConfirmationTimeout.Seconds()},
		ActiveRelayCount: s.ActiveCount, RateWaitingCount: s.WaitingCount,
		Totals: PeerTransferTotals{Started: s.Totals.Started, PeerConfirmed: s.Totals.PeerConfirmed, Failed: s.Totals.Failed, Canceled: s.Totals.Canceled, Busy: s.Totals.Busy, Unconfirmed: s.Totals.Unconfirmed, ProbeStarted: s.Totals.ProbeStarted, ProbeOK: s.Totals.ProbeOK, ProbeFailed: s.Totals.ProbeFailed, SourceToTargetBytes: s.Totals.SourceToTargetBytes, TargetToSourceBytes: s.Totals.TargetToSourceBytes, RateWaitMS: float64(s.Totals.RateWaitNS) / float64(time.Millisecond)},
		Active: make([]PeerTransferObservation, 0, len(s.Active)),
	}
	for _, v := range s.Active {
		if id != "" && v.TransferID != id {
			continue
		}
		out.Active = append(out.Active, PeerTransferObservation{
			TransferID: v.TransferID, Kind: v.Kind, Operation: v.Operation, SourceNodeUUID: v.SourceNodeUUID, TargetNodeUUID: v.TargetNodeUUID, ResourceKey: v.ResourceKey,
			Transport: v.Transport, Phase: v.Phase, Reason: v.Reason, FallbackReason: v.FallbackReason,
			StartedAt: observedPeerTime(v.StartedAt), UpdatedAt: observedPeerTime(v.UpdatedAt), LastProgressAt: observedPeerTime(v.LastProgressAt),
			SourceToTargetBytes: v.SourceToTargetBytes, TargetToSourceBytes: v.TargetToSourceBytes,
			EffectiveRateLimitBPS: v.EffectiveRateLimitBytesPerSecond, ServerRateBPS: v.ServerRateBytesPerSecond, RateWindowSeconds: v.RateWindowSeconds,
			RateWaitMS: float64(v.RateWaitNS) / float64(time.Millisecond),
		})
	}
	return out
}
