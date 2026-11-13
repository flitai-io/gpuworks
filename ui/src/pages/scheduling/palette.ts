import { darkPreview } from "../../theme/tokens";
import { zeronTokens } from "../../theme/zeron.generated";

const dark = zeronTokens.dark;

// Scheduling states keep the established heatmap colours across component themes.
export const schedulingColors = darkPreview ? {
  serving: dark["chart-1"], sleeping: dark["chart-5"], starting: dark["chart-2"],
  external: dark["chart-3"], free: dark["success-surface"], freeBorder: dark["chart-4"],
  failed: dark["chart-7"], offline: dark["chart-6"], switching: dark["fg-default"],
} : {
  serving: "#0076f7", sleeping: "#9a45e4", starting: "#01c8e6",
  external: "#ff9000", free: "#ecfedf", freeBorder: "#57d344",
  failed: "#f93940", offline: "#c8cacd", switching: "#181a1d",
};

export function deploymentColor(id: number) {
  return `hsl(${(id * 137.508) % 360} 61% 42%)`;
}
