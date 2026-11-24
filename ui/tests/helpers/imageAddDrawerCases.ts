import { zhImages } from "./imagesHarness.ts";

// Shared by the image add drawer tests (imageAddDrawerCustom, imageAddDrawerCommunity,
// imageAddDrawerEngines): each file keeps its own DOM and cleanup hooks through
// imageAddDrawerHarness.tsx and stays within the per-file runtime budget.

export const c = zhImages.import.community;

export function selectOptions() {
  return [...document.querySelectorAll(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option")]
    .map((node) => node.getAttribute("title") || node.textContent || "");
}
