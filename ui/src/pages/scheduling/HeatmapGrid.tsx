import { memo, useCallback, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type MouseEvent } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { formatBytes } from "../../format";
import { fillOf, tokenVars } from "./cellColor";
import CardTooltip, { type ActiveCard } from "./CardTooltip";
import type { Arrangement, CardView, GridNode, GroupBy, OverviewModel, Section } from "./overviewModel";

// The keys that jump a page of nodes.
const PAGE_NODES = 10;

/** cardsAcross is how many cards a node's group lays out in a row. */
export function cardsAcross(cards: number): number {
  return cards <= 4 ? Math.max(cards, 1) : cards <= 8 ? 4 : 8;
}

/** cellText is the full reading of a card, for the cell's accessible name. */
export function cellText(t: TFunction<"scheduling">, tDeploy: TFunction<"deployments">, card: CardView, deployments: OverviewModel["deployments"]): string {
  const parts = [t("heatmap.cell", {
    node: card.node.name, index: card.index, model: card.model, memory: formatBytes(card.memoryMiB * 1024 * 1024), state: t(`states.${card.state}`),
  })];
  if (card.awakePct > 0) parts.push(t("heatmap.detail.claimed", { pct: card.awakePct }));
  if (card.sleepingPct > 0) parts.push(t("heatmap.detail.residue", { pct: card.sleepingPct }));
  if (card.switching) parts.push(t("heatmap.detail.switching"));
  if (card.occupants.length > 0) {
    const holders = card.occupants.map((o) => {
      const dep = deployments[o.deployment];
      return t("heatmap.holder", { name: dep?.name ?? "?", level: tDeploy(`yieldPolicy.policy.name.${dep?.yield_policy ?? "never"}`), state: t(`heatmap.holderState.${o.asleep ? "asleep" : "awake"}`) });
    });
    parts.push(t("heatmap.detail.holders", { list: holders.join(", ") }));
  }
  return parts.join(". ");
}

interface CellsProps {
  arrangement: Arrangement;
  model: OverviewModel;
  groupBy: GroupBy;
  onActivate: (active: ActiveCard | null) => void;
  onOpen: (uuid: string) => void;
}

/**
 * Heatmap draws one small cell per card of the arrangement, grouped by node
 * (and by the section when grouped by model or label). The cells are plain
 * buttons, but every handler is on the container and reads the card from the
 * cell's data attributes, so thousands of cards cost one set of handlers; the
 * hover card is a single element beside them. Arrow keys move between the
 * cards, Up and Down between the nodes.
 */
export default function Heatmap({ arrangement, model, groupBy, onOpen }: Omit<CellsProps, "onActivate">) {
  const [active, setActive] = useState<ActiveCard | null>(null);
  return (
    <div className="hm" style={tokenVars()}>
      <Cells arrangement={arrangement} model={model} groupBy={groupBy} onActivate={setActive} onOpen={onOpen} />
      {active && <CardTooltip active={active} deployments={model.deployments} />}
    </div>
  );
}

const Cells = memo(function Cells({ arrangement, model, groupBy, onActivate, onOpen }: CellsProps) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deployments");
  const root = useRef<HTMLDivElement>(null);
  const cards = useMemo(() => {
    const index = new Map<string, CardView>();
    for (const node of model.nodes) for (const card of node.cards) index.set(`${node.uuid}/${card.index}`, card);
    return index;
  }, [model]);
  const searching = arrangement.matched.size > 0;

  // One cell is in the tab order; the arrows move it.
  useLayoutEffect(() => {
    const cells = root.current?.querySelectorAll<HTMLButtonElement>("button.hm-cell");
    if (!cells || cells.length === 0 || [...cells].some((cell) => cell.tabIndex === 0)) return;
    cells[0].tabIndex = 0;
  });

  const cardOf = (target: EventTarget | null): { card: CardView; cell: HTMLElement } | null => {
    const cell = (target as HTMLElement | null)?.closest?.<HTMLElement>("button.hm-cell");
    const card = cell ? cards.get(`${cell.dataset.node}/${cell.dataset.index}`) : undefined;
    return cell && card ? { card, cell } : null;
  };
  const show = useCallback((target: EventTarget | null) => {
    const found = cardOf(target);
    onActivate(found ? { card: found.card, rect: found.cell.getBoundingClientRect() } : null);
  }, [cards, onActivate]);

  const onClick = (event: MouseEvent) => {
    const opener = (event.target as HTMLElement).closest<HTMLElement>("[data-open]");
    if (opener?.dataset.node) onOpen(opener.dataset.node);
  };

  const onKeyDown = (event: KeyboardEvent) => {
    const current = (event.target as HTMLElement).closest<HTMLButtonElement>("button.hm-cell");
    if (!current || !root.current) return;
    const next = neighbour(root.current, current, event.key);
    if (!next) return;
    event.preventDefault();
    current.tabIndex = -1;
    next.tabIndex = 0;
    next.focus();
  };

  return (
    <div
      ref={root}
      className={`hm-body${searching ? " is-searching" : ""}`}
      role="group"
      aria-label={t("heatmap.aria")}
      aria-describedby="hm-help"
      onClick={onClick}
      onKeyDown={onKeyDown}
      onMouseOver={(event) => show(event.target)}
      onMouseLeave={() => onActivate(null)}
      onFocus={(event) => show(event.target)}
      onBlur={() => onActivate(null)}
    >
      <p id="hm-help" className="hm-help">{t("heatmap.help")}</p>
      {arrangement.sections.map((section) => (
        <SectionBlock key={section.key} section={section} grouped={groupBy !== "node"} matched={arrangement.matched} model={model} t={t} tDeploy={tDeploy} />
      ))}
    </div>
  );
});

interface SectionProps {
  section: Section;
  grouped: boolean;
  matched: Set<number>;
  model: OverviewModel;
  t: TFunction<"scheduling">;
  tDeploy: TFunction<"deployments">;
}

function SectionBlock({ section, grouped, matched, model, t, tDeploy }: SectionProps) {
  const heading = section.label || t("heatmap.noLabel");
  return (
    <section className="hm-section" aria-label={grouped ? heading : undefined}>
      {grouped && (
        <h4 className="hm-section-head">
          {heading}
          <span className="hm-section-count">{t("heatmap.sectionCount", { cards: section.cards, free: section.free })}</span>
        </h4>
      )}
      <div className="hm-groups">
        {section.nodes.map((item) => (
          <NodeGroup key={item.node.uuid} item={item} matched={matched} deployments={model.deployments} t={t} tDeploy={tDeploy} />
        ))}
      </div>
    </section>
  );
}

interface GroupProps {
  item: GridNode;
  matched: Set<number>;
  deployments: OverviewModel["deployments"];
  t: TFunction<"scheduling">;
  tDeploy: TFunction<"deployments">;
}

/** NodeGroup is one node's cards. It re-renders only when its node or the search changes. */
const NodeGroup = memo(function NodeGroup({ item, matched, deployments, t, tDeploy }: GroupProps) {
  const { node, cards } = item;
  return (
    <div className={`hm-node${node.online ? "" : " is-offline"}`} role="group" aria-label={node.name} data-node={node.uuid}>
      <button type="button" tabIndex={-1} className="hm-node-name" data-open="" data-node={node.uuid}
        title={node.name} aria-label={t("heatmap.openNode", { node: node.name })}>
        {node.name}
      </button>
      <div className="hm-cards" style={{ gridTemplateColumns: `repeat(${cardsAcross(cards.length)}, var(--hm-cell, 12px))` }}>
        {cards.map((card) => {
          const hit = matched.size > 0 && card.occupants.some((o) => matched.has(o.deployment));
          return (
            <button
              key={card.index}
              type="button"
              tabIndex={-1}
              className={`hm-cell is-${card.state}${card.switching ? " is-switching" : ""}${hit ? " is-hit" : ""}`}
              style={{ background: fillOf(card) }}
              data-open=""
              data-node={node.uuid}
              data-index={card.index}
              aria-label={cellText(t, tDeploy, card, deployments)}
            />
          );
        })}
      </div>
    </div>
  );
});

/**
 * The cell a key moves to from `current`: Left and Right the previous and next
 * card (across nodes), Up and Down the same position on the previous and next
 * node, PageUp and PageDown ten nodes, Home and End the first and last card.
 */
export function neighbour(root: HTMLElement, current: HTMLElement, key: string): HTMLButtonElement | null {
  const cells = [...root.querySelectorAll<HTMLButtonElement>("button.hm-cell")];
  const at = cells.indexOf(current as HTMLButtonElement);
  if (at < 0) return null;
  switch (key) {
    case "ArrowLeft": return cells[Math.max(at - 1, 0)];
    case "ArrowRight": return cells[Math.min(at + 1, cells.length - 1)];
    case "Home": return cells[0];
    case "End": return cells[cells.length - 1];
    case "ArrowUp": case "ArrowDown": case "PageUp": case "PageDown": {
      const groups = [...root.querySelectorAll<HTMLElement>(".hm-node")];
      const group = current.closest<HTMLElement>(".hm-node");
      const position = group ? groups.indexOf(group) : -1;
      if (position < 0 || !group) return null;
      const step = key === "ArrowUp" ? -1 : key === "ArrowDown" ? 1 : key === "PageUp" ? -PAGE_NODES : PAGE_NODES;
      const target = groups[Math.min(Math.max(position + step, 0), groups.length - 1)];
      const inGroup = [...group.querySelectorAll("button.hm-cell")].indexOf(current);
      const there = target.querySelectorAll<HTMLButtonElement>("button.hm-cell");
      return there[Math.min(inGroup, there.length - 1)] ?? null;
    }
    default: return null;
  }
}
