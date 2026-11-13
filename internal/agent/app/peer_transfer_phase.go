package app

import "gpuworks.ai/gpuworks/internal/proto"

// Capture the originating sender rather than emitting old transfer facts on a
// replacement control session after a reconnect.
func (n *NodeRuntime) peerTransferPhaseReporter(enabled bool, id, transport string) func(string) {
	epoch := n.captureObservationConnection()
	return func(phase string) {
		if !enabled || id == "" || (phase != "connected" && phase != "confirming") {
			return
		}
		n.sendMu.Lock()
		if epoch == 0 || n.observationConnectionEpoch != epoch || n.send == nil {
			n.sendMu.Unlock()
			return
		}
		send := n.send
		n.sendMu.Unlock()
		if err := send(proto.TPeerTransferPhase, proto.PeerTransferPhase{TransferID: id, Transport: transport, Phase: phase}); err != nil {
			n.log.Debug("peer transfer phase unavailable", "transfer", id, "phase", phase, "err", err)
		}
	}
}
