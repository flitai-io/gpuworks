import { useCallback, useLayoutEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { ScrollArea } from "../../components/ui/scroll-area";
import type { NetworkTestPair } from "../../networkTypes";

type NodeAxis = [string, string][];
type Props = {
  rows: NodeAxis;
  cols: NodeAxis;
  pairs: Map<string, Map<string, NetworkTestPair>>;
  renderPair: (pair: NetworkTestPair) => ReactNode;
};

const rowHeight = 44;
const columnWidth = 108;
const nameWidth = 124;
const headingHeight = 32;
const scrollbarSize = 12;
const overscan = 2;

// Keep the native table's full scroll extent using spacers for offscreen
// rows and columns. The sticky row and column headers share the cell grid.
export default function NetworkMatrixViewport({ rows, cols, pairs, renderPair }: Props) {
  const { t } = useTranslation("networks");
  const viewport = useRef<HTMLDivElement>(null);
  const pendingFocus = useRef<[number, number] | null>(null);
  const [range, setRange] = useState({ top: 0, bottom: 16, left: 0, right: 10 });

  const measure = useCallback(() => {
    const el = viewport.current;
    if (!el || !el.clientHeight || !el.clientWidth) return;
    const row = Math.floor(el.scrollTop / rowHeight);
    const col = Math.floor(el.scrollLeft / columnWidth);
    const top = Math.max(0, row - overscan);
    const left = Math.max(0, col - overscan);
    const bottom = Math.min(rows.length, row + Math.ceil(el.clientHeight / rowHeight) + overscan);
    const right = Math.min(cols.length, col + Math.ceil(el.clientWidth / columnWidth) + overscan);
    setRange(prev => prev.top === top && prev.bottom === bottom && prev.left === left && prev.right === right
      ? prev : { top, bottom, left, right });
  }, [rows.length, cols.length]);

  useLayoutEffect(() => {
    const el = viewport.current;
    if (!el) return;
    pendingFocus.current = null;
    el.scrollTop = 0;
    el.scrollLeft = 0;
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [rows, cols, measure]);

  function focusCell(row: number, col: number) {
    const button = viewport.current?.querySelector<HTMLButtonElement>(`td[data-row="${row}"][data-col="${col}"] button`);
    if (!button) return false;
    button.focus({ preventScroll: true });
    return true;
  }

  useLayoutEffect(() => {
    const next = pendingFocus.current;
    if (next && focusCell(...next)) pendingFocus.current = null;
  }, [range]);

  function navigate(event: KeyboardEvent<HTMLTableElement>) {
    const cell = (event.target as HTMLElement).closest<HTMLTableCellElement>("td[data-row]");
    const el = viewport.current;
    if (!cell || !el || event.altKey || event.ctrlKey || event.metaKey) return;
    const horizontal = event.key === "ArrowRight" || event.key === "ArrowLeft";
    if (!horizontal && event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    const step = event.key === "ArrowRight" || event.key === "ArrowDown" ? 1 : -1;
    let row = Number(cell.dataset.row), col = Number(cell.dataset.col);
    event.preventDefault();
    do {
      if (horizontal) col += step;
      else row += step;
      if (row < 0 || col < 0 || row >= rows.length || col >= cols.length) return;
    } while (!pairs.get(rows[row][0])?.has(cols[col][0]));
    // Include the frozen axes and scrollbar hit targets when revealing a cell.
    el.scrollTop = Math.max((row + 1) * rowHeight + headingHeight + scrollbarSize - el.clientHeight, Math.min(el.scrollTop, row * rowHeight));
    el.scrollLeft = Math.max((col + 1) * columnWidth + nameWidth + scrollbarSize - el.clientWidth, Math.min(el.scrollLeft, col * columnWidth));
    if (!focusCell(row, col)) pendingFocus.current = [row, col];
    measure();
  }

  const visibleRows = rows.slice(range.top, range.bottom);
  const visibleCols = cols.slice(range.left, range.right);
  const leftWidth = range.left * columnWidth;
  const rightWidth = Math.max(0, cols.length - range.right) * columnWidth;
  const span = 1 + visibleCols.length + Number(leftWidth > 0) + Number(rightWidth > 0);
  const spacer = (height: number) => height > 0 && <tr aria-hidden="true"><td colSpan={span} style={{ height }} /></tr>;
  return <ScrollArea orientation="both" viewportRef={viewport} viewportProps={{ onScroll: measure, style: { overflowAnchor: "none" }, "aria-label": t("test.globalMatrix") }}
    style={{ height: `min(54vh, 30rem, ${headingHeight + rows.length * rowHeight + scrollbarSize + 2}px)` }}
    className="isolate max-w-full rounded-md border border-border [&_[data-slot=scroll-area-scrollbar]]:opacity-100"
    contentClassName="pr-3 pb-3">
    <table aria-label={t("test.globalMatrix")} aria-rowcount={rows.length + 1} aria-colcount={cols.length + 1} onKeyDown={navigate}
      className="table-fixed border-separate border-spacing-0 text-label" style={{ width: nameWidth + cols.length * columnWidth }}>
      <colgroup>
        <col style={{ width: nameWidth }} />
        {leftWidth > 0 && <col style={{ width: leftWidth }} />}
        {visibleCols.map(([id]) => <col key={id} style={{ width: columnWidth }} />)}
        {rightWidth > 0 && <col style={{ width: rightWidth }} />}
      </colgroup>
      <thead><tr aria-rowindex={1} style={{ height: headingHeight }}>
        <th scope="col" aria-colindex={1} className="sticky top-0 left-0 z-20 whitespace-nowrap bg-surface-raised px-2">{t("test.matrix")}</th>
        {leftWidth > 0 && <th aria-hidden="true" />}
        {visibleCols.map(([id, name], i) => <th scope="col" aria-colindex={range.left + i + 2} key={id} title={name}
          className="sticky top-0 z-10 bg-surface-raised px-1">
          <span className="block truncate">{name}</span>
        </th>)}
        {rightWidth > 0 && <th aria-hidden="true" />}
      </tr></thead>
      <tbody>
        {spacer(range.top * rowHeight)}
        {visibleRows.map(([sourceID, name], i) => {
          const row = range.top + i;
          return <tr key={sourceID} aria-rowindex={row + 2} style={{ height: rowHeight }}>
            <th scope="row" aria-colindex={1} title={name} className="sticky left-0 z-10 bg-surface-raised px-2 text-left">
              <span className="block truncate">{name}</span>
            </th>
            {leftWidth > 0 && <td aria-hidden="true" />}
            {visibleCols.map(([targetID], j) => {
              const col = range.left + j;
              const pair = pairs.get(sourceID)?.get(targetID);
              return <td key={targetID} data-row={row} data-col={col} aria-colindex={col + 2} style={{ height: rowHeight }} className="p-px text-center">
                {pair ? renderPair(pair) : sourceID === targetID ? "—" : null}
              </td>;
            })}
            {rightWidth > 0 && <td aria-hidden="true" />}
          </tr>;
        })}
        {spacer((rows.length - range.bottom) * rowHeight)}
      </tbody>
    </table>
  </ScrollArea>;
}
