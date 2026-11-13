import type { ReactNode } from "react";
/** Read-only batch metadata; no form or query behavior. */
export function BatchFacts({ title, action, items }: { title?: ReactNode; action?: ReactNode; items: { key: string; label: ReactNode; value: ReactNode }[] }) {
  return <div className="min-w-0">
    {(title || action) && <div className="mb-3 flex flex-wrap items-center justify-between gap-2"><h3 className="text-body font-semibold">{title}</h3>{action}</div>}
    <dl className="divide-y divide-border rounded-lg border border-border">
      {items.map(item => <div key={item.key} className="grid grid-cols-[minmax(100px,1fr)_minmax(0,2fr)] gap-3 px-3 py-2 text-body"><dt className="text-fg-muted">{item.label}</dt><dd className="min-w-0 break-words">{item.value}</dd></div>)}
    </dl>
  </div>;
}
