import { colors } from "../../theme/tokens";

export const schedulingColors = {
  serving: colors.primary, sleeping: colors.chartTeal, starting: colors.chartSecondary,
  external: colors.muted, free: colors.successSoft, freeBorder: colors.success,
  failed: colors.danger, offline: colors.border, switching: colors.ink,
};

export function deploymentColor(id: number) {
  return `hsl(${(id * 137.508) % 360} 61% 42%)`;
}
