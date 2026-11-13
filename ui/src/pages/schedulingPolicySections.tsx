import type { ReactNode } from "react";
import type { TFunction } from "i18next";
import type { Deployment } from "../api";
import { formatTime } from "../i18n/datetime";
import NodeReplacementEditor, { nodeReplacementSeconds } from "./DeploymentNodeReplacement";
import YieldPolicyEditor from "./yieldPolicy/DeploymentYieldPolicy";
import { savedPolicy } from "./yieldPolicy/model";
import { yieldSummary } from "./yieldPolicy/summary";
import ScheduleSection, { scheduleOrderText, type Schedules } from "./DeploymentSchedule";
import { durationText } from "./durationText";

// What a section reads and calls: the deployment, the schedule rows the card
// loaded, and the reload after a change.
export interface PolicyContext {
  dep: Deployment;
  schedules: Schedules;
  onChanged: () => void;
}

// A PolicySection is one kind of standing policy on a deployment: its name,
// whether the deployment has it, the phrase it adds to the card's summary and
// the editor that changes it. The card lists these in order, so a new policy
// is one more entry here.
export interface PolicySection {
  key: string;
  // The i18n key of its name, in the deployments namespace, and of the name
  // its summary phrase goes by when that differs from the tab's.
  label: string;
  summaryLabel?: string;
  visible: (dep: Deployment) => boolean;
  summary: (t: TFunction<"deployments">, context: PolicyContext) => string;
  render: (context: PolicyContext) => ReactNode;
}

export const policySections: PolicySection[] = [
  {
    key: "failover",
    label: "nodeReplacement.title",
    visible: () => true,
    summary: (t, { dep }) => {
      const sec = nodeReplacementSeconds(dep);
      return sec > 0 ? durationText(t, sec) : t("schedulingPolicy.off");
    },
    render: ({ dep, onChanged }) => (
      <NodeReplacementEditor key={`${dep.id}-${nodeReplacementSeconds(dep)}`} dep={dep} onChanged={onChanged} />
    ),
  },
  {
    key: "schedule",
    label: "automation.schedule.title",
    visible: () => true,
    summary: (t, { schedules }) => scheduleSummary(t, schedules),
    render: ({ dep, schedules }) => <ScheduleSection dep={dep} schedules={schedules} />,
  },
  {
    key: "yieldPolicy",
    label: "yieldPolicy.title",
    summaryLabel: "yieldPolicy.summaryLabel",
    // Shown where the server allows a policy, and where the deployment can
    // sleep but may not yield (the editor says why); a deployment that cannot
    // sleep has nothing to set, unless it kept a policy from when it could, so
    // it can go back.
    visible: (dep) =>
      dep.yield_policy_eligible?.ok === true || dep.schedule_capabilities?.sleep.ok === true || savedPolicy(dep).policy !== "never",
    summary: (t, { dep }) => yieldSummary(t, dep),
    render: ({ dep, onChanged }) => (
      <YieldPolicyEditor key={`yield-${dep.id}-${JSON.stringify(savedPolicy(dep))}`} dep={dep} onChanged={onChanged} />
    ),
  },
];

// scheduleSummary counts the orders and names the next one to run among the
// enabled ones.
function scheduleSummary(t: TFunction<"deployments">, { rows, error }: Schedules): string {
  if (error) return t("schedulingPolicy.loadFailed");
  if (rows === null) return t("schedulingPolicy.loading");
  if (rows.length === 0) return t("schedulingPolicy.none");
  const count = t("schedulingPolicy.scheduleCount", { count: rows.length });
  const upcoming = rows
    .filter((row) => row.enabled && row.next_at)
    .sort((left, right) => new Date(left.next_at!).getTime() - new Date(right.next_at!).getTime());
  if (upcoming.length === 0) return count + t("schedulingPolicy.scheduleAllOff");
  return count + t("schedulingPolicy.scheduleNext", { time: formatTime(upcoming[0].next_at), order: scheduleOrderText(t, upcoming[0]) });
}
