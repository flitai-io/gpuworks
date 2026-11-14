import assert from "node:assert/strict";
import { test } from "node:test";
import { SERIES_COLORS } from "../src/pages/analytics/chartOptions";
import { reasonColor } from "../src/pages/scheduling/activityChart";
import { colors } from "../src/theme/tokens";

// Exercise the consumer mappings: changing the categorical palette must never
// turn a successful request/scheduling outcome into warning yellow or error red.
test("successful and failed outcomes use status colours, not category slots", () => {
  assert.equal(SERIES_COLORS.ok, colors.success);
  assert.equal(SERIES_COLORS.err, colors.danger);
  assert.equal(SERIES_COLORS.interrupted, colors.warning);
  assert.equal(SERIES_COLORS.answer, colors.success);
  assert.equal(reasonColor("scheduled", 0), colors.success);
  assert.equal(reasonColor("failure", 0), colors.danger);
  for (const key of ["placement", "queued"]) {
    assert.notEqual(reasonColor(key, 0), colors.danger);
    assert.notEqual(reasonColor(key, 0), colors.warning);
  }
});
