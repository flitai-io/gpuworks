import type { MonitorService, MonitorStateCount } from "../../api";

export const STATE_BADGE: Record<MonitorService["state"], "success" | "info" | "neutral" | "danger" | "warning"> = {
  running: "success",
  offline: "warning",
  starting: "info",
  stopped: "neutral",
  failed: "danger",
  asleep: "warning",
};

/** The states offered as choices, by the state summary and the state column alike: those with services, plus any picked. */
export function offeredStates(counts: MonitorStateCount[], selected: string[]): MonitorStateCount[] {
  return counts.filter((item) => item.count > 0 || selected.includes(item.state));
}
