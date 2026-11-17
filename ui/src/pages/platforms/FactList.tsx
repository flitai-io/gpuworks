import type { ReactNode } from "react";

/**
 * What an object is, laid out flat as "label value" pairs: two columns where
 * the space allows, one where it does not. Labels are muted and of one width,
 * values in body text; the space is the nearest size container.
 */
export default function FactList({ facts }: { facts: Array<[string, ReactNode]> }) {
  return (
    <dl className="m-0 grid max-w-[880px] grid-cols-1 gap-x-12 gap-y-2 text-body @[640px]:grid-cols-2">
      {facts.map(([name, value]) => (
        <div key={name} className="flex min-w-0 items-baseline gap-3">
          <dt className="w-28 flex-none text-fg-muted">{name}</dt>
          <dd className="m-0 min-w-0 break-words text-fg-default">{value}</dd>
        </div>
      ))}
    </dl>
  );
}
