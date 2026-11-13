import assert from "node:assert/strict";
import test from "node:test";

import { navGroups, navItemForPath, sectionLabelKey, selectedNavKey } from "../src/navigation.tsx";

const groupKeys = () => navGroups.map((group) => group.labelKey);
const keysIn = (labelKey: string) => navGroups.find((group) => group.labelKey === labelKey)?.items.map((item) => item.key);

// Monitoring and analysis lead, then resources, then serving, then
// administration; new features go to the end of a group, so this order is
// the contract the menu keeps.
test("the overview sits above the groups, which run from monitoring through resources and serving down to administration", () => {
  assert.equal(navGroups[0].labelKey, undefined);
  assert.deepEqual(navGroups[0].items.map((item) => item.key), ["/dashboard"]);
  assert.deepEqual(groupKeys().slice(1), [
    "navigation.groups.observability",
    "navigation.groups.compute",
    "navigation.groups.models",
    "navigation.groups.inference",
    "navigation.groups.admin",
  ]);
  assert.deepEqual(keysIn("navigation.groups.inference"), ["/deployments", "/batches", "/playground/chat", "/evaluations"]);
  assert.deepEqual(keysIn("navigation.groups.models"), ["/models", "/inference-templates", "/images"]);
  assert.deepEqual(keysIn("navigation.groups.compute"), ["/providers", "/kubernetes", "/nodes", "/gpus", "/storage", "/networks"]);
  assert.deepEqual(keysIn("navigation.groups.observability"), ["/monitor", "/scheduling", "/analytics"]);
  assert.deepEqual(keysIn("navigation.groups.admin"), ["/apikeys", "/logs", "/settings"]);
});

// The group leads the menu, so it is capped: further views become tabs.
test("monitoring and analysis holds at most five entries", () => {
  assert.ok((keysIn("navigation.groups.observability") ?? []).length <= 5);
});

test("every entry is listed once", () => {
  const keys = navGroups.flatMap((group) => group.items.map((item) => item.key));
  assert.equal(new Set(keys).size, keys.length);
});

// Folded, only the icons show, so two entries must never share one.
test("no two entries share an icon", () => {
  const icons = navGroups.flatMap((group) => group.items.map((item) => (item.icon as { type: { displayName?: string; render?: { displayName?: string } } }).type));
  assert.equal(new Set(icons).size, icons.length);
});

// Deploying is the inference services list's create action, not a menu entry.
test("the deploy page has no entry of its own: it highlights inference services under that group", () => {
  assert.equal(navGroups.some((group) => group.items.some((item) => item.key === "/deploy")), false);
  assert.equal(selectedNavKey("/deploy"), "/deployments");
  assert.equal(navItemForPath("/deploy")?.item.labelKey, "navigation.deployments");
  // Its breadcrumb names the group, then links back to the list (DeployPage's parents).
  assert.equal(sectionLabelKey("/deploy"), "navigation.groups.inference");
});

test("the overview shows no section crumb; every other page names its group", () => {
  assert.equal(sectionLabelKey("/deployments"), "navigation.groups.inference");
  assert.equal(sectionLabelKey("/dashboard"), undefined);
  assert.equal(sectionLabelKey("/batches"), "navigation.groups.inference");
  assert.equal(sectionLabelKey("/scheduling"), "navigation.groups.observability");
  assert.equal(sectionLabelKey("/logs"), "navigation.groups.admin");
  assert.equal(sectionLabelKey("/providers"), "navigation.groups.compute");
});
