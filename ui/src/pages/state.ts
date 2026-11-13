// Shared machine-state presentation. Human-readable text always comes from the
// caller's active i18n instance so a language switch updates existing views.
export type BadgeStatus = "success" | "default" | "error" | "warning" | "processing";
export const detailBadgeTone = { success: "success", default: "neutral", error: "danger", warning: "warning", processing: "info" } as const;
type Translate = (key: string) => string;

const table: Record<string, { status: BadgeStatus; key: string }> = {
	active: { status: "processing", key: "states.active" },
  pending: { status: "warning", key: "states.pending" },
  pulling: { status: "processing", key: "states.pulling" },
  starting: { status: "processing", key: "states.starting" },
  optimizing: { status: "processing", key: "states.optimizing" },
  "pending-removal": { status: "warning", key: "states.pendingRemoval" },
  draining: { status: "warning", key: "states.draining" },
  removing: { status: "warning", key: "states.removing" },
  deleting: { status: "processing", key: "states.deleting" },
  offline: { status: "warning", key: "states.offline" },
  running: { status: "success", key: "states.running" },
  // A deployment whose sleep order the engines have not carried out yet. It
  // is not serving — the gateway already refuses every request — so it must
  // not read as green.
  sleeping: { status: "processing", key: "states.sleeping" },
  // Every serving replica is asleep: awake again with one wake, but refusing
  // requests until then, so it reads as a state needing action rather than a
  // failure.
  asleep: { status: "warning", key: "states.asleep" },
  stopped: { status: "default", key: "states.stopped" },
  failed: { status: "error", key: "states.failed" },
};

export function stateLabel(s: string, t: Translate): { status: BadgeStatus; text: string } {
  const visual = table[s];
  return visual ? { status: visual.status, text: t(visual.key) } : { status: "default", text: s };
}

// effectiveDeploymentState picks the one state to show for a deployment. The
// server already derives it (asleep, sleeping, deleting aside, its
// runtime_state), but only a state this module can label is taken from it: a
// string it does not know would otherwise be shown raw, in English, next to
// translated ones. Anything else falls back to runtime_state.
export function effectiveDeploymentState(deployment: {
	runtime_state: string;
	desired_state?: string;
	state?: string;
	asleep_replicas?: number;
}): string {
	// A delete in flight outranks whatever the runtime reports: the engine may
	// still be running or already gone, but the deployment is going away, and
	// when a node never confirms teardown the row stays here — exactly the
	// state an operator has to see instead of a plain running/stopped badge.
	if (deployment.desired_state === "deleting") return "deleting";
	// The server's state is the one value that already accounts for sleep: it
	// reads asleep when every serving replica is, and sleeping while the order
	// is still being carried out, which runtime_state cannot say (a sleeping
	// engine is still running).
	if (deployment.state && deployment.state in table) return deployment.state;
	return deployment.runtime_state;
}

// partlyAsleep reports a deployment that still serves but sleeps some of its
// replicas: the state stays running, and the counts say the rest. A row must
// not read as plain green then — the asleep replicas answer nothing.
export function partlyAsleep(deployment: {
	runtime_state: string;
	desired_state?: string;
	state?: string;
	asleep_replicas?: number;
}): boolean {
	return effectiveDeploymentState(deployment) === "running" && (deployment.asleep_replicas ?? 0) > 0;
}
