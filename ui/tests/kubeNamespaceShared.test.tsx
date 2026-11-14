import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { kubeEntry } from "./helpers/platformFixtures.ts";
import { freshPopupFramesPerTest } from "./helpers/platformDialog.ts";

// The warning beside a cluster entry's name (ui/src/pages/kubernetes/KubeNamespaceShared.tsx)
// when the server names other entries that use its namespace and may be of the
// same cluster: the one applied later takes over the other's agents. Where it
// sits (a local cluster's row, a cloud cluster's group header) is the page's.

const en = await createI18n(["kubernetes"], "en");
const zh = await createI18n(["kubernetes"], "zh");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: KubeNamespaceShared } = await import("../src/pages/kubernetes/KubeNamespaceShared.tsx");
import type { KubeCluster } from "../src/api";

await freshPopupFramesPerTest();
afterEach(() => { cleanup(); });

function renderBadge(cluster: KubeCluster, language: "en" | "zh" = "en") {
  render(<I18nextProvider i18n={(language === "en" ? en : zh).i18n}><KubeNamespaceShared cluster={cluster} /></I18nextProvider>);
  return userEvent.setup({ document: dom.window.document });
}

test("an entry that shares its namespace with none shows nothing", () => {
  renderBadge(kubeEntry({ namespace_shared_with: [] }));
  assert.equal(document.body.textContent, "");
});

test("an entry the server says shares its namespace carries a badge that names the others and the namespace when pointed at", async () => {
  const k = en.locale.kubernetes;
  const user = renderBadge(kubeEntry({ namespace: "gpu-lab", namespace_shared_with: ["lab", "lab-2"] }));
  const badge = screen.getByText(k.namespaceShared.badge);
  await user.hover(badge);
  assert.equal((await screen.findByRole("tooltip")).textContent, k.namespaceShared.hint.replace("{{namespace}}", "gpu-lab").replace("{{names}}", "lab, lab-2"));
});

// Only that the badge is a tab stop: opening a Base UI tooltip by focus does not settle under jsdom (the test never ends),
// so the explanation itself is read by pointer above; the browser check covers it by keyboard.
test("the badge is a tab stop, so the keyboard reaches it", async () => {
  const k = en.locale.kubernetes;
  renderBadge(kubeEntry({ namespace_shared_with: ["lab"] }));
  const badge = screen.getByText(k.namespaceShared.badge).closest("[tabindex]") as HTMLElement | null;
  assert.equal(badge?.tabIndex, 0);
  assert.equal(badge?.textContent, k.namespaceShared.badge);
});

test("in zh the badge and its explanation read in Chinese", async () => {
  const k = zh.locale.kubernetes;
  const user = renderBadge(kubeEntry({ namespace_shared_with: ["lab"] }), "zh");
  await user.hover(screen.getByText(k.namespaceShared.badge));
  assert.equal((await screen.findByRole("tooltip")).textContent, k.namespaceShared.hint.replace("{{namespace}}", "gpuworks-system").replace("{{names}}", "lab"));
});
