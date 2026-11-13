import type { ThemeConfig } from "antd";
import { zeronTokens } from "./zeron.generated";

// Preview changes take effect on reload; index.html chooses the mode before modules load.
export const darkPreview = typeof document !== "undefined" && document.documentElement.dataset.theme === "dark";
const semantic = zeronTokens[darkPreview ? "dark" : "light"];

/**
 * radius is the corner scale: sm for small parts (tags, checkboxes, menu
 * items, small buttons), md for controls (buttons, inputs, selects,
 * tooltips), lg for containers (cards, dialogs, alerts, the page header).
 * styles.css mirrors it as --radius-sm/md/lg for hand-written styles.
 */
export const radius = { sm: 4, md: 6, lg: 8 };

/** A Zeron #rrggbb colour at the given opacity, for borders antd cannot take as CSS variables. */
function alpha(hex: string, opacity: number) {
  const value = Number.parseInt(hex.slice(1, 7), 16);
  return `rgba(${value >> 16}, ${(value >> 8) & 255}, ${value & 255}, ${opacity})`;
}

export const colors = {
  primary: semantic["brand"],
  primaryHover: semantic["brand-hover"],
  primaryActive: semantic["brand-active"],
  primarySoft: semantic["info-surface"],
  primaryPale: semantic["info-surface-raised"],
  ink: semantic["fg-default"],
  text: semantic["fg-muted"],
  muted: semantic["fg-subtle"],
  weak: semantic["fg-subtle"],
  border: semantic["border"],
  borderSoft: semantic["border-subtle"],
  controlBorder: semantic["input"],
  canvas: semantic["surface-base"],
  white: semantic["surface-floating"],
  success: semantic["success-strong"],
  successSoft: semantic["success-surface"],
  successText: semantic["fg-success"],
  warning: semantic["warning-strong"],
  warningSoft: semantic["warning-surface-subtle"],
  warningText: semantic["fg-warning"],
  danger: semantic["destructive-action"],
  dangerHover: semantic["destructive-action-hover"],
  dangerSoft: semantic["danger-surface-subtle"],
  dangerText: semantic["fg-danger"],
  chartPrimary: semantic["chart-1"],
  chartSecondary: semantic["chart-2"],
  chartTertiary: semantic["chart-3"],
  chartQuaternary: semantic["chart-4"],
  chartTeal: semantic["chart-5"],
  chartViolet: semantic["chart-6"],
  chartSlate: semantic["chart-7"],
  chartGrid: semantic["border-subtle"],
  chartTick: semantic["fg-subtle"],
} as const;

export const themeConfig: ThemeConfig = {
  token: {
    colorPrimary: colors.primary,
    colorInfo: colors.primary,
    colorSuccess: colors.success,
    colorWarning: colors.warning,
    colorError: colors.danger,
    // Light status surfaces come from Zeron directly: antd's own derivation
    // from these strong colours over the translucent borders came out grey-green.
    colorSuccessBg: semantic["success-surface"],
    colorSuccessBorder: alpha(semantic["success-border"], 0.28),
    colorWarningBg: semantic["warning-surface-subtle"],
    colorWarningBorder: alpha(semantic["warning-border"], 0.28),
    colorErrorBg: semantic["danger-surface-subtle"],
    colorErrorBorder: alpha(semantic["danger-border"], 0.28),
    colorInfoBg: semantic["info-surface"],
    colorInfoBorder: alpha(semantic["info-border"], 0.28),
    colorBgLayout: colors.canvas,
    colorBgContainer: colors.white,
    colorBgElevated: semantic["surface-overlay"],
    colorFillSecondary: colors.borderSoft,
    colorFillTertiary: colors.canvas,
    colorFillQuaternary: colors.canvas,
    colorText: colors.text,
    colorTextHeading: colors.ink,
    colorTextSecondary: colors.muted,
    colorTextTertiary: colors.weak,
    colorBorder: colors.border,
    colorBorderSecondary: colors.borderSoft,
    controlOutline: colors.primarySoft,
    borderRadius: radius.md,
    borderRadiusLG: radius.lg,
    borderRadiusSM: radius.sm,
    controlHeight: 32,
    controlHeightLG: 40,
    controlHeightSM: 28,
    fontSize: 14,
    fontSizeSM: 12,
    lineHeight: 1.4286,
    fontFamily: semantic["font-family-sans"],
    boxShadow: semantic["shadow-floating"],
    boxShadowSecondary: semantic["shadow-overlay"],
    motionDurationMid: "0.2s",
  },
  components: {
    Alert: { borderRadiusLG: radius.lg, withDescriptionPadding: "10px 12px" },
    Button: {
      borderRadius: radius.md,
      borderRadiusSM: radius.sm,
      primaryShadow: "none",
      defaultShadow: "none",
      dangerShadow: "none",
      fontWeight: 400,
      paddingInline: 12,
    },
    Card: {
      borderRadiusLG: radius.lg,
      headerHeight: 40,
      headerFontSize: 14,
      headerFontSizeSM: 14,
      bodyPadding: 16,
      bodyPaddingSM: 12,
    },
    Drawer: { borderRadiusLG: radius.lg },
    Form: {
      itemMarginBottom: 16,
      labelColor: colors.text,
      labelFontSize: 14,
      verticalLabelPadding: "0 0 6px",
    },
    Input: {
      colorBorder: colors.controlBorder,
      activeBorderColor: colors.primary,
      hoverBorderColor: colors.primary,
      activeShadow: `0 0 0 2px ${colors.primarySoft}`,
      errorActiveShadow: `0 0 0 2px ${colors.dangerSoft}`,
      paddingInline: 12,
    },
    InputNumber: {
      activeBorderColor: colors.primary,
      hoverBorderColor: colors.primary,
      activeShadow: `0 0 0 2px ${colors.primarySoft}`,
    },
    // The side menu follows the Zeron NavMenu (md): 32px items 4px apart, a
    // soft neutral selection instead of brand blue, hover only darkens text.
    Menu: {
      itemHeight: 32,
      itemBorderRadius: radius.lg,
      itemMarginInline: 8,
      itemMarginBlock: 4,
      iconSize: 16,
      iconMarginInlineEnd: 8,
      itemColor: colors.text,
      itemSelectedBg: colors.borderSoft,
      itemSelectedColor: colors.ink,
      itemActiveBg: colors.borderSoft,
      itemHoverBg: "transparent",
      itemHoverColor: colors.ink,
      groupTitleFontSize: 12,
      groupTitleColor: colors.weak,
    },
    Modal: {
      borderRadiusLG: radius.lg,
      titleFontSize: 16,
      titleLineHeight: 1.5,
      headerBg: colors.white,
      footerBg: colors.white,
      contentBg: colors.white,
    },
    Segmented: {
      itemSelectedBg: colors.primary,
      itemSelectedColor: semantic["fg-on-primary-action"],
      itemHoverBg: colors.borderSoft,
      trackBg: colors.canvas,
      trackPadding: 2,
    },
    Select: {
      optionSelectedBg: colors.primarySoft,
      optionSelectedColor: colors.text,
      optionActiveBg: colors.canvas,
      activeBorderColor: colors.primary,
      hoverBorderColor: colors.primary,
      activeOutlineColor: colors.primarySoft,
    },
    Table: {
      headerBg: colors.white,
      headerColor: colors.muted,
      headerBorderRadius: 0,
      headerSplitColor: "transparent",
      borderColor: colors.border,
      rowHoverBg: colors.canvas,
      cellPaddingBlock: 8,
      cellPaddingInline: 8,
      cellFontSize: 14,
      cellPaddingBlockSM: 6,
      cellPaddingInlineSM: 8,
    },
    Tabs: {
      itemActiveColor: colors.primary,
      itemHoverColor: colors.primary,
      itemSelectedColor: colors.primary,
      inkBarColor: colors.primary,
    },
    Tag: { borderRadiusSM: radius.sm, defaultBg: colors.canvas, defaultColor: colors.text },
    Tooltip: { borderRadius: radius.md },
  },
};

export const tokens = themeConfig.token!;
