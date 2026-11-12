package hub

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/prepobs"
)

// The coordinator owns the observation until authenticated byte/hash
// confirmation. A successful bridge or probe is not a completed resource.
func (h *Hub) beginObservedPeerTransfer(ctx context.Context, spec prepobs.PeerTransferSpec) (context.Context, func(error, bool)) {
	ctx, finish := h.beginPeerTransfer(ctx, spec)
	h.peerTransferEvent(spec.TransferID, "started", "")
	return ctx, func(err error, inProgress bool) {
		outcome, reason := "peer_confirmed", ""
		if spec.Operation == "probe" {
			outcome = "probe_ok"
		}
		if inProgress {
			outcome, reason = "unconfirmed", "target_already_active"
		}
		if err != nil {
			outcome, reason = "failed", "transfer_failed"
			if errors.Is(err, messages.ErrPeerRelayBusy) {
				outcome, reason = "busy", "capacity_busy"
			} else if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				outcome, reason = "canceled", "canceled"
			}
			view := h.PeerTransferSnapshotFor(spec.TransferID)
			if len(view.Active) == 1 && view.Active[0].Reason != "" {
				reason = view.Active[0].Reason
			}
		}
		h.peerTransferEvent(spec.TransferID, outcome, reason)
		finish(outcome, reason)
	}
}

func (h *Hub) observeArchiveRelay(id string, relay archiveRelay, onBusy func()) archiveRelay {
	if relay == nil {
		return nil
	}
	return func(ctx context.Context, method, transferID string, params any) (json.RawMessage, error) {
		h.updatePeerTransfer(id, "", "connecting", "", "direct_dial_failed")
		h.peerTransferEvent(id, "relay_fallback", "direct_dial_failed")
		raw, err := relay(ctx, method, transferID, params)
		if errors.Is(err, messages.ErrPeerRelayBusy) {
			h.updatePeerTransfer(id, "", "waiting", "capacity_busy", "")
			if onBusy != nil {
				onBusy()
			}
		}
		return raw, err
	}
}

// Events use the existing log ingest sink explicitly: Info slog records alone
// do not enter the server log center. No tokens, endpoints or RPC payloads.
func (h *Hub) peerTransferEvent(id, event, reason string) {
	if h.onLogs == nil {
		return
	}
	view := h.PeerTransferSnapshotFor(id)
	if len(view.Active) != 1 {
		return
	}
	o := view.Active[0]
	line, err := json.Marshal(struct {
		Event      string `json:"event"`
		TransferID string `json:"transfer_id"`
		Kind       string `json:"kind"`
		Operation  string `json:"operation"`
		Source     string `json:"source_node_uuid"`
		Target     string `json:"target_node_uuid"`
		Transport  string `json:"transport,omitempty"`
		Phase      string `json:"phase"`
		Reason     string `json:"reason,omitempty"`
		Forward    int64  `json:"source_to_target_bytes"`
		Reverse    int64  `json:"target_to_source_bytes"`
		WaitNS     int64  `json:"rate_wait_ns"`
	}{event, id, o.Kind, o.Operation, o.SourceNodeUUID, o.TargetNodeUUID, o.Transport, o.Phase, reason, o.SourceToTargetBytes, o.TargetToSourceBytes, o.RateWaitNS})
	if err == nil {
		h.onLogs("", []proto.LogLine{{Source: "server", Level: "info", TimeMS: h.now().UnixMilli(), Line: string(line)}})
	}
}

// RunPeerTransferMetrics has one sampler per Hub lifecycle, with bounded enum
// labels. Transfer/node IDs belong in logs rather than time-series labels.
func (h *Hub) RunPeerTransferMetrics(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		h.samplePeerTransferMetrics()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (h *Hub) samplePeerTransferMetrics() {
	if h.onMetrics == nil {
		return
	}
	v := h.PeerTransferSnapshot()
	now := v.ObservedAt.UnixMilli()
	var samples []proto.MetricSample
	add := func(name string, value float64, label, labelValue string) {
		labels := map[string]string{"node": "server"}
		if label != "" {
			labels[label] = labelValue
		}
		samples = append(samples, proto.MetricSample{Metric: name, Value: value, Labels: labels, TimeMS: now})
	}
	add("gpuworks_peer_relay_active", float64(v.ActiveCount), "", "")
	add("gpuworks_peer_relay_rate_waiting", float64(v.WaitingCount), "", "")
	add("gpuworks_peer_relay_rate_limit_bytes_per_second", float64(v.Policy.RateLimitBytesPerSecond), "", "")
	add("gpuworks_peer_relay_bytes_total", float64(v.Totals.SourceToTargetBytes), "direction", "source_to_target")
	add("gpuworks_peer_relay_bytes_total", float64(v.Totals.TargetToSourceBytes), "direction", "target_to_source")
	add("gpuworks_peer_relay_rate_wait_seconds_total", float64(v.Totals.RateWaitNS)/float64(time.Second), "", "")
	for _, item := range []struct {
		outcome string
		count   uint64
	}{
		{"started", v.Totals.Started}, {"peer_confirmed", v.Totals.PeerConfirmed}, {"failed", v.Totals.Failed}, {"canceled", v.Totals.Canceled}, {"busy", v.Totals.Busy}, {"unconfirmed", v.Totals.Unconfirmed},
	} {
		add("gpuworks_peer_transfers_total", float64(item.count), "outcome", item.outcome)
	}
	for _, item := range []struct {
		outcome string
		count   uint64
	}{{"started", v.Totals.ProbeStarted}, {"ok", v.Totals.ProbeOK}, {"failed", v.Totals.ProbeFailed}} {
		add("gpuworks_peer_probes_total", float64(item.count), "outcome", item.outcome)
	}
	h.onMetrics("server", samples)
}
