import { useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { Button } from "../../components/ui/button";
import { Badge } from "../../components/ui/badge";
import { Tooltip } from "../../components/ui/tooltip";
import { DropdownMenu, DropdownTrigger, DropdownContent } from "../../components/ui/dropdown";
import { MenuItem } from "../../components/ui/menu-item";
import { Container, ContainerBody } from "../../components/ui/container";
import { MetricCard } from "../../components/ui/metric-card";
import { Notice } from "../../ui-kit/Notice";
import { ArrowRightOutlined, DatabaseOutlined, InfoCircleOutlined } from "@ant-design/icons";
import { Link, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { GPUListResponse, SchedulingCardSummary } from "../../api";
import { VENDOR_ASSETS, VENDOR_LOGO_HEIGHT } from "../../components/vendorAssets";
import { compareVendors, vendorForFramework } from "../../vendors";
import { storageDetailPath } from "../storage/storageLinks";

const GIB = 1024 ** 3;
const MAX_VENDOR_ROWS = 3;
const MAX_MODELS_PER_VENDOR = 2;
const MAX_SINGLE_VENDOR_ROWS = 2;
// One formatter each: building an Intl.NumberFormat per number cost the overview tens of milliseconds.
const integer = new Intl.NumberFormat();
const wholeNumber = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });
export const fmt = (value?: number) => value == null ? "—" : integer.format(value);
export const gib = (value?: number) => value == null ? "—" : `${(value / GIB).toFixed(1)} GiB`;
const gibText = (value: number) => `${wholeNumber.format(value)} GiB`;

export type StorageRow = { id: number; name: string; total?: number; available?: number; problems: number };
export type AttentionEntry = { to: string; title: string; content: ReactNode };

/** One of the page's big blocks: a section title above a grey tray of white panels. */
export function Block({ title, hint, extra, className, children }: { title: string; hint?: string; extra?: ReactNode; className?: string; children: ReactNode }) {
  return <section className="dashboard-block" aria-label={title}>
    <div className="dashboard-block-head"><h2>{title}</h2>{hint && <span className="dashboard-block-hint">{hint}</span>}{extra && <span className="dashboard-block-extra">{extra}</span>}</div>
    {/* Tray 16, panels 12 (ui-guidelines 10.x); Zeron's defaults are 24 / 16. */}
    <Container className="rounded-2xl"><div className={className}>{children}</div></Container>
  </section>;
}
export function Panel({ title, tag, to, label, className, children }: { title?: string; tag?: string; to?: string; label?: string; className?: string; children: ReactNode }) {
  return <ContainerBody className={`dashboard-panel rounded-xl${className ? ` ${className}` : ""}`}>
    {title && <div className="dashboard-panel-head"><h3>{title}</h3>{tag && <span className="dashboard-overview-count">{tag}</span>}{to && <ModuleArrow to={to} label={label ?? title} />}</div>}
    {children}
  </ContainerBody>;
}
export function ModuleArrow({ to, label }: { to: string; label: string }) { return <Link to={to} className="dashboard-overview-module-arrow" aria-label={label} title={label}><ArrowRightOutlined /></Link>; }
export function EmptyState({ title, detail }: { title: string; detail: string }) { return <div className="dashboard-overview-empty"><strong>{title}</strong><span>{detail}</span></div>; }
export function Metric({ label, value, exact, hint, tip, className, to }: { label: string; value: ReactNode; exact?: string; hint?: string; tip?: string; className?: string; to?: string }) {
  return <MetricCard className={`dashboard-overview-metric rounded-none border-0 bg-transparent p-0 shadow-none${className ? ` ${className}` : ""}`} label={label} value={to ? <Link to={to}>{value}</Link> : value} title={exact} footer={hint} labelClassName="text-label text-fg-muted" valueClassName="text-heading-md" leading={tip ? <Tooltip content={tip}><InfoCircleOutlined className="dashboard-overview-metric-tip" tabIndex={0} role="img" aria-label={tip} /></Tooltip> : undefined} />;
}
/** A state count with its dot. Zero stays visible, in grey, so a row keeps its shape. */
export function StatusCount({ count, label, tone }: { count?: number; label: string; tone?: "success" | "warning" | "danger" | "info" | "neutral" }) {
  if (count == null) return null;
  return <span className={`dashboard-overview-status${tone ? ` dashboard-overview-status-${tone}` : ""}${count === 0 ? " is-zero" : ""}`}>{label}</span>;
}
/** A labelled count; a link only when it is above zero and has a page to go to. */
export function Figure({ label, value, to, hot }: { label: string; value?: number | string; to?: string; hot?: boolean }) {
  const body = <><span>{label}</span><b className={hot ? "is-hot" : undefined}>{typeof value === "number" ? fmt(value) : value ?? "—"}</b></>;
  const zero = value === 0;
  return to && !zero && value != null ? <Link to={to} className="dashboard-figure is-link">{body}</Link> : <span className={`dashboard-figure${zero ? " is-zero" : ""}`}>{body}</span>;
}

export function AttentionBanner({ entries, grouped, count }: { entries: AttentionEntry[]; grouped: boolean; count: number }) {
  const { t } = useTranslation("dashboard");
  const navigate = useNavigate();
  const contentRef = useRef<HTMLDivElement>(null);
  const [visibleCount, setVisibleCount] = useState(2);
  const entryTitles = entries.map((entry) => entry.title).join("\n");
  useLayoutEffect(() => {
    const content = contentRef.current;
    if (!content) return;
    const update = () => {
      if (!content.clientWidth) return;
      const headingWidth = content.querySelector<HTMLElement>(".dashboard-overview-attention-heading")?.offsetWidth ?? 0;
      const entryWidths = [...content.querySelectorAll<HTMLElement>(".dashboard-overview-attention-measure-entry")].map((element) => element.offsetWidth);
      const moreWidths = [...content.querySelectorAll<HTMLElement>(".dashboard-overview-attention-measure-more")].map((element) => element.offsetWidth);
      for (let visible = entryWidths.length; visible >= 0; visible--) {
        const itemsWidth = entryWidths.slice(0, visible).reduce((sum, width) => sum + width, 0) + Math.max(0, visible - 1) * 8;
        const hidden = entryWidths.length - visible;
        const required = headingWidth + (visible ? 12 + itemsWidth : 0) + (hidden ? 12 + moreWidths[hidden - 1] : 0);
        if (required <= content.clientWidth) {
          setVisibleCount(Math.max(1, visible));
          return;
        }
      }
      setVisibleCount(1);
    };
    update();
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(update);
    observer?.observe(content);
    window.addEventListener("resize", update);
    return () => { observer?.disconnect(); window.removeEventListener("resize", update); };
  }, [entryTitles, grouped, count]);
  const shown = Math.min(visibleCount, entries.length);
  const hidden = entries.slice(shown);
  const moreLabel = (amount: number) => t(grouped ? "overview.moreCategories" : "overview.moreIssues", { count: amount });
  return <div className="dashboard-overview-attention"><Notice tone="warning" title={<div ref={contentRef} className="dashboard-overview-attention-content">
    <span className="dashboard-overview-attention-heading">{t("overview.attention")}{" "}<Badge status="warning" className="dashboard-overview-attention-count">{grouped ? t("overview.attentionCategories", { count }) : count}</Badge></span>
    <div className="dashboard-overview-attention-items">{entries.slice(0, shown).map((entry, index) => <Link key={index} to={entry.to} title={entry.title} className="dashboard-overview-attention-link">{entry.content}</Link>)}</div>
    {hidden.length > 0 && <DropdownMenu><DropdownTrigger render={<Button variant="ghost" size="sm" className="dashboard-overview-attention-more">{moreLabel(hidden.length)}</Button>} /><DropdownContent align="end">{hidden.map((entry, index) => <MenuItem key={index} index={index} onSelect={() => navigate(entry.to)} label={entry.title} />)}</DropdownContent></DropdownMenu>}
    <div className="dashboard-overview-attention-measure" aria-hidden="true">{entries.map((entry, index) => <span key={index} className="dashboard-overview-attention-measure-entry">{entry.content}</span>)}{entries.map((_, index) => <Button key={index} variant="ghost" size="sm" tabIndex={-1} className="dashboard-overview-attention-more dashboard-overview-attention-measure-more">{moreLabel(index + 1)}</Button>)}</div>
  </div>} /></div>;
}

export function StorageCapacityRow({ row }: { row: StorageRow }) {
  const { t } = useTranslation("dashboard");
  const used = row.total != null && row.available != null ? Math.max(0, row.total - row.available) : undefined;
  const percent = used != null && row.total ? Math.min(100, used / row.total * 100) : 0;
  return <div className="dashboard-overview-storage-row"><DatabaseOutlined /><div><Link to={storageDetailPath(row.id)}>{row.name}</Link>{row.problems > 0 && <small>{t("overview.storageProblem", { count: row.problems })}</small>}</div><div className="dashboard-overview-storage-capacity"><span>{used == null ? t("overview.capacityUnknown") : t("overview.usedStorage", { used: gib(used), total: gib(row.total) })}</span>{row.available != null && <span>{t("overview.availableStorage", { size: gib(row.available) })}</span>}</div>{used != null && <i className="dashboard-overview-storage-progress"><b style={{ width: `${percent}%` }} /></i>}</div>;
}

// The card states in the scheduling overview's order: the three a working
// fleet is in, then the three that need someone.
const CARD_STATES = ["awake", "asleep", "free", "held_outside", "unhealthy", "offline"] as const;

/** The fleet's cards by state, as a ring of card counts rather than a capacity bar. */
export function CardOccupancy({ cards }: { cards: SchedulingCardSummary }) {
  const { t } = useTranslation("dashboard");
  const total = cards.cards;
  const radius = 48, circumference = 2 * Math.PI * radius, gap = total > 1 ? 1.6 : 0;
  let offset = 0;
  const arcs = CARD_STATES.filter((state) => cards[state] > 0).map((state) => {
    const length = cards[state] / total * circumference;
    const arc = <circle key={state} className={`dashboard-ring-arc is-${state}`} cx="60" cy="60" r={radius} strokeDasharray={`${Math.max(length - gap, 0.5)} ${circumference}`} strokeDashoffset={-offset}><title>{`${t(`overview.cardState.${state}`)} ${fmt(cards[state])}`}</title></circle>;
    offset += length;
    return arc;
  });
  return <div className="dashboard-ring-wrap">
    <div className="dashboard-ring"><svg viewBox="0 0 120 120" role="img" aria-label={t("overview.cardsAria", { total: fmt(total) })}><circle className="dashboard-ring-track" cx="60" cy="60" r={radius} />{arcs}</svg><div className="dashboard-ring-center"><strong>{fmt(total)}</strong><span>{t("overview.cardsCenter")}</span></div></div>
    <div className="dashboard-ring-legend">{CARD_STATES.map((state) => <div key={state} className={`dashboard-ring-row${cards[state] ? "" : " is-zero"}`}><em className={`is-${state}`} /><span>{t(`overview.cardState.${state}`)}</span><b>{fmt(cards[state])}</b><small>{cards[state] && total ? `${Math.round(cards[state] / total * 100)}%` : ""}</small></div>)}</div>
  </div>;
}

type VendorGroup = { vendor: string; cards: number; vramGiB: number; models: { name: string; cards: number; vramGiB: number }[] };

/** The platform's own cards grouped by vendor, then model, largest VRAM first. */
export function vendorGroups(gpus: GPUListResponse, unknownModel: string): VendorGroup[] {
  const byVendor = new Map<string, Map<string, { cards: number; bytes: number }>>();
  for (const gpu of gpus.items.filter((card) => !card.ownership)) {
    const vendor = vendorForFramework(gpu.vendor);
    const models = byVendor.get(vendor) ?? new Map();
    const name = gpu.name || unknownModel;
    const row = models.get(name) ?? { cards: 0, bytes: 0 };
    row.cards += 1;
    row.bytes += Number.isFinite(gpu.memory_total_bytes) ? gpu.memory_total_bytes : 0;
    models.set(name, row);
    byVendor.set(vendor, models);
  }
  return [...byVendor].map(([vendor, models]) => {
    const rows = [...models].map(([name, row]) => ({ name, cards: row.cards, vramGiB: row.bytes / GIB })).sort((a, b) => b.vramGiB - a.vramGiB || a.name.localeCompare(b.name));
    return { vendor, models: rows, cards: rows.reduce((sum, row) => sum + row.cards, 0), vramGiB: rows.reduce((sum, row) => sum + row.vramGiB, 0) };
  }).sort((a, b) => b.vramGiB - a.vramGiB || compareVendors(a.vendor, b.vendor));
}

function VendorMark({ vendor }: { vendor: string }) {
  const { t } = useTranslation("common");
  const name = vendor === "amd" ? "AMD" : t(`vendor.${vendor}`, { defaultValue: vendor.toUpperCase() });
  const asset = VENDOR_ASSETS[vendor === "ppu" ? "thead" : vendor];
  return <span className="dashboard-vendor-mark" title={name}>{asset ? <img src={asset} alt={name} style={{ height: VENDOR_LOGO_HEIGHT[vendor === "ppu" ? "thead" : vendor] ?? 14 }} /> : <span>{name}</span>}</span>;
}

export function ModelComposition({ groups }: { groups: VendorGroup[] }) {
  const { t } = useTranslation("dashboard");
  const { t: tc } = useTranslation("common");
  const counts = (cards: number, vramGiB: number) => <><span className="dashboard-model-num"><b>{t("overview.cardsCount", { count: cards })}</b></span><span className="dashboard-model-num">{gibText(vramGiB)}</span></>;
  if (groups.length === 1) {
    const [group] = groups;
    const shown = group.models.slice(0, MAX_SINGLE_VENDOR_ROWS), rest = group.models.slice(shown.length);
    const vendorName = group.vendor === "amd" ? "AMD" : tc(`vendor.${group.vendor}`, { defaultValue: group.vendor.toUpperCase() });
    return <div className="dashboard-models is-single">
      <div className="dashboard-models-single-head"><VendorMark vendor={group.vendor} /><span>{t("overview.singleVendor", { vendor: vendorName })}</span><span className="dashboard-models-single-sum"><b>{t("overview.cardsCount", { count: group.cards })}</b> · {gibText(group.vramGiB)}</span></div>
      {shown.map((model) => <div key={model.name} className="dashboard-model-row is-model"><span className="dashboard-model-name" title={model.name}>{model.name}</span>{counts(model.cards, model.vramGiB)}</div>)}
      {rest.length > 0 && <div className="dashboard-model-row is-model is-more"><span className="dashboard-model-name">{t("overview.moreModelRows", { count: rest.length })}</span>{counts(rest.reduce((sum, model) => sum + model.cards, 0), rest.reduce((sum, model) => sum + model.vramGiB, 0))}</div>}
    </div>;
  }
  const shown = groups.length > MAX_VENDOR_ROWS ? groups.slice(0, MAX_VENDOR_ROWS - 1) : groups;
  const rest = groups.slice(shown.length);
  return <div className="dashboard-models">
    {shown.map((group) => {
      const names = group.models.slice(0, MAX_MODELS_PER_VENDOR).map((model) => model.name).join(tc("resourceTable.valueSeparator"));
      const more = group.models.length - MAX_MODELS_PER_VENDOR;
      return <div key={group.vendor} className="dashboard-model-row" title={group.models.map((model) => `${model.name} · ${t("overview.cardsCount", { count: model.cards })}`).join("\n")}>
        <VendorMark vendor={group.vendor} /><span className="dashboard-model-name">{names}{more > 0 && <em>{t("overview.moreModels", { count: more })}</em>}</span>{counts(group.cards, group.vramGiB)}
      </div>;
    })}
    {rest.length > 0 && <div className="dashboard-model-row is-more"><span className="dashboard-vendor-mark">{t("overview.otherVendors", { count: rest.length })}</span><span className="dashboard-model-name">{rest.map((group) => group.vendor === "amd" ? "AMD" : tc(`vendor.${group.vendor}`, { defaultValue: group.vendor.toUpperCase() })).join(tc("resourceTable.valueSeparator"))}</span>{counts(rest.reduce((sum, group) => sum + group.cards, 0), rest.reduce((sum, group) => sum + group.vramGiB, 0))}</div>}
  </div>;
}
