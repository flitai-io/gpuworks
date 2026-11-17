import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["nodes", "common"], "en");
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const apiModule = await import("../src/api.ts");
const NodePlacementPolicy = (await import("../src/components/NodePlacementPolicy.tsx")).default;

afterEach(() => cleanup());

type Node = import("../src/api.ts").Node;
type Setting = import("../src/api.ts").NodePlacementPolicySetting;
const text = locale.nodes.placementPolicy as Record<string, any>;

const dictionary: Setting[] = [{ key: "share_with_foreign", kind: "bool", default: false }];
const node = (over: Partial<Node> = {}): Node => ({
  uuid: "node-1",
  placement_policy: { share_with_foreign: false },
  placement_policy_availability: [{ key: "share_with_foreign", applicable: true }],
  ...over,
}) as Node;

function stub(dict: () => Promise<Setting[]>, save?: (uuid: string, patch: Record<string, unknown>) => Promise<Node>) {
  const original = { dict: apiModule.api.nodePlacementPolicyDictionary, save: apiModule.api.setNodePlacementPolicy };
  apiModule.api.nodePlacementPolicyDictionary = dict as never;
  if (save) apiModule.api.setNodePlacementPolicy = save as never;
  return () => {
    apiModule.api.nodePlacementPolicyDictionary = original.dict;
    apiModule.api.setNodePlacementPolicy = original.save;
  };
}

const show = (value: Node, onUpdated: (n: Node) => void = () => {}) =>
  render(<I18nextProvider i18n={i18n}><NodePlacementPolicy node={value} onUpdated={onUpdated} /></I18nextProvider>);

test("each knob of the dictionary renders by its kind with its label and help", async () => {
  const restore = stub(async () => dictionary);
  try {
    show(node());
    assert.ok(screen.getByTestId("placement-policy-loading"), "a loading placeholder shows first");
    const toggle = await screen.findByRole("switch", { name: text.share_with_foreign.label });
    assert.equal(toggle.getAttribute("aria-checked"), "false");
    assert.ok(screen.getByText(text.share_with_foreign.help));
    assert.equal(screen.queryByText(/No effect on this node/), null);
  } finally {
    restore();
  }
});

test("switching a knob from the keyboard saves it by key and hands the node back", async () => {
  const saved: Array<Record<string, unknown>> = [];
  let updated: Node | undefined;
  const restore = stub(async () => dictionary, async (uuid, patch) => {
    assert.equal(uuid, "node-1");
    saved.push(patch);
    return node({ placement_policy: { share_with_foreign: true } });
  });
  try {
    show(node(), (n) => { updated = n; });
    const toggle = await screen.findByRole("switch", { name: text.share_with_foreign.label });
    toggle.focus();
    await userEvent.setup({ document: dom.window.document }).keyboard(" ");
    await waitFor(() => assert.deepEqual(saved, [{ share_with_foreign: true }]));
    await waitFor(() => assert.equal(updated?.placement_policy?.share_with_foreign, true));
    assert.equal(toggle.getAttribute("aria-checked"), "true");
  } finally {
    restore();
  }
});

test("a failed save puts the switch back and says why", async () => {
  const restore = stub(async () => dictionary, async () => { throw new Error("node gone"); });
  try {
    show(node());
    const toggle = await screen.findByRole("switch", { name: text.share_with_foreign.label });
    fireEvent.click(toggle);
    await screen.findByText(text.saveFailed.replace("{{detail}}", "node gone"));
    await waitFor(() => assert.equal(toggle.getAttribute("aria-checked"), "false"));
  } finally {
    restore();
  }
});

test("a knob with no effect on the node says why and can still be switched", async () => {
  const saved: Array<Record<string, unknown>> = [];
  const restore = stub(async () => dictionary, async (_uuid, patch) => { saved.push(patch); return node(); });
  try {
    show(node({ placement_policy_availability: [{ key: "share_with_foreign", applicable: false, reason: "NODE_PLACEMENT_POLICY_AGENT_TOO_OLD" }] }));
    const toggle = await screen.findByRole("switch", { name: text.share_with_foreign.label });
    assert.ok(screen.getByText(text.notApplicable.replace("{{reason}}", text.reason.NODE_PLACEMENT_POLICY_AGENT_TOO_OLD)));
    assert.equal(toggle.hasAttribute("disabled"), false);
    fireEvent.click(toggle);
    await waitFor(() => assert.deepEqual(saved, [{ share_with_foreign: true }]));
  } finally {
    restore();
  }
});

test("an empty dictionary and a failed load each say so", async () => {
  let restore = stub(async () => []);
  try {
    show(node());
    await screen.findByText(text.empty);
  } finally {
    restore();
  }
  cleanup();
  restore = stub(async () => { throw new Error("server down"); });
  try {
    show(node());
    await screen.findByText(text.loadFailed);
    assert.ok(screen.getByText("server down"));
  } finally {
    restore();
  }
});
