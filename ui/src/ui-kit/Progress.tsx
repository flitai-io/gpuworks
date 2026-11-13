export function Progress({ value, max = 100, label }: { value: number; max?: number; label: string }) {
  const limit = max > 0 ? max : 100;
  const bounded = Math.min(limit, Math.max(0, Number.isFinite(value) ? value : 0));
  return <div role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={limit} aria-valuenow={bounded}
    className="h-2 w-full overflow-hidden rounded-full bg-emphasis">
    <div className="h-full rounded-full bg-brand transition-[width] duration-moderate motion-reduce:transition-none" style={{ width: `${bounded / limit * 100}%` }} />
  </div>;
}
