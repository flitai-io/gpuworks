import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Table, TableHeader, TableHead, TableBody, TableRow, TableCell } from "../../components/ui/table";
import { Badge } from "../../components/ui/badge";
import { Tooltip } from "../../components/ui/tooltip";
import { formatBytes } from "../../format";
import type { BoardGPU, BoardNode, BoardOccupant } from "../../schedulingApi";
import { deploymentColor } from "./palette";
import ResidencyLegend from "./ResidencyLegend";
import { tokenVars } from "./cellColor";

interface MemoryPart {
  state: BoardOccupant["state"];
  deploymentID?: number;
  name: string;
  bytes?: number;
  key: string;
  sleepLevel?: number;
}

function instanceBytes(occupant: BoardOccupant) {
  return occupant.state === "awake" ? occupant.claim_bytes : (occupant.residue_bytes ?? 0);
}

function partsOf(card: BoardGPU): MemoryPart[] {
  const parts: MemoryPart[] = [];
  for (const state of ["awake", "asleep"] as const) {
    const groups = new Map<string, MemoryPart>();
    for (const occupant of card.occupants) {
      if (occupant.state !== state) continue;
      const id = occupant.deployment_id;
      const sleepLevel = state === "asleep" ? occupant.sleep_level : undefined;
      const key = `${state}:${id}:${sleepLevel ?? ""}`;
      const part = groups.get(key) ?? { key, state, deploymentID: id, name: occupant.deployment, bytes: 0, sleepLevel };
      const bytes = instanceBytes(occupant);
      part.bytes = bytes != null && part.bytes != null ? part.bytes + bytes : undefined;
      groups.set(key, part);
    }
    parts.push(...groups.values());
    const total = state === "awake" ? card.awake_bytes : card.sleeping_bytes;
    const known = [...groups.values()];
    if (known.every(part => part.bytes != null)) {
      const remainder = total - known.reduce((sum, part) => sum + (part.bytes ?? 0), 0);
      if (remainder > 0) parts.push({ key: state, state, name: "", bytes: remainder });
    }
  }
  return parts;
}

function AllocationTag({ part }: { part: MemoryPart }) {
  const { t } = useTranslation("scheduling");
  const color = part.deploymentID == null ? undefined : deploymentColor(part.deploymentID);
  const label = t(part.state === "awake" ? "nodeChart.servingMemory" : "nodeChart.sleepingMemory");
  return <Badge color={color ? { base: color, onStrong: "var(--fg-on-brand)" } : "gray"} size="sm" className="resident-tag">
    <i aria-hidden="true" className="resident-marker" data-state={part.state} style={{ color }}/>
    <span>{label}{part.sleepLevel != null && ` L${part.sleepLevel}`}</span>
    {part.deploymentID == null ? t("nodeChart.unreportedDeployment") :
      <Link to={`/deployments?deployment=${part.deploymentID}`}>{part.name || `#${part.deploymentID}`}</Link>}
    <span>· {part.bytes == null ? t("nodeChart.unreported") : formatBytes(part.bytes)}</span>
  </Badge>;
}

function CurrentMemory({ card, offline }: { card: BoardGPU; offline: boolean }) {
  const { t } = useTranslation("scheduling");
  const parts = partsOf(card);
  const heldOutside = card.held_outside || card.state === "held_outside";
  const hasCapacity = Number.isFinite(card.memory_bytes) && card.memory_bytes > 0;
  const freeUnknown = !hasCapacity || offline || heldOutside || !Number.isFinite(card.free_bytes) || card.free_bytes < 0 || card.free_bytes > card.memory_bytes;
  const free = freeUnknown ? t("nodeChart.unknown") : formatBytes(card.free_bytes);
  const measured = parts.filter((part): part is MemoryPart & { bytes: number } =>
    part.bytes != null && Number.isFinite(part.bytes) && part.bytes >= 0);
  const allocated = measured.reduce((sum, part) => sum + part.bytes, 0);
  const segments = hasCapacity && allocated <= card.memory_bytes ? measured : [];
  const balanced = !freeUnknown && measured.length === parts.length && allocated + card.free_bytes === card.memory_bytes;
  return <div className="residency-memory">
    <div className="memory-tags">
      {card.switching && <span className="sr-only">{t(offline ? "nodeChart.recordedSwitching" : "timeline.state.switching")}</span>}
      {parts.map(part => <AllocationTag key={part.key} part={part}/>)}
      {offline && !parts.length && <small>{t("nodeChart.noAllocation")}</small>}
      {heldOutside && <Tooltip content={t("nodes.heldOutsideHint")} className="max-w-72">
        <Badge size="sm" color="orange" tabIndex={0} className="external-memory-tag">{t("states.held_outside")}</Badge>
      </Tooltip>}
      <Badge size="sm" className="free-tag">
        {t("nodes.legend.free")} · {free}
      </Badge>
    </div>
    <div className="residency-meter" data-switching={card.switching} data-balanced={balanced} aria-hidden="true">{segments.map(part => (
      <i key={part.key} data-state={part.state}
        style={{ color: part.deploymentID == null ? "var(--fg-muted)" : deploymentColor(part.deploymentID), width: `${part.bytes / card.memory_bytes * 100}%` }}/>
    ))}</div>
  </div>;
}

export default function GpuResidencyTable({ node }: { node: BoardNode }) {
  const { t } = useTranslation("scheduling");
  const cards = [...node.gpus].sort((a, b) => a.index - b.index);
  return <>
    {!node.online && <p className="offline-memory-note">{t("nodeChart.offlineNote")}</p>}
    <ResidencyLegend items={[
      { state: "awake", label: t("nodeChart.servingMemory") },
      { state: "asleep", label: t("nodeChart.sleepingMemory") },
      { state: "switching", label: t(node.online ? "timeline.state.switching" : "nodeChart.recordedSwitching") },
      { state: "free", label: t("nodes.legend.free") },
    ]}/>
    <Table size="md" className="table-fixed" style={tokenVars()} containerClassName="max-w-full">
      <TableHeader><TableRow><TableHead className="gpu-ident-col">GPU</TableHead><TableHead>{t("nodeChart.allocation")}</TableHead></TableRow></TableHeader>
      <TableBody className="[&>tr]:font-normal">
        {cards.map(card => <TableRow key={card.index} data-gpu-index={card.index}>
          <TableCell>
            <div className="gpu-meta">
              <div className="flex items-center gap-1.5">
                <strong className="whitespace-nowrap">GPU {card.index}</strong>
                {card.state === "unhealthy" && <Badge size="sm" status="danger">{t("states.unhealthy")}</Badge>}
              </div>
              <small>{card.name}{card.memory_bytes > 0 && " · " + formatBytes(card.memory_bytes)}</small>
            </div>
          </TableCell>
          <TableCell>
            {node.online && card.state === "offline" && <small>{t("nodeChart.cardOfflineNote")}</small>}
            <CurrentMemory card={card} offline={!node.online || card.state === "offline"}/>
          </TableCell>
        </TableRow>)}
        {!cards.length && <TableRow><TableCell colSpan={2}>{t("nodeChart.noGpu")}</TableCell></TableRow>}
      </TableBody>
    </Table>
  </>;
}
