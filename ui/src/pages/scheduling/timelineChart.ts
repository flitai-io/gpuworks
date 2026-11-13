import type { TimelineDeployment, TimelineSegment, TimelineState, TimelineSummary, TimelineSummaryDeployment } from "../../schedulingApi";
import type { YieldPolicyName } from "../../yieldPolicyApi";
import { schedulingColors } from "./palette";

// Geometry of the residency timeline. Every x is a position on a track that is
// CHART_WIDTH units wide and spans the window exactly, so a block's width is
// its share of the window and a tick sits where its hour does.
export const CHART_WIDTH = 1000;
// LANE_HEIGHT is the height of a lane's track in the same units.
export const LANE_HEIGHT = 18;
// MIN_BLOCK keeps a very short stretch (a one-minute switch in 24 h) visible;
// it is the only place the drawing departs from the scale.
const MIN_BLOCK = 1.5;

export interface Span {
  fromMs: number;
  toMs: number;
}

export function windowSpan(window: { from: string; to: string }): Span {
  return { fromMs: Date.parse(window.from), toMs: Date.parse(window.to) };
}

/** x of an instant on the track, clamped into the window. */
export function xOf(ms: number, span: Span): number {
  const width = Math.max(span.toMs - span.fromMs, 1);
  return Math.min(Math.max((ms - span.fromMs) / width, 0), 1) * CHART_WIDTH;
}

export function blockOf(segment: Pick<TimelineSegment, "from" | "to">, span: Span): { x: number; width: number } {
  const x = xOf(Date.parse(segment.from), span);
  const end = xOf(Date.parse(segment.to), span);
  const width = Math.max(end - x, MIN_BLOCK);
  return { x: Math.min(x, CHART_WIDTH - width), width };
}

export interface Tick {
  ms: number;
  x: number;
}

type TickStep = { unit: "minute" | "hour" | "day"; every: number; ms: number };

// The steps ticks may be spaced at, shortest first: whole minutes, whole
// hours and whole days of the local clock, each dividing the unit it counts
// in, so a tick sits where a clock would show it.
const TICK_STEPS: TickStep[] = [
  ...[1, 2, 3, 5, 10, 15, 30].map((every): TickStep => ({ unit: "minute", every, ms: every * 60_000 })),
  ...[1, 2, 3, 4, 6, 12].map((every): TickStep => ({ unit: "hour", every, ms: every * 3_600_000 })),
  { unit: "day", every: 1, ms: 86_400_000 },
];
// MAX_TICKS is the most ticks a range gets: it takes the shortest step that fits.
const MAX_TICKS = 7;

function tickStepOf(span: Span): TickStep {
  const width = Math.max(span.toMs - span.fromMs, 1);
  return TICK_STEPS.find((step) => width / step.ms <= MAX_TICKS) ?? TICK_STEPS[TICK_STEPS.length - 1];
}

/**
 * Ticks at the local clock's whole minutes, hours or days inside the range,
 * about six of them: every 3 minutes for 15 minutes, every 10 for an hour,
 * every hour for 6 h, every 4 h for 24 h, every day for a week.
 */
export function ticksOf(span: Span): Tick[] {
  const step = tickStepOf(span);
  const at = new Date(span.fromMs);
  at.setSeconds(0, 0);
  if (step.unit === "minute") at.setMinutes(at.getMinutes() - (at.getMinutes() % step.every));
  else {
    at.setMinutes(0);
    if (step.unit === "hour") at.setHours(at.getHours() - (at.getHours() % step.every));
    else at.setHours(0);
  }
  const out: Tick[] = [];
  for (; at.getTime() <= span.toMs; advance(at, step)) {
    if (at.getTime() >= span.fromMs) out.push({ ms: at.getTime(), x: xOf(at.getTime(), span) });
  }
  return out;
}

function advance(at: Date, step: TickStep): void {
  if (step.unit === "minute") at.setMinutes(at.getMinutes() + step.every);
  else if (step.unit === "hour") at.setHours(at.getHours() + step.every);
  else at.setDate(at.getDate() + step.every);
}

const pad = (n: number) => String(n).padStart(2, "0");

/**
 * The text of a tick: the clock time within a day, the date and time when the
 * range is longer than a day, the date alone when the ticks are days apart.
 */
export function tickLabel(ms: number, span: Span): string {
  const at = new Date(ms);
  const date = pad(at.getMonth() + 1) + "-" + pad(at.getDate());
  if (tickStepOf(span).unit === "day") return date;
  const time = pad(at.getHours()) + ":" + pad(at.getMinutes());
  return span.toMs - span.fromMs > 86_400_000 ? date + " " + time : time;
}

/** Fill of each state, from the theme's chart colours. */
export const stateColor: Record<TimelineState, string> = {
  awake: schedulingColors.serving,
  asleep: schedulingColors.sleeping,
  switching: schedulingColors.switching,
  starting: schedulingColors.starting,
};

export const stateOrder: TimelineState[] = ["awake", "asleep", "switching", "starting"];

/** A summarized slice as one deployment's lane draws it: the slice and what the server counted for the deployment in it. */
export interface LaneSlice {
  slice: TimelineSummary;
  deployment: TimelineSummaryDeployment;
}

export interface Lane {
  deploymentID: number;
  deployment: string;
  policy: YieldPolicyName;
  /** The deployment has been deleted since (the stats say so). */
  deleted: boolean;
  /** Its summarized slices, oldest first: the part of the lane before the card's summarized_before. */
  summary: LaneSlice[];
  segments: TimelineSegment[];
}

/**
 * One lane per deployment that held the card, raw or summarized, in the order
 * they first held it (the summarized time is the earlier). A deployment that
 * held the card only in the summarized time still has its lane, with no
 * segments. A deployment missing from the stats is a resident one: the stats
 * list only the deployments that share cards. A deleted one is marked as its
 * stats row marks it.
 */
export function lanesOf(segments: TimelineSegment[], stats: TimelineDeployment[], summary: TimelineSummary[] = []): Lane[] {
  const rows = new Map(stats.map((row) => [row.deployment_id, row]));
  const lanes = new Map<number, Lane>();
  const laneOf = (deploymentID: number, name: string): Lane => {
    let lane = lanes.get(deploymentID);
    if (!lane) {
      const row = rows.get(deploymentID);
      lane = { deploymentID, deployment: name, policy: row?.yield_policy || "never", deleted: row?.deployment_deleted ?? false, summary: [], segments: [] };
      lanes.set(deploymentID, lane);
    }
    return lane;
  };
  for (const slice of summary) {
    for (const deployment of slice.deployments) laneOf(deployment.deployment_id, deployment.deployment).summary.push({ slice, deployment });
  }
  for (const segment of segments) laneOf(segment.deployment_id, segment.deployment).segments.push(segment);
  return [...lanes.values()];
}

export function cardIntervals(segments: TimelineSegment[], span: Span) {
  const edges = new Map<number, { start: TimelineSegment[]; end: TimelineSegment[] }>();
  const edge = (time: number) => {
    const value = edges.get(time) ?? { start: [], end: [] };
    edges.set(time, value);
    return value;
  };
  for (const segment of segments) {
    const from = Math.max(span.fromMs, Date.parse(segment.from));
    const to = Math.min(span.toMs, Date.parse(segment.to));
    if (!(from < to)) continue;
    edge(from).start.push(segment);
    edge(to).end.push(segment);
  }
  const priority = { awake: 0, switching: 1, starting: 2, asleep: 3 };
  const times = [...edges.keys()].sort((a, b) => a - b);
  const active = new Set<TimelineSegment>();
  return times.flatMap((from, index) => {
    const { start, end } = edges.get(from)!;
    for (const s of end) active.delete(s);
    for (const s of start) active.add(s);
    if (!active.size || index + 1 === times.length) return [];
    const residents = [...active].sort((a, b) => priority[a.state] - priority[b.state] || a.deployment_id - b.deployment_id);
    return [{ from, to: times[index + 1], residents }];
  });
}
