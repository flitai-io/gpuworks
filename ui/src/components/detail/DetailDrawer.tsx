import { RightOutlined } from "@ant-design/icons";
import { SideDrawer } from "../../ui-kit/SideDrawer";
import { Tabs, TabsList, TabItem, TabPanel } from "../ui/tabs";
import type { TableProps } from "antd";
import { useEffect, useState, type ReactNode } from "react";

// As wide as the deployment drawer: room for a detail's tables without
// scrolling sideways, never wider than the window less a margin.
export const DETAIL_DRAWER_WIDTH = "min(720px, calc(100vw - 32px))";

export type DetailTab = {
  key: string;
  label: string;
  // A resource count shown as a pill beside the label, apart from its text.
  count?: number;
  countTitle?: string;
  children: ReactNode;
};

// DetailDrawer is the frame every resource detail opens in: the title (the
// resource's name, then a secondary line such as its hostname), the actions,
// anything that must stay in view above the tabs (banner), and the tabs, whose
// bar stays at the top while a tab scrolls. Children follow the tabs: a
// detail with one view passes only children; dialogs the detail opens go
// there too.
export default function DetailDrawer({
  open,
  onClose,
  title,
  subtitle,
  extra,
  banner,
  tabs,
  activeTab,
  onTabChange,
  className,
  children,
}: {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  subtitle?: ReactNode;
  extra?: ReactNode;
  banner?: ReactNode;
  tabs?: DetailTab[];
  activeTab?: string;
  onTabChange?: (key: string) => void;
  className?: string;
  children?: ReactNode;
}) {
  const selected = activeTab ?? tabs?.[0]?.key;
  const [visited, setVisited] = useState<Set<string>>(() => new Set());
  useEffect(() => {
    if (!open) { setVisited(new Set()); return; }
    if (selected) setVisited(previous => previous.has(selected) ? previous : new Set([...previous, selected]));
  }, [open, selected]);
  const changeTab = (key: string) => {
    setVisited(previous => new Set([...previous, key]));
    onTabChange?.(key);
  };
  return (
    <SideDrawer title={title}
      open={open} onClose={onClose} width={DETAIL_DRAWER_WIDTH} className={className}
      subtitle={subtitle} extra={extra}>
      {banner}
      {tabs && <Tabs className="detail-tabs" value={activeTab} defaultValue={tabs[0]?.key} onValueChange={changeTab} variant="underline">
        <TabsList className="sticky top-0 z-10 bg-surface-overlay">
          {tabs.map((tab) => <TabItem key={tab.key} value={tab.key} label={tab.label}
            badge={tab.count == null ? undefined : <span title={tab.countTitle}>{tab.count}</span>} />)}
        </TabsList>
        {/* Zeron's tab list overhangs 4px below itself; the panel starts 16px under the tabs, as antd's did. */}
        {tabs.map((tab) => <TabPanel key={tab.key} value={tab.key} keepMounted={visited.has(tab.key)} className="pt-4">{tab.children}</TabPanel>)}
      </Tabs>}
      {children}
    </SideDrawer>
  );
}

// detailExpandable is a detail table's expand behaviour: a 22px bordered
// chevron, the whole row clickable, and the expanded row one step down from
// its row (detailDrawer.css). Links in a row stop the click from expanding it.
// A table whose row click already does something else (opens the row's own
// view) passes rowClick: false and expands by its chevron only.
export function detailExpandable<T>(
  render: (record: T) => ReactNode,
  labels: { expand: string; collapse: string },
  options: { rowExpandable?: (record: T) => boolean; rowClick?: boolean } = {},
): NonNullable<TableProps<T>["expandable"]> {
  const { rowExpandable, rowClick = true } = options;
  return {
    expandRowByClick: rowClick,
    columnWidth: 36,
    rowExpandable,
    expandIcon: ({ expanded, onExpand, record, expandable }) =>
      expandable ? (
        <button
          type="button"
          className="detail-expand"
          aria-expanded={expanded}
          aria-label={expanded ? labels.collapse : labels.expand}
          onClick={(event) => {
            // The row's own click must not also fire (it may open the row).
            event.stopPropagation();
            onExpand(record, event);
          }}
        >
          <RightOutlined />
        </button>
      ) : null,
    expandedRowRender: render,
  };
}

type ExtraColumn<T> = {
  key: string;
  title?: ReactNode;
  // Full-width in the expanded row (a path, a list of hints).
  wide?: boolean;
  render?: (value: unknown, record: T, index: number) => ReactNode;
};

// DetailExtra renders a row's secondary columns as its expanded detail: the
// columns a table moved out of its row keep their own titles and renders.
export function DetailExtra<T>({ columns, record }: { columns: ExtraColumn<T>[]; record: T }) {
  return (
    <dl className="detail-extra">
      {columns.map((column) => (
        <div key={column.key} style={column.wide ? { gridColumn: "1 / -1" } : undefined}>
          <dt>{column.title}</dt>
          <dd>{column.render ? column.render(undefined, record, 0) : null}</dd>
        </div>
      ))}
    </dl>
  );
}
