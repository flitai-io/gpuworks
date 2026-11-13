// Horizontal accelerator vendor marks shipped under public/vendor-logos, so
// the UI stays self-contained and works offline. Sources (see NOTICE):
// - NVIDIA: NVIDIA's own lockup from NVIDIA/bionemo-framework
//   (docs/docs/assets/images/logo-white.svg, Apache-2.0), in the dark-on-light
//   colours of Wikimedia Commons "File:NVIDIA logo.svg".
// - AMD: Wikimedia Commons "File:AMD Logo.svg" (public domain text logo).
// - Ascend: the header logo of hiascend.com, Huawei's Ascend site.
// - T-Head: the header logo of t-head.cn, T-Head's site.
// - Hygon: its reviewed horizontal WebP.
// - Apple publishes no horizontal mark, so ours sets the Simple Icons glyph
//   (simple-icons 16.35.0, CC0-1.0) beside "Apple" outlined from Liberation
//   Sans Bold, in Apple's near-black, to sit with the other wordmarks.
export const VENDOR_ASSETS: Record<string, string> = {
  nvidia: "/vendor-logos/nvidia.svg",
  amd: "/vendor-logos/amd.svg",
  ascend: "/vendor-logos/ascend.svg",
  thead: "/vendor-logos/thead.png",
  hygon: "/vendor-logos/hygon.webp",
  // Official company site: https://www.kunlunxin.com/wp-content/uploads/2023/01/logo-quan.png
  kunlunxin: "/vendor-logos/kunlunxin.png",
  apple: "/vendor-logos/apple.svg",
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
