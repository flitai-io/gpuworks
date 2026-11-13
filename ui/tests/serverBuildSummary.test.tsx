import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import type { ServerBuildInfo } from "../src/serverBuildInfo";
import { formatTime } from "../src/i18n/datetime.ts";

const { i18n, locale } = await createI18n(["nodes", "settings"]);
const k = locale.common.buildInfo;
const { cleanup, render, screen, within } = await import("@testing-library/react");
const { default: ServerBuildSummary } = await import("../src/components/ServerBuildSummary.tsx");
const { default: UpgradePreviewModal } = await import("../src/components/agentUpgrades/UpgradePreviewModal.tsx");
afterEach(() => cleanup());
after(() => dom.window.close());
function mount(build?: ServerBuildInfo, version = "target-build") {
  return render(<I18nextProvider i18n={i18n}><ServerBuildSummary version={version} build={build} /></I18nextProvider>);
}
for (const kind of ["stable", "prerelease", "development", "unknown"] as const) {
  test(`shows the server-provided ${kind} classification and explanation`, () => {
    const version = kind === "stable" ? "v1.2.3" : kind === "prerelease" ? "v1.2.3-rc.1" : "target-build";
    mount({ version, kind, ...(["stable", "prerelease"].includes(kind) ? { published_version: version } : {}) }, version);
    assert.ok(screen.getByText(k.kind[kind]));
    assert.ok(screen.getByText(k.description[kind]));
    assert.ok(screen.getByText(version));
  });
}
test("missing or mismatched metadata cannot label the target stable", () => {
  const view = mount({ version: "v99.0.0", kind: "stable", published_version: "v99.0.0", published_at: "2099-01-01T00:00:00Z" });
  assert.ok(screen.getByText(k.kind.unknown));
  assert.ok(screen.getByText(k.unmatched));
  assert.equal(screen.queryByText(k.kind.stable), null);
  assert.equal(screen.queryByText(/v99\.0\.0/), null);
  view.unmount(); mount();
  assert.ok(screen.getByText(k.kind.unknown));
});
test("matching records separate build and publication time and put commit in details", () => {
  const built = "2026-10-30T01:00:00Z", published = "2026-10-31T02:00:00Z";
  const commit = "0123456789abcdef0123456789abcdef01234567";
  mount({ version: "v1.2.3", kind: "stable", commit, build_channel: "release", built_at: built, published_version: "v1.2.3", published_at: published }, "v1.2.3");
  assert.ok(screen.getByText(i18n.t("common:buildInfo.builtAt", { time: formatTime(built) })));
  assert.ok(screen.getByText(/Matching release: v1\.2\.3/));
  assert.ok(screen.getByText(new RegExp(formatTime(published))));
  const summary = screen.getByText(k.details);
  assert.equal(summary.tagName, "SUMMARY");
  assert.ok(summary.closest("details")?.contains(screen.getByText(new RegExp(commit))));
  assert.ok(screen.getByText(i18n.t("common:buildInfo.channel", { channel: "release" })));
});
test("upgrade confirmation explains its target and links settings before consent", () => {
  render(<I18nextProvider i18n={i18n}><UpgradePreviewModal open loading={false} busy={false} error=""
    preview={{ target_version: "target-build", target_build: { version: "target-build", kind: "development", build_channel: "nightly" }, manifest_fingerprint: "sealed", node_uuids: ["node-a"], nodes: [{ node_uuid: "node-a", name: "Node A", from_version: "old-build", os: "linux", arch: "amd64", transport: "push", eligibility: "eligible", version_change: "different_build" }], counts: { eligible: 1 } }}
    onClose={() => {}} onRefresh={() => {}} onStart={() => {}} /></I18nextProvider>);
  const modal = screen.getByRole("dialog");
  assert.ok(within(modal).getByText(k.kind.development));
  assert.ok(within(modal).getByText("old-build → target-build"));
  assert.ok(within(modal).getByText(locale.nodes.fleet.replaceBuild));
  assert.ok(within(modal).getByText(locale.nodes.batchUpgrade.targetSource));
  assert.ok(within(modal).getByText(locale.nodes.batchUpgrade.versionWarning));
  assert.equal(within(modal).getByRole("link", { name: locale.nodes.batchUpgrade.versionDetails }).getAttribute("href"), "/settings#updates");
  assert.equal((within(modal).getByRole("button", { name: locale.nodes.batchUpgrade.start }) as HTMLButtonElement).disabled, true);
  assert.equal(within(modal).queryByText(/Different commit versions/), null);
});
