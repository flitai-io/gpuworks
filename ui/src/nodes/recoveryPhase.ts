/**
 * The phases the server reports while it restores an agent over SSH, and
 * how each one reads. One table: the node badge's note and the recovery
 * card's tag are the same list of phases seen from two places, and a phase
 * added on the server must appear here once rather than twice.
 *
 * Whether a phase means recovery is still underway is the server's call
 * (store.RecoveryUnderway) — it is already folded into the node's state by
 * the time a client sees it, so nothing here repeats that judgement.
 */
export const recoveryLabel: Record<string, { key: string; color: string }> = {
  probing: { key: "recovery.phase.probing", color: "processing" },
  probe_failed: { key: "recovery.phase.probeFailed", color: "error" },
  artifact_check: { key: "recovery.phase.artifactCheck", color: "processing" },
  uploading: { key: "recovery.phase.uploading", color: "processing" },
  verifying: { key: "recovery.phase.verifying", color: "processing" },
  replacing: { key: "recovery.phase.replacing", color: "blue" },
  restarting: { key: "recovery.phase.restarting", color: "blue" },
  reinstalling: { key: "recovery.phase.reinstalling", color: "purple" },
  reconnecting: { key: "recovery.phase.reconnecting", color: "processing" },
  unreachable: { key: "recovery.phase.unreachable", color: "error" },
  recovery_blocked: { key: "recovery.phase.blocked", color: "error" },
  stopped: { key: "recovery.phase.stopped", color: "default" },
};
