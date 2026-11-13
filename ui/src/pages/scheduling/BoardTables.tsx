import { Button } from "../../components/ui/button";
import { Input } from "../../components/ui/input";
import { DropdownMenu, DropdownTrigger, DropdownContent } from "../../components/ui/dropdown";
import { MenuItem } from "../../components/ui/menu-item";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { ScheduleMessage } from "../../api";
import type { BoardPending, BoardUpcoming, BoardWaitingWake } from "../../schedulingApi";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { Tooltip, TooltipPortalContainer } from "../../components/ui/tooltip";
import { formatTime } from "../../i18n/datetime";
import { reasonWithFirst } from "../DeploymentPlacementStatus";
import { scheduleText } from "../deploy/deploySchedule";
import { durationText } from "../durationText";

const queuePreviewLimit = 4;

function DeploymentLink({ id, name }: { id: number; name: string }) {
  return <Link to={`/deployments?deployment=${id}`}>{name}</Link>;
}

type QueueKind = "pending" | "waiting" | "upcoming";

function QueuePanel({ kind, count, onOpen, children, note }: {
  kind: QueueKind;
  count: number;
  onOpen: () => void;
  children: ReactNode;
  note?: ReactNode;
}) {
  const { t } = useTranslation("scheduling");
  return (
    <section className="scheduling-queue-panel" aria-label={t(`${kind}.title`)}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-fg-muted">{t(`${kind}.shortTitle`)}</span>
        {count > 0 && (
          <Button variant="link" size="sm" trailingIcon={ConsoleIcon.chevronRight} onClick={onOpen} aria-label={t("queues.showAll", { count })}>
            {kind !== "pending" && count > queuePreviewLimit ? t("queues.showAll", { count }) : t("queues.details")}
          </Button>
        )}
      </div>
      <div className="my-1 mb-2 flex flex-wrap items-baseline gap-1"><strong className="text-2xl font-semibold">{count}</strong>
        <span className="text-label text-fg-muted">
          {kind === "upcoming" ? t("queues.executions", { count }) : t("queues.deployments", { count })}
          {note && <> · {note}</>}
        </span>
      </div>
      {children}
    </section>
  );
}

function QueueCard<T extends { deployment_id: number; deployment: string }>({ id, kind, columns, data, rowKey, empty, preview, note }: {
  id: string;
  kind: QueueKind;
  columns: ResourceColumn<T>[];
  data: T[];
  rowKey: (row: T) => string;
  empty: string;
  preview: (row: T) => ReactNode;
  note?: ReactNode;
}) {
  const { t } = useTranslation("scheduling");
  const [open, setOpen] = useState(false);
  return (
    <QueuePanel kind={kind} count={data.length} note={note} onOpen={() => setOpen(true)}>
      {data.length > 0 ? (
        <ul role="list" className="scheduling-preview-list divide-y divide-border">
          {data.slice(0, queuePreviewLimit).map(row => (
            <li key={rowKey(row)} className="py-1">
              <div className="scheduling-queue-entry scheduling-preview-row">
                <DeploymentLink id={row.deployment_id} name={row.deployment}/>
                {preview(row)}
              </div>
            </li>
          ))}
        </ul>
      ) : <span className="text-fg-muted">{empty}</span>}
      <SideDrawer open={open} onClose={() => setOpen(false)} title={t(`${kind}.title`)} width={960}>
        <ResourceTable<T>
          tableId={id}
          columns={columns}
          data={data}
          rowKey={rowKey}
          pagination={data.length > 10 ? undefined : false}
          columnSettings={false}
          emptyDescription={empty}
        />
      </SideDrawer>
    </QueuePanel>
  );
}

// PendingCard lists deployments the controller could not place, worded the
// way a deployment's own placement alert words them.
export function PendingCard({ rows }: { rows: BoardPending[] }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deploy");
  const render = (value: ScheduleMessage) => scheduleText(tDeploy, value);
  const reasonText = (row: BoardPending) => render(reasonWithFirst(row.reason ?? {}, (first) => render(first).replace(/[。.]$/, ""))) || t("pending.noReason");
  const [selection, setSelection] = useState<{ reason?: string } | null>(null);
  const [tooltipHost, setTooltipHost] = useState<HTMLDivElement | null>(null);
  const [search, setSearch] = useState("");
  const openDetails = (reason?: string) => {
    setSelection({ reason });
    setSearch("");
  };
  const groups = new Map<string, { key: string; label: string; rows: BoardPending[] }>();
  const nameCounts = new Map<string, number>();
  for (const row of rows) {
    nameCounts.set(row.deployment, (nameCounts.get(row.deployment) ?? 0) + 1);
    const outer = row.reason ?? {};
    const first = outer.params?.first_reason;
    const representative = outer.code === "pool_no_fit" && first && typeof first === "object" && ("code" in first || "message" in first)
      ? first as ScheduleMessage : outer;
    const code = representative.code?.trim() ?? "";
    const key = code ? `code:${code}` : "no-code";
    const label = render(representative) || code || t("pending.noReasonCode");
    const group = groups.get(key);
    if (group) {
      group.rows.push(row);
      // Parameterized descriptions can name an individual node. Keep them in the detail rows.
      if (group.label !== label) group.label = code || t("pending.noReasonCode");
    } else {
      groups.set(key, { key, label, rows: [row] });
    }
  }
  const causes = [...groups.values()].sort((a, b) => b.rows.length - a.rows.length || a.key.localeCompare(b.key));
  const selectedGroup = selection?.reason ? groups.get(selection.reason) : undefined;
  const visibleRows = selection?.reason ? selectedGroup?.rows ?? [] : rows;
  const query = search.trim().toLowerCase();
  const filteredRows = query ? visibleRows.filter((row) =>
    row.deployment.toLowerCase().includes(query) || `#${row.deployment_id}`.includes(query) || reasonText(row).toLowerCase().includes(query)
  ) : visibleRows;
  const newestFirst = [...filteredRows].sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
  const columns: ResourceColumn<BoardPending>[] = [
    {
      key: "deployment", title: t("pending.columns.deployment"), width: 180, ellipsis: false, sortValue: (row) => row.deployment,
      render: (_: unknown, row) => (
        <div className="scheduling-queue-entry flex flex-wrap items-baseline gap-1.5">
          <DeploymentLink id={row.deployment_id} name={row.deployment} />
          {(nameCounts.get(row.deployment) ?? 0) > 1 && <span className="text-fg-muted">#{row.deployment_id}</span>}
        </div>
      ),
    },
    {
      key: "reason", title: t("pending.columns.reason"), width: 370,
      render: (_: unknown, row) => {
        const text = reasonText(row);
        return <Tooltip content={text} className="max-w-[min(32rem,90vw)] whitespace-pre-wrap [overflow-wrap:anywhere]">
          <span tabIndex={0} className="block truncate">{text}</span>
        </Tooltip>;
      },
    },
    { key: "at", title: t("pending.columns.at"), width: 160, sortValue: (row) => Date.parse(row.at), render: (_: unknown, row) => formatTime(row.at) },
  ];
  return (
    <QueuePanel kind="pending" count={rows.length} onOpen={() => openDetails()}>
      {rows.length > 0 ? (
        <div className="scheduling-cause-list">
          {causes.slice(0, queuePreviewLimit).map(({ key, label, rows: affected }) => (
            <Button key={key} variant="ghost" contentSized className="scheduling-cause" trailingIcon={ConsoleIcon.chevronRight}
              contentClassName="w-full" labelClassName="flex flex-1 items-center justify-between gap-2" onClick={() => openDetails(key)}>
              <span className="scheduling-cause-text" title={label}>{label}</span>
              <span className="scheduling-cause-count">{t("pending.affected", { count: affected.length })}</span>
            </Button>
          ))}
          {causes.length > queuePreviewLimit && (
            <DropdownMenu>
              <DropdownTrigger render={<Button variant="link" size="sm">{t("pending.otherReasons", { count: causes.length - queuePreviewLimit })}</Button>}/>
              <DropdownContent align="start" className="max-h-[45vh] max-w-[min(460px,90vw)] overflow-auto">
                {causes.slice(queuePreviewLimit).map((group, index) => <MenuItem key={group.key} index={index}
                  label={`${group.label} · ${t("pending.affected", { count: group.rows.length })}`} onSelect={() => openDetails(group.key)}/>)}
              </DropdownContent>
            </DropdownMenu>
          )}
        </div>
      ) : <span className="text-fg-muted">{t("pending.empty")}</span>}
      <SideDrawer open={selection !== null} onClose={() => setSelection(null)} title={t("pending.title")} width={960}>
        {selection?.reason && <div className="scheduling-cause-filter flex flex-wrap items-center gap-2">
          <span>{selectedGroup?.label ?? t("pending.noReason")}</span>
          <Button variant="link" size="sm" onClick={() => openDetails()}>{t("queues.showAll", { count: rows.length })}</Button>
        </div>}
        <div ref={setTooltipHost} />
        <TooltipPortalContainer value={tooltipHost}>
        <Input type="search" placeholder={t("pending.search")} aria-label={t("pending.search")}
          className="mb-2" value={search} onChange={(event) => setSearch(event.target.value)} />
        <ResourceTable<BoardPending>
          key={JSON.stringify([selection?.reason, query])} tableId="scheduling-pending" columns={columns} data={newestFirst} rowKey="deployment_id"
          columnSettings={false} emptyDescription={t(query ? "pending.noMatches" : "pending.empty")}
        />
        </TooltipPortalContainer>
      </SideDrawer>
    </QueuePanel>
  );
}

// WaitingWakesCard lists wakes that wait for room; each can be given up.
export function WaitingWakesCard({ rows, heldRequests, busyID, onCancel }: { rows: BoardWaitingWake[]; heldRequests: number; busyID: number | null; onCancel: (row: BoardWaitingWake) => void }) {
  const { t } = useTranslation("scheduling");
  const cancelButton = (row: BoardWaitingWake) => (
    <Button variant="tertiary" size="sm" loading={busyID === row.deployment_id} aria-label={t("waiting.cancelOf", { name: row.deployment })} onClick={() => onCancel(row)}>
      {t("waiting.cancel")}
    </Button>
  );
  const columns: ResourceColumn<BoardWaitingWake>[] = [
    { key: "deployment", title: t("waiting.columns.deployment"), width: 200, render: (_: unknown, row) => <DeploymentLink id={row.deployment_id} name={row.deployment} /> },
    { key: "instances", title: t("waiting.columns.instances"), width: 260, render: (_: unknown, row) => row.instances.map((id) => id.slice(0, 8)).join(", ") },
    { key: "since", title: t("waiting.columns.since"), width: 160, render: (_: unknown, row) => formatTime(row.since) },
    {
      key: "actions",
      role: "actions",
      width: 160,
      render: (_: unknown, row) => cancelButton(row),
    },
  ];
  return (
    <QueueCard id="scheduling-waiting" kind="waiting" columns={columns} data={rows} rowKey={(row) => String(row.deployment_id)} empty={t("waiting.empty")}
      note={t("waiting.requestCount", { count: heldRequests })}
      preview={(row) => {
        const since = Date.parse(row.since);
        return (
          <div className="scheduling-preview-meta">
            <span className="text-fg-muted" title={formatTime(row.since)}>
              {t("waiting.instances", { count: row.instances.length })}
              {Number.isFinite(since) && <> · {t("waiting.elapsed", { duration: durationText(t, Math.max(0, Date.now() - since) / 1000, "elapsed") })}</>}
            </span>
            {cancelButton(row)}
          </div>
        );
      }} />
  );
}

// UpcomingCard lists what the schedules will do in the next 24 hours.
export function UpcomingCard({ rows }: { rows: BoardUpcoming[] }) {
  const { t } = useTranslation("scheduling");
  const { t: tDeployments } = useTranslation("deployments");
  const actionText = (row: BoardUpcoming) => {
    const key = row.action === "replicas" && row.role ? "roleReplicas" : row.action;
    return t(`upcoming.action.${key}`, { level: row.sleep_level ?? "", count: row.replicas ?? "", role: row.role ? tDeployments(`detail.pd.role.${row.role}`, { defaultValue: row.role }) : "", defaultValue: row.action });
  };
  const columns: ResourceColumn<BoardUpcoming>[] = [
    { key: "at", title: t("upcoming.columns.at"), width: 160, render: (_: unknown, row) => formatTime(row.at) },
    { key: "deployment", title: t("upcoming.columns.deployment"), width: 200, render: (_: unknown, row) => <DeploymentLink id={row.deployment_id} name={row.deployment} /> },
    {
      key: "action",
      title: t("upcoming.columns.action"),
      width: 220,
      render: (_: unknown, row) => actionText(row),
    },
  ];
  return (
    <QueueCard id="scheduling-upcoming" kind="upcoming" columns={columns} data={rows} rowKey={(row) => `${row.schedule_id}-${row.at}`} empty={t("upcoming.empty")}
      preview={(row) => (
        <div className="scheduling-preview-meta">
          <span className="text-fg-muted">{actionText(row)}</span>
          <span className="text-fg-muted">{formatTime(row.at)}</span>
        </div>
      )} />
  );
}
