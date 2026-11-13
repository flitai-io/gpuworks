import type { ReactNode } from "react";
export function Timeline({ items, label }: { label: string; items: { key: string; title: ReactNode; description?: ReactNode; time?: string }[] }) {
  return <ol aria-label={label} className="m-0 list-none space-y-4 border-l border-border pl-4">
    {items.map((item) => <li key={item.key} className="relative">
      <span aria-hidden="true" className="absolute -left-5 top-1.5 size-2 rounded-full bg-brand" />
      <div className="text-fg-default">{item.title}</div>
      {item.time && <time className="text-label text-fg-muted">{item.time}</time>}
      {item.description && <div className="text-fg-muted">{item.description}</div>}
    </li>)}
  </ol>;
}
