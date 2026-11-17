import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "../../components/ui/button";
import { TableBody, TableCell, TableRow } from "../../components/ui/table";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";

/**
 * One group of a grouped table: a header row that names the group and folds
 * it, then the group's rows. The group is its own row group, named after it.
 */
export function TableGroup({ name, colSpan, open, onToggle, title, extra, children, ...attributes }: {
  /** The group's accessible name, and what its fold button says it folds. */
  name: string;
  colSpan: number;
  open: boolean;
  onToggle: () => void;
  /** After the fold button: the group's name and what it is. */
  title: ReactNode;
  /** At the far end of the header: where the group stands, and what can be done with it. */
  extra?: ReactNode;
  children?: ReactNode;
  "data-cluster-entry"?: string;
}) {
  const { t } = useTranslation("providers");
  return (
    // Every group but the last keeps the rule under its last row.
    <TableBody aria-label={name} className="[&>tr:last-child]:border-b last:[&>tr:last-child]:border-b-0" {...attributes}>
      <TableRow className="bg-surface-base">
        <TableCell colSpan={colSpan} className="py-1.5">
          <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
            <div className="flex min-w-0 flex-1 items-center gap-2">
              <Button variant="ghost" size="xs" iconOnly aria-expanded={open} aria-label={t(open ? "detail.collapse" : "detail.expand", { name })} onClick={onToggle}>
                {open ? <ConsoleIcon.chevronDown size={16} /> : <ConsoleIcon.chevronRight size={16} />}
              </Button>
              {title}
            </div>
            {extra && <div className="flex flex-none flex-wrap items-center gap-x-3 gap-y-1">{extra}</div>}
          </div>
        </TableCell>
      </TableRow>
      {open && children}
    </TableBody>
  );
}

/** A row that spans the table: a note, a loading mark, or a group's own controls. */
export function SpanRow({ colSpan, children, className = "" }: { colSpan: number; children: ReactNode; className?: string }) {
  return <TableRow><TableCell colSpan={colSpan} className={className}>{children}</TableCell></TableRow>;
}
