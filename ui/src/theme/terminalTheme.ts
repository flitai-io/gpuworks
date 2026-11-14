import type { ITerminalOptions } from "@xterm/xterm";
import { zeronTokens } from "./zeron.generated";

// Terminals have a fixed dark surface, independent of the console theme.
// Preserve ANSI hue meanings; use the dark palette instead of xterm's dim defaults.
const dark = zeronTokens.dark;
export const terminalOptions = {
  fontSize: Number.parseFloat(dark["font-size-body"]) * 16,
  fontFamily: '"SFMono-Regular", "JetBrains Mono", Menlo, Consolas, "Courier New", monospace',
  minimumContrastRatio: 4.5,
  theme: {
    background: dark["surface-base"], foreground: dark["fg-default"],
    cursor: dark["fg-default"], selectionBackground: dark["info-surface"],
    black: dark["surface-base"], brightBlack: dark["chart-6"],
    red: dark["chart-7"], brightRed: dark["fg-danger"],
    green: dark["chart-4"], brightGreen: dark["fg-success"],
    yellow: dark["chart-3"], brightYellow: dark["fg-warning"],
    blue: dark["chart-1"], brightBlue: dark["fg-info"],
    magenta: dark["chart-5"], brightMagenta: dark["chart-5"],
    cyan: dark["chart-2"], brightCyan: dark["chart-2"],
    white: dark["fg-default"], brightWhite: dark["fg-default"],
  },
} satisfies ITerminalOptions;

// Log surfaces remain dark in both console themes. Never use chart-category
// colours for platform events or light-theme foregrounds on this surface.
export const logColors = {
  background: dark["surface-base"], foreground: dark["fg-default"],
  muted: dark["fg-subtle"], info: dark["fg-info"],
  warning: dark["fg-warning"], error: dark["fg-danger"],
  eventBackground: dark["surface-overlay"],
};
export const logSurfaceStyle = {
  background: logColors.background, color: logColors.foreground,
  fontSize: "var(--font-size-body)", lineHeight: "var(--line-height-code)",
  fontFamily: terminalOptions.fontFamily,
};
export const logEventStyle = {
  background: logColors.eventBackground, color: logColors.info,
  borderRadius: "var(--radius-md)", padding: "0 4px",
};

// Same category hue order as the charts, with foregrounds suitable for the
// fixed dark log surface even when the surrounding console uses light mode.
export const logReplicaColors = [dark["fg-info"], dark["chart-2"], dark["chart-3"],
  dark["chart-4"], dark["chart-5"], dark["fg-subtle"]] as const;
