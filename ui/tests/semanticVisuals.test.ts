import assert from "node:assert/strict";
import { test } from "node:test";
import { terminalOptions, logColors, logEventStyle, logReplicaColors } from "../src/theme/terminalTheme";

function luminance(hex: string) {
  const rgb = hex.slice(1).match(/../g)!.map((v) => Number.parseInt(v, 16) / 255)
    .map((v) => v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4);
  return rgb[0] * 0.2126 + rgb[1] * 0.7152 + rgb[2] * 0.0722;
}
test("terminal purple and foreground remain readable on its fixed dark background", () => {
  const theme = terminalOptions.theme;
  for (const key of ["foreground", "magenta", "brightMagenta"] as const) {
    const contrast = (luminance(theme[key]) + 0.05) / (luminance(theme.background) + 0.05);
    assert.ok(contrast >= 4.5, `${key}: ${contrast}`);
  }
  assert.equal(terminalOptions.minimumContrastRatio, 4.5, "also correct low contrast ANSI/256-colour output dynamically");
  assert.equal(terminalOptions.fontSize, 14);
  assert.equal(Object.keys(theme).length, 20, "explicitly configure all 16 ANSI colours plus background/foreground/cursor/selection");
});

test("platform log events are information blue and all log levels are readable on dark surfaces", () => {
  assert.equal(logEventStyle.color, logColors.info);
  for (const key of ["foreground", "muted", "info", "warning", "error"] as const) {
    for (const background of [logColors.background, logColors.eventBackground]) {
      assert.ok((luminance(logColors[key]) + 0.05) / (luminance(background) + 0.05) >= 4.5, key);
    }
  }
});

test("every replica prefix remains readable on both dark log backgrounds", () => {
  for (const color of logReplicaColors) for (const background of [logColors.background, logColors.eventBackground]) {
    assert.ok((luminance(color) + 0.05) / (luminance(background) + 0.05) >= 4.5, color);
  }
});
