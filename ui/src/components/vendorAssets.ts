// Horizontal accelerator vendor marks shipped under public/vendor-logos, so
// the UI stays self-contained and works offline. Sources (see NOTICE):
// - NVIDIA: NVIDIA's own lockup from NVIDIA/bionemo-framework
//   (docs/docs/assets/images/logo-white.svg, Apache-2.0), in the dark-on-light
//   colours of Wikimedia Commons "File:NVIDIA logo.svg".
// - AMD: Wikimedia Commons "File:AMD Logo.svg" (public domain text logo).
// - Ascend: the header logo of hiascend.com, Huawei's Ascend site.
// - T-Head: the header logo of t-head.cn, T-Head's site.
// - Hygon: its reviewed horizontal mark, converted losslessly from WebP to PNG.
// - Apple publishes no horizontal mark, so ours sets the Simple Icons glyph
//   (simple-icons 16.35.0, CC0-1.0) beside "Apple" outlined from Liberation
//   Sans Bold, in Apple's near-black, to sit with the other wordmarks.
// Every mark is a horizontal wordmark on a transparent canvas: SVG (preferred)
// or PNG with alpha, no padding box or background of its own. Draw a mark only
// with VendorMarkImage (VendorLogo.tsx), which styles it by its ink tone
// (VENDOR_LOGO_INK) for each theme. The Go tests in internal/uiassets
// enforce the canvas, that this map and the directory match, that every mark
// declares an ink tone its artwork measures up to, and that no other file
// draws a mark (docs/dev/ui.md, "Vendor marks").
export const VENDOR_ASSETS: Record<string, string> = {
  nvidia: "/vendor-logos/nvidia.svg",
  amd: "/vendor-logos/amd.svg",
  ascend: "/vendor-logos/ascend.svg",
  thead: "/vendor-logos/thead.png",
  hygon: "/vendor-logos/hygon.png",
  // Official company site: https://www.kunlunxin.com/wp-content/uploads/2023/01/logo-quan.png
  kunlunxin: "/vendor-logos/kunlunxin.png",
  apple: "/vendor-logos/apple.svg",
};

// Original ink tone. Light artwork gets a light-theme filter. Dark artwork
// gets a light plate in the dark theme.
// TestVendorLogoInkReadsOnItsSurfaces measures the artwork against the
// declared tone, so a wrong entry fails the build.
export const VENDOR_LOGO_INK: Record<string, "dark" | "light"> = {
  nvidia: "dark",
  amd: "dark",
  ascend: "dark",
  thead: "dark",
  hygon: "dark",
  kunlunxin: "light",
  apple: "dark",
};

// The height each mark is shown at when marks sit side by side. One box does
// not make them look the same size: a bold full-height wordmark (Hygon) reads
// larger than a mark with a symbol (Ascend) at the same height, so each gets
// its own height inside an 80 × 20 box.
export const VENDOR_LOGO_HEIGHT: Record<string, number> = {
  nvidia: 15,
  ascend: 20,
  hygon: 11,
  amd: 12,
  apple: 16,
  thead: 16,
};
