export type DateTimeInput = string | number | Date | null | undefined;

const DATE_ONLY = /^\d{4}-\d{2}-\d{2}$/;

function pad(value: number): string {
  return String(value).padStart(2, "0");
}

function parse(value: DateTimeInput): Date | null {
  if (value == null || value === "") return null;
  const date = value instanceof Date ? value : new Date(value);
  return Number.isNaN(date.getTime()) ? null : date;
}

function datePart(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** Formats an instant in the browser timezone using the product-wide layout. */
export function formatTime(value: DateTimeInput, fallback = "—"): string {
  const date = parse(value);
  if (!date) return fallback;
  return `${datePart(date)} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** Time of day for an instant today ("09:46"), else the full formatTime: for "updated at" labels. */
export function formatClock(value: DateTimeInput, fallback = "—", now = new Date()): string {
  const date = parse(value);
  if (!date) return fallback;
  return datePart(date) === datePart(now) ? `${pad(date.getHours())}:${pad(date.getMinutes())}` : formatTime(date, fallback);
}

/** Same as formatTime, with seconds for logs and other high-frequency events. */
export function formatTimeSec(value: DateTimeInput, fallback = "—"): string {
  const date = parse(value);
  if (!date) return fallback;
  return `${formatTime(date, fallback)}:${pad(date.getSeconds())}`;
}

/** Compact month-day + seconds for dense log line prefixes. */
export function formatTimeShort(value: DateTimeInput, fallback = "—"): string {
  const date = parse(value);
  if (!date) return fallback;
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

/** Date-only metadata is not an instant and must not be timezone shifted. */
export function formatDate(value: DateTimeInput, fallback = "—"): string {
  if (typeof value === "string" && DATE_ONLY.test(value)) return value;
  const date = parse(value);
  return date ? datePart(date) : fallback;
}
