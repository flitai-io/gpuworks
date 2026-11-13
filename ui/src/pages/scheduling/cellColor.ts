import { colors } from "../../theme/tokens";
import { schedulingColors } from "./palette";
import type { CardView, CellState } from "./overviewModel";

// The heatmap's palette, from the theme's chart colours: one hue per state,
// and for an awake card the share of its memory that is claimed shown as how
// deep the blue is, so a nearly empty card and a full one differ at a glance.

/** Base fill of each state; an awake card's is shaded by awakeFill. */
export const stateFill: Record<CellState, string> = {
  awake: schedulingColors.serving,
  asleep: schedulingColors.sleeping,
  free: schedulingColors.free,
  held_outside: schedulingColors.external,
  unhealthy: schedulingColors.failed,
  offline: schedulingColors.offline,
};

/** Outline of a free card, which is otherwise as pale as the page. */
export const freeBorder = schedulingColors.freeBorder;

function channels(hex: string): [number, number, number] {
  const n = parseInt(hex.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

/** mix is `from` moved `amount` (0-1) of the way to `to`, as #rrggbb. */
export function mix(from: string, to: string, amount: number): string {
  const [a, b] = [channels(from), channels(to)];
  const t = Math.min(Math.max(amount, 0), 1);
  return `#${a.map((value, i) => Math.round(value + (b[i] - value) * t).toString(16).padStart(2, "0")).join("")}`;
}

/** The lightest blue an awake card shows, at a claim of 1%. */
const AWAKE_FLOOR = 0.3;

/** awakeFill shades the awake colour by the share of the card claimed (0-100). */
export function awakeFill(pct: number): string {
  return mix(colors.white, stateFill.awake, AWAKE_FLOOR + (1 - AWAKE_FLOOR) * (Math.min(Math.max(pct, 0), 100) / 100));
}

/** fillOf is the colour a card is drawn in. */
export function fillOf(card: Pick<CardView, "state" | "awakePct">): string {
  return card.state === "awake" ? awakeFill(card.awakePct) : stateFill[card.state];
}

/** The palette as custom properties, from the theme's tokens, for the heatmap's stylesheet and its legend. */
export function tokenVars(): Record<string, string> {
  return {
    "--hm-free-border": freeBorder, "--hm-switching": schedulingColors.switching, "--hm-highlight": colors.danger, "--hm-offline-line": colors.weak, "--hm-tip-text": colors.white,
    "--hm-border": colors.border, "--hm-held": stateFill.held_outside, "--hm-unhealthy": stateFill.unhealthy, "--hm-offline": stateFill.offline,
  };
}
