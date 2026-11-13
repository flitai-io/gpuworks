import assert from "node:assert/strict";
import test from "node:test";
import { formatClock, formatDate, formatTime, formatTimeShort, formatTimeSec } from "../src/i18n/datetime.ts";

test("date-time helpers use the fixed product format", () => {
  const local = new Date(2026, 7, 25, 4, 3, 2);
  assert.equal(formatTime(local), "2026-08-25 04:03");
  assert.equal(formatTimeSec(local), "2026-08-25 04:03:02");
});

test("date-only metadata is not shifted through Date", () => {
  assert.equal(formatDate("2026-08-25"), "2026-08-25");
  assert.equal(formatTime("not-a-date"), "—");
});

test("formatClock shows only the time of day for an instant today, in the browser's timezone", () => {
  const now = new Date(2026, 10, 11, 18, 30);
  assert.equal(formatClock(new Date(2026, 10, 11, 9, 46, 59), "—", now), "09:46");
  assert.equal(formatClock(new Date(2026, 10, 11, 0, 0).getTime(), "—", now), "00:00", "midnight today is still today");
  assert.equal(formatClock(new Date(2026, 10, 11, 23, 59).toISOString(), "—", now), "23:59", "an ISO string is read as its local instant");
});

test("formatClock shows the full date and time for another day, even at the same clock time", () => {
  const now = new Date(2026, 10, 11, 9, 46);
  assert.equal(formatClock(new Date(2026, 10, 10, 23, 59), "—", now), "2026-11-10 23:59");
  assert.equal(formatClock(new Date(2026, 10, 12, 0, 1), "—", now), "2026-11-12 00:01", "a future day is not today either");
  assert.equal(formatClock(new Date(2025, 10, 11, 9, 46), "—", now), "2025-11-11 09:46", "the same day last year");
});

test("formatClock defaults today to the current date", () => {
  const now = new Date();
  assert.match(formatClock(now), /^\d{2}:\d{2}$/);
});

test("invalid and empty instants read as the fallback in every helper", () => {
  for (const value of [null, undefined, "", "not-a-date", Number.NaN, new Date(Number.NaN)]) {
    assert.equal(formatClock(value), "—");
    assert.equal(formatClock(value, "never"), "never");
    assert.equal(formatTimeShort(value), "—");
    assert.equal(formatTimeSec(value, "n/a"), "n/a");
    assert.equal(formatDate(value, ""), "");
  }
});

test("formatTimeShort is month-day and seconds; formatDate reads a full instant as its local date", () => {
  const local = new Date(2026, 0, 5, 7, 8, 9);
  assert.equal(formatTimeShort(local), "01-05 07:08:09");
  assert.equal(formatDate(local), "2026-01-05");
  assert.equal(formatDate(local.getTime()), "2026-01-05");
});
