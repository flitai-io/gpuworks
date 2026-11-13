import { Fragment, useEffect, useMemo, useState, type CSSProperties, type ReactNode, type Key } from "react";
import type { TableProps } from "antd";
import type { ColumnType, TablePaginationConfig } from "antd/es/table/interface";
import { type ColumnDef, type ColumnFiltersState, type SortingState, type RowSelectionState, type ExpandedState } from "@tanstack/react-table";
import { useTranslation } from "react-i18next";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Table, TableHeader, TableHead, TableBody, TableRow, TableCell } from "../ui/table";
import { useDataTable, DataTablePagination, DataTableColumnHeader } from "../ui/data-table";
import { ColumnFilter } from "../../ui-kit/ColumnFilter";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";

const ClearIcon = ConsoleIcon.close;
const RefreshIcon = ConsoleIcon.refresh;
const ColumnsIcon = ConsoleIcon.settings;
const ExpandedIcon = ConsoleIcon.chevronDown;
const CollapsedIcon = ConsoleIcon.chevronRight;
import { Spinner } from "../../ui-kit/Spinner";
import EmptyState, { type EmptyStateKind } from "../EmptyState";
import { formatClock, formatTime } from "../../i18n/datetime";
import ColumnSettingsDrawer from "./ColumnSettingsDrawer";
import { PAGE_SIZE_OPTIONS, arrangeColumns, defaultColumnPrefs, loadColumnPrefs, minWidthOf, saveColumnPrefs, widthOf, type ColumnPrefs } from "./columnPrefs";
export type SortValue = string | number | null | undefined;

let measureContext: CanvasRenderingContext2D | null | undefined;
// Measured titles, by label and marks: reading the body font forces a style
// recalculation, which each render of a long list used to pay once per column.
const titleWidths = new Map<string, number>();
/** Width a header needs: the title in the header font, cell padding, and the sort and filter marks. */
function titleWidth(label: string, sortable: boolean, filterable: boolean) {
  if (measureContext === undefined) {
    try { measureContext = document.createElement("canvas").getContext("2d"); } catch { measureContext = null; }
  }
  // No canvas (tests, very old browsers): keep the declared width.
  if (!measureContext) return 0;
  const key = `${sortable ? 1 : 0}${filterable ? 1 : 0}${label}`;
  const known = titleWidths.get(key);
  if (known !== undefined) return known;
  if (!titleWidths.size) measureContext.font = `600 14px ${getComputedStyle(document.body).fontFamily}`;
  const width = Math.ceil(measureContext.measureText(label).width) + 24 + (sortable ? 28 : 0) + (filterable ? 28 : 0) + 8;
  titleWidths.set(key, width);
  return width;
}

/**
 * One column of a resource list. The transitional column contract names a stable
 * key (preferences are saved under it), a default width, and its role: the
 * name column leads and freezes left, the actions column trails and freezes
 * right, and neither hides nor moves.
 */
export type ResourceColumn<T> = Omit<ColumnType<T>, "key" | "width" | "fixed" | "sorter" | "sortOrder"> & {
  key: string;
  width: number;
  minWidth?: number;
  /** The column's name in the settings drawer when the title is not plain text. */
  label?: string;
  role?: "name" | "actions";
  defaultHidden?: boolean;
  /** Sorts a client-side list by this value. */
  sortValue?: (row: T) => SortValue;
  /** The list is sorted by the server under this column's key. */
  serverSort?: boolean;
};

export interface ServerSort {
  key: string;
  order: "asc" | "desc";
}

/** The page size a table last used in this browser, for a server-paged list's first request. */
export function storedPageSize(tableId: string, fallback: number): number {
  return loadColumnPrefs(tableId, []).pageSize ?? fallback;
}

export function compareSortValues(a: SortValue, b: SortValue): number {
  const missingA = a == null || a === "";
  const missingB = b == null || b === "";
  if (missingA || missingB) return missingA === missingB ? 0 : missingA ? -1 : 1;
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: "base" });
}


const EMPTY_ROWS: never[] = [];
export default function ResourceTable<T extends object>({ tableId, columns, data, rowKey, loading, error, onRefresh, updatedAt,
  toolbar, toolbarExtra, pagination, sort, onSortChange, onFiltersChange, rowSelection, expandable, onRow,
  emptyDescription, emptyKind = "data", emptyAction, columnSettings = true,
}: {
  tableId: string; columns: ResourceColumn<T>[]; data?: T[]; rowKey: TableProps<T>["rowKey"];
  loading?: boolean; error?: Error | null; onRefresh?: () => void; updatedAt?: Date | null;
  toolbar?: ReactNode; toolbarExtra?: ReactNode; pagination?: TablePaginationConfig | false;
  sort?: ServerSort | null; onSortChange?: (sort: ServerSort | null) => void;
  onFiltersChange?: (filters: Record<string, string[]>) => void;
  rowSelection?: TableProps<T>["rowSelection"]; expandable?: TableProps<T>["expandable"]; onRow?: TableProps<T>["onRow"];
  emptyDescription?: string; emptyKind?: EmptyStateKind; emptyAction?: ReactNode; columnSettings?: boolean;
}) {
  const { t } = useTranslation("common");
  const rows = data ?? EMPTY_ROWS;
  const [prefs, setPrefs] = useState<ColumnPrefs>(() => loadColumnPrefs(tableId, columns));
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [localPage, setLocalPage] = useState({ pageIndex: 0, pageSize: prefs.pageSize ?? 20 });
  const [localSort, setLocalSort] = useState<SortingState>([]);
  // A column's defaultFilteredValue is where its filter starts; the user changes it from there.
  const [filters, setFilters] = useState<ColumnFiltersState>(() => columns.filter((column) => column.defaultFilteredValue?.length).map((column) => ({ id: column.key, value: column.defaultFilteredValue })));
  const [localSelection, setLocalSelection] = useState<RowSelectionState>({});
  const [expanded, setExpanded] = useState<ExpandedState>(() => Object.fromEntries((expandable?.defaultExpandedRowKeys ?? []).map((key) => [String(key), true])));
  const columnKeys = columns.map((column) => column.key).join("|");
  useEffect(() => { setPrefs(loadColumnPrefs(tableId, columns)); }, [tableId, columnKeys]);
  const update = (next: ColumnPrefs) => { setPrefs(next); saveColumnPrefs(tableId, next); };
  const arranged = useMemo(() => arrangeColumns(columns, prefs), [columns, prefs]);
  const keyOf = (record: T, index: number): Key => typeof rowKey === "function" ? rowKey(record, index) : (record as Record<string, Key>)[String(rowKey ?? "key")];
  const selectedKeys = rowSelection?.selectedRowKeys;
  const selection = selectedKeys ? Object.fromEntries(selectedKeys.map((key) => [String(key), true])) : localSelection;
  const controlledFilters = columns.filter((column) => column.filteredValue != null).map((column) => ({ id: column.key, value: column.filteredValue }));
  const allFilters = [...filters.filter((item) => !controlledFilters.some((other) => other.id === item.id)), ...controlledFilters];
  const page = pagination ? { pageIndex: (pagination.current ?? 1) - 1, pageSize: pagination.pageSize ?? prefs.pageSize ?? 20 } : localPage;
  const sorting = onSortChange ? sort ? [{ id: sort.key, desc: sort.order === "desc" }] : [] : localSort;
  // Hidden columns stay in the table, invisible, so a filter set on one before
  // it was hidden keeps filtering the rows its chip says it does.
  const hiddenColumns = columns.filter((column) => !arranged.includes(column));
  const defs: ColumnDef<T, unknown>[] = [...arranged, ...hiddenColumns].map((column) => ({
    id: column.key, size: widthOf(column, prefs), minSize: minWidthOf(column),
    accessorFn: (record) => {
      if (column.sortValue) return column.sortValue(record);
      const path = Array.isArray(column.dataIndex) ? column.dataIndex : [column.dataIndex ?? column.key];
      return path.reduce<unknown>((value, key) => value && typeof value === "object" ? (value as Record<string, unknown>)[String(key)] : undefined, record);
    },
    enableSorting: onSortChange ? !!column.serverSort : !!column.sortValue,
    // Columns are shown and hidden in the column settings drawer, not from a header menu.
    enableHiding: false,
    sortDescFirst: false, sortingFn: (a, b, id) => compareSortValues(a.getValue(id), b.getValue(id)),
    filterFn: (row, _id, values: unknown) => !Array.isArray(values) || values.length === 0 || values.some((value) => column.onFilter?.(value, row.original)),
    cell: ({ row, getValue }) => {
      const content = column.render ? column.render(getValue(), row.original, row.index) : getValue() as ReactNode;
      return content && typeof content === "object" && "children" in content && "props" in content ? content.children : content as ReactNode;
    },
  }));
  const { table } = useDataTable({
    data: rows, columns: defs, getRowId: (record, index) => String(keyOf(record, index)),
    manualPagination: !!pagination || pagination === false, manualSorting: !!onSortChange, manualFiltering: !!onFiltersChange,
    rowCount: pagination ? pagination.total : undefined,
    autoResetPageIndex: false, enableMultiSort: false,
    enableRowSelection: (row) => !rowSelection?.getCheckboxProps?.(row.original).disabled,
    getRowCanExpand: (row) => !!expandable && (expandable.rowExpandable?.(row.original) ?? true),
    state: { pagination: page, sorting, columnFilters: allFilters, rowSelection: selection,
      columnVisibility: Object.fromEntries(hiddenColumns.map((column) => [column.key, false])),
      expanded: expandable?.expandedRowKeys ? Object.fromEntries(expandable.expandedRowKeys.map((key) => [String(key), true])) : expanded },
    onExpandedChange: (updater) => {
      const next = typeof updater === "function" ? updater(table.getState().expanded) : updater;
      setExpanded(next);
      expandable?.onExpandedRowsChange?.(next === true ? rows.map(keyOf) : Object.keys(next).filter((key) => next[key]));
    },
    onPaginationChange: (updater) => {
      const next = typeof updater === "function" ? updater(page) : updater;
      if (next.pageSize !== page.pageSize) { next.pageIndex = 0; update({ ...prefs, pageSize: next.pageSize }); }
      if (pagination) pagination.onChange?.(next.pageIndex + 1, next.pageSize);
      else setLocalPage(next);
    },
    onSortingChange: (updater) => {
      const next = typeof updater === "function" ? updater(sorting) : updater;
      if (onSortChange) onSortChange(next[0] ? { key: next[0].id, order: next[0].desc ? "desc" : "asc" } : null);
      else setLocalSort(next);
    },
    onColumnFiltersChange: (updater) => {
      const next = typeof updater === "function" ? updater(allFilters) : updater;
      setFilters(next);
      if (!pagination) setLocalPage((current) => ({ ...current, pageIndex: 0 }));
      onFiltersChange?.(Object.fromEntries(columns.filter((column) => column.filters).map((column) => [column.key, (next.find((item) => item.id === column.key)?.value as unknown[] ?? []).map(String)])));
    },
    onRowSelectionChange: (updater) => {
      const next = typeof updater === "function" ? updater(selection) : updater;
      setLocalSelection(next);
      const knownKeys = new Map(rows.map((record, index) => [String(keyOf(record, index)), keyOf(record, index)]));
      selectedKeys?.forEach((key) => knownKeys.set(String(key), key));
      const keys = Object.keys(next).filter((key) => next[key]).map((key) => knownKeys.get(key) ?? key);
      rowSelection?.onChange?.(keys, rows.filter((record, index) => next[String(keyOf(record, index))]), { type: "multiple" });
    },
  });
  useEffect(() => {
    if (!pagination && localPage.pageIndex >= table.getPageCount()) setLocalPage((current) => ({ ...current, pageIndex: Math.max(0, table.getPageCount() - 1) }));
  }, [pagination, rows.length, localPage.pageIndex, localPage.pageSize, allFilters.length]);
  const titleOf = (column: ResourceColumn<T>): ReactNode => typeof column.title === "function" ? labelOf(column) : column.title ?? labelOf(column);
  const labelOf = (column: ResourceColumn<T>) => column.label ?? (typeof column.title === "string" ? column.title : column.role === "actions" ? t("resourceTable.actions") : column.key);
  // A column the user has not resized is at least wide enough for its whole
  // title plus its sort and filter marks, so titles are never cut by default.
  const widths = arranged.map((column) => prefs.widths[column.key] != null ? widthOf(column, prefs)
    : Math.max(widthOf(column, prefs), titleWidth(labelOf(column), !!table.getColumn(column.key)?.getCanSort(), !!column.filters)));
  const totalWidth = widths.reduce((sum, width) => sum + width, (rowSelection ? 48 : 0) + (expandable ? 36 : 0));
  const pinnedStyle = (column: ResourceColumn<T>, index: number): CSSProperties => ({
    width: widths[index], minWidth: widths[index], textAlign: column.align,
    ...(column.role === "name" ? { position: "sticky", left: (rowSelection ? 48 : 0) + (expandable ? 36 : 0), zIndex: 1, backgroundColor: "var(--surface-floating)" } : {}),
    ...(column.role === "actions" ? { position: "sticky", right: 0, zIndex: 1, backgroundColor: "var(--surface-floating)" } : {}),
  });
  const resize = (column: ResourceColumn<T>, width: number) => update({ ...prefs, widths: { ...prefs.widths, [column.key]: Math.max(minWidthOf(column), width) } });
  // Every filter in effect, including one on a column the user has hidden, so it can always be seen and cleared.
  const activeFilters = columns.filter((column) => column.filters).map((column) => {
    const value = allFilters.find((item) => item.id === column.key)?.value;
    const picked = Array.isArray(value) ? value.map(String) : [];
    const text = (key: string) => { const option = column.filters!.find((item) => String(item.value) === key); return option && typeof option.text === "string" ? option.text : key; };
    return { column, values: picked.map(text) };
  }).filter((item) => item.values.length > 0);
  return <div className="resource-table">
    <div className="resource-table-toolbar">
      <div className="resource-table-toolbar-left">{toolbar}</div>
      <div className="resource-table-toolbar-right flex flex-nowrap items-center gap-2">
        {toolbarExtra}{updatedAt && <span className="resource-table-updated whitespace-nowrap text-label text-fg-muted" title={t("dataTable.updatedAt", { time: formatTime(updatedAt) })}>{t("dataTable.updatedAt", { time: formatClock(updatedAt) })}</span>}
        {/* Same family and size as the menu and top bar: HugeIcons at 16px in a 32px control that lines up with the search box. */}
        {onRefresh && <Button iconOnly variant="tertiary" className="[&_svg]:size-4!" aria-label={t("dataTable.refresh")} onClick={onRefresh}><RefreshIcon /></Button>}
        {columnSettings && <Button iconOnly variant="tertiary" className="[&_svg]:size-4!" aria-label={t("resourceTable.settings")} onClick={() => setSettingsOpen(true)}><ColumnsIcon /></Button>}
      </div>
    </div>
    {/* Filters are set from the column header funnels; the ones in effect are listed here
        as "column: values" so a filter on an off-screen column is never invisible. */}
    {activeFilters.length > 0 && <div className="resource-table-filters flex flex-wrap items-center gap-1.5" role="group" aria-label={t("resourceTable.activeFilters")}>
      <span className="text-label text-fg-subtle">{t("resourceTable.activeFilters")}</span>
      {activeFilters.map(({ column, values }) => <Button key={column.key} size="xs" variant="tertiary" className="resource-table-filter-chip" trailingIcon={ClearIcon}
        aria-label={t("resourceTable.clearFilter", { name: labelOf(column), values: values.join(t("resourceTable.valueSeparator")) })}
        onClick={() => table.setColumnFilters((current) => current.filter((item) => item.id !== column.key))}>
        <span className="font-semibold">{labelOf(column)}</span>{t("resourceTable.filterValues", { values: values.join(t("resourceTable.valueSeparator")) })}
      </Button>)}
      {activeFilters.length > 1 && <Button size="xs" variant="ghost" className="text-fg-brand" onClick={() => table.resetColumnFilters()}>{t("resourceTable.clearFilters")}</Button>}
    </div>}
    {error ? <div role="alert" className="flex flex-wrap items-center gap-2 text-fg-danger"><strong>{t("dataTable.loadFailed")}</strong><span>{error.message}</span>{onRefresh && <Button onClick={onRefresh}>{t("actions.retry")}</Button>}</div> : <>
      {loading && <Spinner label={t("dataTable.refresh")} />}
      {/* DataTable has no row-props or header-cell slot at the pinned release. Table preserves
          row-specific state/title and resize handles; useDataTable owns all query mechanics. */}
      {/* Medium size: cells 14px like Zeron's DataTable, so a column's first line
          never falls back to 12px; second lines are 12px by their own class. */}
      {/* Separate borders: with collapsed ones Chrome paints row rules under the
          opaque sticky cells, so the name and action columns lost them. */}
      <Table variant="bordered" size="md" className="border-separate border-spacing-0" containerClassName="max-w-full overflow-x-auto [container-type:inline-size]" style={{ tableLayout: "fixed", width: totalWidth, minWidth: "100%" }} aria-busy={loading}>
        <TableHeader><TableRow>
          {rowSelection && <TableHead style={{ width: 48, position: "sticky", left: 0, zIndex: 2, background: "var(--surface-raised)" }}><Checkbox aria-label={t("resourceTable.selectPage")} checked={table.getIsAllPageRowsSelected()} indeterminate={table.getIsSomePageRowsSelected()} onCheckedChange={(checked) => table.toggleAllPageRowsSelected(checked)} /></TableHead>}
          {expandable && <TableHead style={{ width: 36, position: "sticky", left: rowSelection ? 48 : 0, zIndex: 2, background: "var(--surface-raised)" }} />}
          {arranged.map((column, index) => {
            const control = table.getColumn(column.key)!;
            return <TableHead key={column.key} style={{ ...pinnedStyle(column, index), ...(column.role === "name" || column.role === "actions" ? { backgroundColor: "var(--surface-raised)" } : {}) }} aria-label={labelOf(column)} aria-sort={control.getIsSorted() === "asc" ? "ascending" : control.getIsSorted() === "desc" ? "descending" : undefined}>
              {/* A title longer than its column is cut with an ellipsis (full text on hover), never drawn over the next one. */}
              <div className="relative flex min-w-0 items-center gap-1">
                {/* Sortable titles carry Zeron's up/down mark and its sort menu, so they read as controls (ui-guidelines 3). */}
                {control.getCanSort()
                  // A rich title (with a help tooltip) renders as-is; Zeron only places the label.
                  ? <DataTableColumnHeader column={control} label={titleOf(column) as string} aria-label={labelOf(column)} title={labelOf(column)}
                    className="min-w-0 max-w-full font-semibold [&_span]:min-w-0 [&_svg]:shrink-0 [&_[data-slot=button-label]]:truncate" labels={{ ascending: t("resourceTable.sortAscending"), descending: t("resourceTable.sortDescending"), resetSorting: t("resourceTable.sortReset") }} />
                  : <span className="min-w-0 truncate" title={labelOf(column)}>{titleOf(column)}</span>}
                {column.filters && <ColumnFilter column={control} title={t("resourceTable.filterByColumn", { name: labelOf(column) })} multiple={column.filterMultiple !== false} clearLabel={t("resourceTable.clearFilters")} search={column.filterSearch ? t("resourceTable.searchOptions") : undefined}
                  options={column.filters.map((item) => ({ value: String(item.value), label: typeof item.text === "string" ? item.text : String(item.value) }))} />}

                {column.role !== "actions" && <span role="separator" className="resource-table-resizer" tabIndex={0} aria-orientation="vertical" aria-label={t("resourceTable.resizeColumn", { name: labelOf(column) })} aria-valuenow={widths[index]}
                  onKeyDown={(event) => { if (event.key === "ArrowLeft" || event.key === "ArrowRight") { event.preventDefault(); resize(column, widths[index] + (event.key === "ArrowLeft" ? -16 : 16)); } }}
                  onPointerDown={(event) => {
                    event.preventDefault(); event.stopPropagation();
                    const start = event.clientX; const width = widths[index];
                    const move = (next: PointerEvent) => resize(column, width + next.clientX - start);
                    const end = () => { window.removeEventListener("pointermove", move); window.removeEventListener("pointerup", end); window.removeEventListener("pointercancel", end); };
                    window.addEventListener("pointermove", move); window.addEventListener("pointerup", end); window.addEventListener("pointercancel", end);
                  }} />}
              </div>
            </TableHead>;
          })}
        </TableRow></TableHeader>
        <TableBody>
          {table.getRowModel().rows.map((row, index) => {
            const props = onRow?.(row.original, row.index) ?? {};
            const checkboxProps = rowSelection?.getCheckboxProps?.(row.original) as { title?: string; "aria-label"?: string } | undefined;
            const expand = () => { expandable?.onExpand?.(!row.getIsExpanded(), row.original); row.toggleExpanded(); };
            return <Fragment key={row.id}><TableRow {...props} className={[props.className, "data-[state=selected]:bg-selection border-b-0"].filter(Boolean).join(" ")} index={index} data-row-key={row.id} data-state={row.getIsSelected() ? "selected" : undefined}
              onClick={(event) => { props.onClick?.(event); if (expandable?.expandRowByClick && row.getCanExpand() && !(event.target as HTMLElement).closest("button,a,input,[role=checkbox]")) expand(); }}>
              {rowSelection && <TableCell className="resource-table-pinned resource-table-cell" style={{ position: "sticky", left: 0, zIndex: 1, backgroundColor: row.getIsSelected() ? "var(--selection)" : "var(--surface-floating)" }}><Checkbox title={checkboxProps?.title} aria-label={checkboxProps?.["aria-label"] ?? t("resourceTable.selectRow", { name: row.id })} checked={row.getIsSelected()} disabled={!row.getCanSelect()} onCheckedChange={(checked) => row.toggleSelected(checked)} onClick={(event) => event.stopPropagation()} /></TableCell>}
              {expandable && <TableCell className="resource-table-pinned resource-table-cell" style={{ position: "sticky", left: rowSelection ? 48 : 0, zIndex: 1, backgroundColor: "var(--surface-floating)" }}>{row.getCanExpand() && (expandable.expandIcon ? expandable.expandIcon({ prefixCls: "resource-table", expanded: row.getIsExpanded(), expandable: true, record: row.original, onExpand: (_record, event) => { event.stopPropagation(); expand(); } }) : <Button size="xs" variant="ghost" iconOnly aria-label={t(row.getIsExpanded() ? "resourceTable.collapseRow" : "resourceTable.expandRow")} aria-expanded={row.getIsExpanded()} onClick={(event) => { event.stopPropagation(); expand(); }}>{row.getIsExpanded() ? <ExpandedIcon size={16} /> : <CollapsedIcon size={16} />}</Button>)}</TableCell>}
              {row.getVisibleCells().map((cell, cellIndex) => <TableCell key={cell.id} className={["resource-table-cell whitespace-nowrap", arranged[cellIndex].role === "actions" ? "" : "overflow-hidden text-ellipsis", arranged[cellIndex].role === "name" ? "resource-table-name" : "", arranged[cellIndex].role === "name" || arranged[cellIndex].role === "actions" ? "resource-table-pinned" : "", arranged[cellIndex].className].filter(Boolean).join(" ")} style={{ ...pinnedStyle(arranged[cellIndex], cellIndex), ...(row.getIsSelected() ? { background: "var(--selection)" } : {}) }}>{typeof cell.column.columnDef.cell === "function" ? cell.column.columnDef.cell(cell.getContext()) : cell.column.columnDef.cell}</TableCell>)}
            </TableRow>{row.getIsExpanded() && expandable?.expandedRowRender && <TableRow><TableCell colSpan={arranged.length + (rowSelection ? 1 : 0) + 1}>{expandable.expandedRowRender(row.original, row.index, 0, true)}</TableCell></TableRow>}</Fragment>;
          })}
          {!table.getRowModel().rows.length && <TableRow><TableCell colSpan={arranged.length + (rowSelection ? 1 : 0) + (expandable ? 1 : 0)}>{/* The empty state holds to the visible part of a table wider than its container (100cqw
              is the container's width, the cell pads 12px a side), so it centers in what the user sees. */}
            {!loading && <div className="sticky left-3 w-[calc(100cqw-1.5rem)]"><EmptyState kind={emptyKind} description={emptyDescription ?? t("dataTable.empty")} action={emptyAction} /></div>}</TableCell></TableRow>}
        </TableBody>
      </Table>
      {/* One footer row: the total on the left, Zeron's pager on the right, both in its 12px label size. */}
      {pagination !== false && <div className="resource-table-footer flex items-center justify-between gap-4 pt-2"><span className="shrink-0 whitespace-nowrap text-label tabular-nums text-fg-muted">{t("resourceTable.total", { count: pagination ? pagination.total ?? rows.length : table.getFilteredRowModel().rows.length })}</span><DataTablePagination className="w-auto min-w-0 flex-1 px-0 py-0" table={table} pageSizeOptions={PAGE_SIZE_OPTIONS} labels={{
        firstPage: t("resourceTable.firstPage"), lastPage: t("resourceTable.lastPage"), nextPage: t("resourceTable.nextPage"), previousPage: t("resourceTable.previousPage"), rowsPerPage: t("resourceTable.rowsPerPage"),
        pageSummary: (current, count) => t("resourceTable.pageSummary", { current, count }),
        selectedRows: (_selected, total) => t("resourceTable.total", { count: pagination ? pagination.total ?? total : table.getFilteredRowModel().rows.length }),
      }} /></div>}
      <ColumnSettingsDrawer open={settingsOpen} columns={columns.map((column) => ({ key: column.key, role: column.role, label: labelOf(column) }))} prefs={prefs} onChange={update} onReset={() => update({ ...defaultColumnPrefs(columns), ...(prefs.pageSize ? { pageSize: prefs.pageSize } : {}) })} onClose={() => setSettingsOpen(false)} />
    </>}
  </div>;
}
