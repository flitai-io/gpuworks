import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { nodes, evaluation } from "./helpers/refusedLabels.ts";
const { i18n } = await createI18n(["deploy", "nodes"], "en");
const { cleanup, render, screen, fireEvent } = await import("@testing-library/react");
const { Form } = await import("antd");
const NodeSection = (await import("../src/pages/deploy/DeployNodeSection.tsx")).default;
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });
after(() => dom.window.close());
async function renderCard(result = evaluation, loading = false) {
  const fake = installFleet(api as never, nodes as never);
  const fleet = await fake.nodeFacets({ state: ["online"] });
  function Harness() {
    const [form] = Form.useForm();
    return <Form form={form} initialValues={{ node_selection_mode: "auto", target_vendor: "nvidia", node_labels: { runtime_type: "sandbox" } }}>
      <NodeSection fleet={fleet} knownNodes={new Map()} eligibleNodes={result.eligible_nodes} selection={undefined as never}
        modelReady result={result} loading={loading} onNodeMode={() => {}} />
    </Form>;
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
}
test("label-matched refusal leads the cluster list, explains the zero stage, and preserves every refusal", async () => {
  await renderCard();
  const list = document.querySelector(".deploy-node-refusals")!;
  assert.match(list.querySelector("li")!.textContent!, /^selected-turing:/);
  assert.match(list.textContent!, /7\.5/);
  assert.match(list.textContent!, />=8\.0/);
  assert.doesNotMatch(list.textContent!, /\{"vendor"/);
  assert.ok(screen.getByText(/Cluster: 14 → Node conditions: 1 → Hardware and scheduling conditions: 0/));
  assert.ok(screen.getByText(/Node conditions matched; hardware requirements rejected/));
  const more = screen.getByRole("button", { name: /13 other rejected nodes/ });
  fireEvent.click(more);
  assert.equal(document.querySelectorAll(".deploy-node-refusals li").length, 14);
  for (let i = 0; i < 13; i++) assert.ok(screen.getByText(`other-${i}`));
});

test("in-flight and legacy evaluations do not assert node-condition success", async () => {
  await renderCard(evaluation, true);
  assert.equal(screen.queryByText(/Node conditions matched/), null);
  assert.equal(document.querySelector(".deploy-node-refusals"), null);
  assert.ok(screen.getByText("Evaluating the current node conditions…"));
  cleanup();
  const legacy = structuredClone(evaluation);
  legacy.eligible_nodes.forEach((node) => { delete (node as Partial<typeof node>).requirements_match; });
  await renderCard(legacy);
  assert.equal(screen.queryByText(/Node conditions matched/), null);
  assert.equal(screen.queryByText(/Cluster: 14 →/), null);
});

test("zero matches and mixed subsequent failures never claim the labels passed hardware", async () => {
  const zero = structuredClone(evaluation);
  zero.eligible_nodes.forEach((node) => { node.requirements_match = false; });
  await renderCard(zero);
  assert.ok(screen.getByText(/No nodes matched the node conditions/));
  assert.ok(screen.getByText(/Cluster: 14 → Node conditions: 0 → Hardware and scheduling conditions: 0/));
  cleanup();
  const mixed = structuredClone(evaluation);
  mixed.eligible_nodes[13].reasons.push({ code: "node_offline" } as never);
  await renderCard(mixed);
  assert.equal(screen.queryByText(/hardware requirements rejected/), null);
  assert.ok(screen.getByText(/The evaluated nodes failed subsequent checks or resource planning/));
  assert.match(document.querySelector(".deploy-node-refusals li")!.textContent!, /offline/i);
});

test("all condition matches remain expandable beyond the first five", async () => {
  const all = structuredClone(evaluation);
  all.eligible_nodes.forEach((node) => { node.requirements_match = true; });
  await renderCard(all);
  assert.ok(screen.getByText(/Cluster: 14 → Node conditions: 14 → Hardware and scheduling conditions: 0/));
  assert.equal(document.querySelectorAll(".deploy-node-refusals li").length, 5);
  fireEvent.click(screen.getByRole("button", { name: /and 9 more nodes turned down/ }));
  assert.equal(document.querySelectorAll(".deploy-node-refusals li").length, 14);
});

test("server GPU facts retain unknown, mixed-card and non-CC predicate differences", async () => {
  const { placementRefusalText } = await import("../src/pages/deploy/deploySchedule.ts");
  const t = i18n.getFixedT("en", "deploy");
  const text = placementRefusalText(t, { code: "chip_mismatch", params: {
    required_vendor: "nvidia", required_cc: ">=8.0", required_name: "A100|H100",
    actual_gpus: [{ name: "RTX 2080 Ti", vendor: "nvidia", cc: "7.5" }, { name: "Unknown card", vendor: "nvidia", cc: "" }],
  } });
  assert.match(text, /CC 7\.5/);
  assert.match(text, /CC unknown/);
  assert.match(text, /name pattern A100\|H100/);
  const noCC = placementRefusalText(t, { code: "chip_mismatch", params: {
    required_vendor: "ascend", required_name: "910B", actual_gpus: [{ name: "310P", vendor: "ascend", cc: "" }],
  } });
  assert.match(noCC, /910B/); assert.match(noCC, /310P/); assert.doesNotMatch(noCC, /CC/);
  assert.match(placementRefusalText(t, { code: "vendor_mismatch", params: { vendor: "nvidia", actual_gpus: [] } }), /no reported GPUs/);
});

test("resource planning failures show their zero without counting untried nodes as rejected", async () => {
  const noFit = structuredClone(evaluation);
  noFit.eligible_nodes[13].eligible = true;
  noFit.eligible_nodes[13].reasons = [];
  noFit.candidates[13].reason = { code: "no_fit", params: { gpus: 1, tp: 1, pp: 1, gpu_count: 1, required_gib: 20, available_gib: 11 } } as never;
  await renderCard(noFit);
  // The node funnel and the planning funnel are one line of facts.
  assert.ok(screen.getByText(/^Cluster: 14 → .* → Hardware and scheduling conditions: 1 · Resource planning: 0 passed \/ 1 evaluated; 0 not evaluated$/));
  cleanup();
  const budget = structuredClone(evaluation);
  budget.eligible_nodes.forEach((node) => { node.eligible = true; node.requirements_match = true; node.reasons = []; });
  budget.candidates = budget.candidates.slice(0, 8).map((candidate) => ({ ...candidate, reason: { code: "no_fit", params: { gpus: 1, tp: 1, pp: 1, gpu_count: 1, required_gib: 20, available_gib: 11 } } as never }));
  await renderCard(budget);
  assert.ok(screen.getByText(/ · Resource planning: 0 passed \/ 8 evaluated; 6 not evaluated$/));
  assert.equal(screen.queryByText("selected-turing", { selector: ".deploy-node-refusals strong" }), null);
  const { nodeConditionSummary } = await import("../src/pages/deploy/deployPlacementRefusal.ts");
  budget.candidates = [{ ...budget.candidates[0], ok: true }];
  const stopped = nodeConditionSummary(budget)!;
  assert.deepEqual([stopped.passed, stopped.tried, stopped.untried], [1, 1, 13]);
});
