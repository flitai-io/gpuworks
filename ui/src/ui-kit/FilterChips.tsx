import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";

export type FilterChip<V extends string> = { value: V; label: ReactNode; count?: number };

/**
 * FilterChips is a row of one-choice filter buttons above a list (model
 * capability, task type). Zeron's small control size: 28px high, 12px text,
 * so the row reads as a filter and not as page actions; the chosen one is
 * the active state, a count follows the label in the subtle colour.
 */
export function FilterChips<V extends string>({ items, value, onChange, label, className }: {
  items: FilterChip<V>[]; value: V | ""; onChange: (value: V) => void; label: string; className?: string;
}) {
  return (
    <div role="group" aria-label={label} className={`flex flex-wrap items-center gap-1.5${className ? ` ${className}` : ""}`}>
      {items.map((item) => {
        const selected = item.value === value;
        return (
          <Button key={item.value} variant="tertiary" size="sm" active={selected} aria-pressed={selected}
            className={selected ? "text-fg-brand" : undefined} onClick={() => onChange(item.value)}>
            {item.label}
            {item.count != null && <span className="ml-0.5 tabular-nums text-fg-subtle">{item.count}</span>}
          </Button>
        );
      })}
    </div>
  );
}
