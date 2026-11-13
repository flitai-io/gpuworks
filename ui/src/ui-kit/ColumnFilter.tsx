import { useState } from "react";
import { Input } from "@/components/ui/input";
import type { Column } from "@tanstack/react-table";
import { Button } from "@/components/ui/button";
import { CheckboxGroup, CheckboxItem } from "@/components/ui/checkbox-group";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ConsoleIcon } from "./consoleIcons";

export type ColumnFilterOption = { value: string; label: string };

/**
 * ColumnFilter is the funnel beside a filterable column's title, the one place
 * a list's column filters are set; ResourceTable lists the ones in effect above
 * the table. The funnel turns brand-blue while a filter is applied. Zeron has
 * no header filter of its own.
 */
export function ColumnFilter<T>({ column, title, options, multiple = true, clearLabel, search }: {
  column: Column<T, unknown>; title: string; options: ColumnFilterOption[]; multiple?: boolean; clearLabel: string;
  /** Placeholder of a search box over the options, for long lists (node names, models). */
  search?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const shown = query ? options.filter((option) => option.label.toLowerCase().includes(query.trim().toLowerCase())) : options;
  const value = column.getFilterValue();
  const selected = new Set(Array.isArray(value) ? value.map(String) : []);
  const active = selected.size > 0;
  const toggle = (option: string) => {
    if (!multiple) {
      column.setFilterValue(selected.has(option) ? undefined : [option]);
      setOpen(false);
      return;
    }
    const next = new Set(selected);
    if (next.has(option)) next.delete(option); else next.add(option);
    column.setFilterValue(next.size ? [...next] : undefined);
  };
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger render={(
        <Button variant="ghost" size="xs" iconOnly active={open} aria-label={title} aria-pressed={active}
          className={`shrink-0 ${active ? "text-fg-brand" : "text-fg-subtle"}`}>
          <ConsoleIcon.filter />
        </Button>
      )} />
      <PopoverContent align="start" className="w-60 p-1" sideOffset={4}>
        <div className="px-2 py-1.5 text-label text-fg-muted">{title}</div>
        {search && <div className="px-1 pb-1"><Input size="sm" value={query} placeholder={search} aria-label={search} onChange={(event) => setQuery(event.target.value)} /></div>}
        {/* Long option lists scroll inside the popover instead of running off the screen. */}
        <div className="max-h-72 overflow-y-auto">
          <CheckboxGroup aria-label={title} checkedIndices={new Set(shown.flatMap((option, index) => selected.has(option.value) ? [index] : []))} className="w-full">
            {shown.map((option, index) => (
              <CheckboxItem key={option.value} index={index} checked={selected.has(option.value)} label={option.label} onToggle={() => toggle(option.value)} />
            ))}
          </CheckboxGroup>
        </div>
        {active && (
          <Button className="mt-0.5 w-full border-t border-border" size="md" variant="ghost" onClick={() => { column.setFilterValue(undefined); setOpen(false); }}>
            {clearLabel}
          </Button>
        )}
      </PopoverContent>
    </Popover>
  );
}
