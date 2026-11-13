import { CloseOutlined, LockOutlined } from "@ant-design/icons";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { SortableCollection, type SortableCollectionItem } from "../ui/sortable-collection";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { useTranslation } from "react-i18next";
import type { ColumnPrefs } from "./columnPrefs";

export interface SettingsColumn {
  key: string;
  label: string;
  role?: "name" | "actions";
}

function LockedItem({ column }: { column: SettingsColumn }) {
  return (
    <div className="flex min-w-0 items-center gap-1.5 px-2 py-2 text-body text-fg-subtle" data-slot="locked-column">
      <span className="grid size-6 shrink-0 place-items-center"><LockOutlined /></span>
      <span className="min-w-0 flex-1 truncate font-medium">{column.label}</span>
    </div>
  );
}

/**
 * The column settings drawer: the shown columns on the left, in order and
 * draggable; every column on the right, as checkboxes. The name and actions
 * columns are always shown and never move.
 */
export default function ColumnSettingsDrawer({ open, columns, prefs, onChange, onReset, onClose }: {
  open: boolean;
  columns: SettingsColumn[];
  prefs: ColumnPrefs;
  onChange: (next: ColumnPrefs) => void;
  onReset: () => void;
  onClose: () => void;
}) {
  const { t } = useTranslation("common");
  const byKey = new Map(columns.map((column) => [column.key, column]));
  const hidden = new Set(prefs.hidden);
  const shown = prefs.order.filter((key) => !hidden.has(key));
  const leading = columns.filter((column) => column.role === "name");
  const trailing = columns.filter((column) => column.role === "actions");
  const setHidden = (key: string, hide: boolean) =>
    onChange({ ...prefs, hidden: hide ? [...prefs.hidden, key] : prefs.hidden.filter((item) => item !== key) });
  // Zeron's sortable list moves the shown columns, by pointer or keyboard
  // (Space or Enter picks up, arrows move, Escape cancels). Hidden columns keep
  // their places in the saved order.
  const items: SortableCollectionItem[] = shown.map((key) => byKey.get(key)).filter((column): column is SettingsColumn => !!column)
    .map((column) => ({ id: column.key, title: column.label, removable: false }));
  const reorder = (next: SortableCollectionItem[]) => {
    const moved = next.map((item) => item.id);
    let index = 0;
    onChange({ ...prefs, order: prefs.order.map((key) => (hidden.has(key) || !byKey.has(key) ? key : moved[index++] ?? key)) });
  };
  const total = columns.length;
  const shownCount = leading.length + shown.length + trailing.length;
  return (
    <SideDrawer
      open={open}
      onClose={onClose}
      width={560}
      title={t("resourceTable.settingsTitle")}
      className="resource-table-settings"
      footer={<Button onClick={onReset}>{t("resourceTable.reset")}</Button>}
    >
      <div className="resource-table-settings-panes">
        <section aria-label={t("resourceTable.selected", { count: shownCount })}>
          <strong className="resource-table-settings-heading">{t("resourceTable.selected", { count: shownCount })}</strong>
          <div className="flex flex-col gap-1 rounded-lg border border-border-subtle bg-muted/70 p-1.5">
            {leading.map((column) => <LockedItem key={column.key} column={column} />)}
            {/* Escape while a column is lifted cancels the move in the list; it
                stops here so the drawer, which closes on Escape, does not close too. */}
            <div onKeyDown={(event) => {
              if (event.key === "Escape" && (event.target as HTMLElement).getAttribute("aria-pressed") === "true") event.stopPropagation();
            }}>
            <SortableCollection className="border-0 bg-transparent p-0" items={items} onItemsChange={reorder}
              reorderLabel={(name) => t("resourceTable.dragColumn", { name })}
              renderActions={(item) => (
                <Button variant="ghost" size="sm" iconOnly onClick={() => setHidden(item.id, true)}
                  aria-label={t("resourceTable.hideColumn", { name: String(item.title) })}><CloseOutlined /></Button>
              )} />
            </div>
            {trailing.map((column) => <LockedItem key={column.key} column={column} />)}
          </div>
        </section>
        <section aria-label={t("resourceTable.all", { shown: shownCount, total })}>
          <strong className="resource-table-settings-heading">{t("resourceTable.all", { shown: shownCount, total })}</strong>
          <div className="resource-table-settings-all">
            {leading.map((column) => <label key={column.key} className="flex items-center gap-2"><Checkbox checked disabled />{column.label}</label>)}
            {prefs.order.map((key) => byKey.get(key)).filter((column): column is SettingsColumn => !!column).map((column) => (
              <label key={column.key} className="flex items-center gap-2"><Checkbox checked={!hidden.has(column.key)} onCheckedChange={(checked) => setHidden(column.key, !checked)} />{column.label}</label>
            ))}
            {trailing.map((column) => <label key={column.key} className="flex items-center gap-2"><Checkbox checked disabled />{column.label}</label>)}
          </div>
        </section>
      </div>
    </SideDrawer>
  );
}
