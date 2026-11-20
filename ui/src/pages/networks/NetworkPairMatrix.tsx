import { useMemo, useState, type ReactNode } from "react";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { Input } from "../../components/ui/input";
import { Checkbox } from "../../components/ui/checkbox";
import type { NetworkTestPair, NetworkTestRun, NetworkTestStatus, NetworkTestRail } from "../../networkTypes";

import NetworkMatrixViewport from "./NetworkMatrixViewport";

const statuses: NetworkTestStatus[] = ["passed", "partial", "failed"];
const tone = {
  passed: "bg-success-surface text-fg-success",
  partial: "bg-warning-surface text-fg-warning",
  failed: "bg-danger-surface text-fg-danger",
};

export function railOK(r: NetworkTestRail) {
  return !r.code && !r.error && (r.gbps > 0 || !!r.reachable);
}

/** A historic RDMA pair without measurements has no known per-rail average. */
export function pairBandwidth(run: NetworkTestRun, pair: NetworkTestPair) {
  return run.kind === "rdma_tool" ? pair.avg_rail_gbps : pair.gbps;
}

type PairCellProps = {
  t: TFunction<"networks">;
  pair: NetworkTestPair;
  run: NetworkTestRun;
  slow: boolean;
  onOpen: (pair: NetworkTestPair) => void;
  failure: string;
};

export function PairCell({ t, pair, run, slow, onOpen, failure }: PairCellProps) {
  const gbps = pairBandwidth(run, pair);
  const partial = pair.status === "partial";
  let value = t("test.failed");
  if (pair.status !== "failed") {
    value = run.bandwidth ? (gbps == null ? "—" : t("test.gbps", { value: gbps.toFixed(1) })) : t("test.reachable");
  }
  const railText = partial && pair.rails?.length
    ? t("test.railsPassed", { passed: pair.rails.filter(railOK).length, total: pair.rails.length })
    : "";
  const hint = failure || (slow ? t("test.slow") : partial ? t("test.partialHint") : "");
  const label = [value, railText].filter(Boolean).join(" · ");
  return <button type="button" onClick={() => onOpen(pair)} title={hint || undefined} aria-description={hint || undefined}
    aria-label={`${pair.source_name} → ${pair.target_name}: ${label}`}
    className={`block h-full min-h-8 w-full cursor-pointer rounded-sm px-1 py-0.5 text-center text-label tabular-nums hover:brightness-90 hover:underline focus-visible:outline-2 focus-visible:outline-current focus-visible:brightness-90 ${tone[pair.status]}`}>
    {value}{slow && <span aria-label={t("test.slow")}> ↓</span>}
    {railText && <span className="block">{railText}</span>}
  </button>;
}

type Props = {
  run: NetworkTestRun;
  renderPair: (pair: NetworkTestPair) => ReactNode;
};

export default function NetworkPairMatrix({ run, renderPair }: Props) {
  const { t } = useTranslation("networks");
  const [source, setSource] = useState("");
  const [target, setTarget] = useState("");
  const [selected, setSelected] = useState(statuses);
  const filtered = useMemo(() => {
    const rows = new Map<string, string>();
    const cols = new Map<string, string>();
    const pairs = new Map<string, Map<string, NetworkTestPair>>();
    const from = source.trim().toLowerCase();
    const to = target.trim().toLowerCase();
    let count = 0;
    for (const pair of run.pairs) {
      if (!selected.includes(pair.status) || !pair.source_name.toLowerCase().includes(from) || !pair.target_name.toLowerCase().includes(to)) continue;
      rows.set(pair.source_uuid, pair.source_name);
      cols.set(pair.target_uuid, pair.target_name);
      let row = pairs.get(pair.source_uuid);
      if (!row) {
        row = new Map();
        pairs.set(pair.source_uuid, row);
      }
      row.set(pair.target_uuid, pair);
      count++;
    }
    const sorted = (names: Map<string, string>) => [...names].sort((a, b) => a[1].localeCompare(b[1]) || a[0].localeCompare(b[0]));
    return { rows: sorted(rows), cols: sorted(cols), pairs, count };
  }, [run.pairs, source, target, selected]);
  return <div className="min-w-0 space-y-3">
    <div className="flex flex-wrap gap-2">
      <Input className="w-40 max-w-full" type="search" size="sm" value={source} onChange={e => setSource(e.target.value)}
        aria-label={t("test.filterSource")} placeholder={t("test.filterSource")} />
      <Input className="w-40 max-w-full" type="search" size="sm" value={target} onChange={e => setTarget(e.target.value)}
        aria-label={t("test.filterTarget")} placeholder={t("test.filterTarget")} />
    </div>
    <div className="flex flex-wrap items-center gap-3" role="group" aria-label={t("test.result")}>
      {statuses.map(status => <label className="inline-flex cursor-pointer items-center gap-1.5 text-label" key={status}>
        <Checkbox aria-label={t(`test.filterStatus.${status}`)} checked={selected.includes(status)}
          onCheckedChange={checked => setSelected(prev => checked ? [...prev, status] : prev.filter(s => s !== status))} />
        <span className={`rounded-sm px-1 ${tone[status]}`}>{t(`test.filterStatus.${status}`)}</span>
      </label>)}
      <span className="ml-auto text-label text-fg-muted" aria-live="polite">{t("test.matchingPairs", { count: filtered.count })}</span>
    </div>
    {filtered.count === 0 ? <p className="text-fg-muted">{t("test.noMatchingPairs")}</p> :
      <NetworkMatrixViewport rows={filtered.rows} cols={filtered.cols} pairs={filtered.pairs} renderPair={renderPair} />}
  </div>;
}
