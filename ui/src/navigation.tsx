import { createElement } from "react";
import { ConsoleIcon } from "./ui-kit/consoleIcons";
import type { ReactNode } from "react";

export interface NavItem {
  key: string;
  icon: ReactNode;
  labelKey: string;
  /** Other first path segments filed under this entry, e.g. the create page of a list. */
  also?: string[];
}

export interface NavGroup {
  /** Absent for the entries above the first group (the overview). */
  labelKey?: string;
  items: NavItem[];
}

// The side menu, also read by PageHeader so a page's breadcrumb names the
// same section the menu files it under. Monitoring and analysis come first,
// then the resources work builds on (compute, models), then serving, and
// administration last, where the log search sits with the API keys. New
// features go to the end of their group so existing entries keep their
// place; monitoring and analysis holds at most five entries (alerts would
// be the fourth), and further views become tabs.
// Menu icons: the HugeIcons stroke set Zeron uses, 16px, thin stroke (ui-guidelines 9.2).
const icon = (Icon: (typeof ConsoleIcon)[keyof typeof ConsoleIcon]) => createElement(Icon, { size: 16, strokeWidth: 1.5 });

export const navGroups: NavGroup[] = [
  { items: [
    { key: "/dashboard", icon: icon(ConsoleIcon.dashboard), labelKey: "navigation.dashboard" },
  ] },
  { labelKey: "navigation.groups.observability", items: [
    { key: "/monitor", icon: icon(ConsoleIcon.monitor), labelKey: "navigation.monitor" },
    { key: "/scheduling", icon: icon(ConsoleIcon.scheduling), labelKey: "navigation.scheduling" },
    { key: "/analytics", icon: icon(ConsoleIcon.analytics), labelKey: "navigation.analytics" },
  ] },
  { labelKey: "navigation.groups.compute", items: [
    // External platforms provide nodes, so they sit above them. Public and
    // private clouds share one list; /zstack redirects to it.
    // Kubernetes clusters provide nodes too.
    { key: "/providers", icon: icon(ConsoleIcon.platforms), labelKey: "navigation.platforms" },
    { key: "/kubernetes", icon: icon(ConsoleIcon.kubernetes), labelKey: "navigation.kubernetes" },
    { key: "/nodes", icon: icon(ConsoleIcon.nodes), labelKey: "navigation.nodes" },
    { key: "/gpus", icon: icon(ConsoleIcon.gpus), labelKey: "navigation.gpus" },
    { key: "/storage", icon: icon(ConsoleIcon.storage), labelKey: "navigation.storage" },
    { key: "/networks", icon: icon(ConsoleIcon.networks), labelKey: "navigation.networks" },
  ] },
  { labelKey: "navigation.groups.models", items: [
    { key: "/models", icon: icon(ConsoleIcon.models), labelKey: "navigation.models" },
    { key: "/inference-templates", icon: icon(ConsoleIcon.inferenceTemplates), labelKey: "navigation.inferenceTemplates" },
    { key: "/images", icon: icon(ConsoleIcon.images), labelKey: "navigation.images" },
  ] },
  { labelKey: "navigation.groups.inference", items: [
    // Deploying is the list's create action, so /deploy highlights the list.
    { key: "/deployments", icon: icon(ConsoleIcon.deployments), labelKey: "navigation.deployments", also: ["deploy"] },
    { key: "/batches", icon: icon(ConsoleIcon.batches), labelKey: "navigation.batches" },
    { key: "/playground/chat", icon: icon(ConsoleIcon.playground), labelKey: "navigation.playground" },
    { key: "/evaluations", icon: icon(ConsoleIcon.evaluations), labelKey: "navigation.evaluations" },
  ] },
  { labelKey: "navigation.groups.admin", items: [
    { key: "/apikeys", icon: icon(ConsoleIcon.apiKeys), labelKey: "navigation.apiKeys" },
    { key: "/logs", icon: icon(ConsoleIcon.logs), labelKey: "navigation.logs" },
    { key: "/settings", icon: icon(ConsoleIcon.settings), labelKey: "navigation.settings" },
  ] },
];

function pathSection(pathname: string) {
  return pathname.split("/")[1] || "";
}

/** navItemForPath returns the menu entry a path belongs to, by its first segment. */
export function navItemForPath(pathname: string) {
  const section = pathSection(pathname);
  for (const group of navGroups) {
    const item = group.items.find((entry) => entry.key.split("/")[1] === section || entry.also?.includes(section));
    if (item) return { group, item };
  }
  return undefined;
}

/** sectionLabelKey is the breadcrumb's section: the entry's group, absent for the overview, which sits above the groups. */
export function sectionLabelKey(pathname: string) {
  return navItemForPath(pathname)?.group.labelKey;
}

/**
 * selectedNavKey is the menu item key for a path. A nested entry such as
 * /playground/chat still matches /playground/audio, so the item highlights.
 */
export function selectedNavKey(pathname: string) {
  return navItemForPath(pathname)?.item.key ?? `/${pathSection(pathname) || "dashboard"}`;
}
