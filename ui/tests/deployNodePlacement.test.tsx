import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The deploy form's node section separates the replica's shape (single node,
// multi-node, PD) from where it runs: for a single-node replica, automatic
// candidates or one pinned node, the hardware and network the candidates must
// have (offered from the system labels the server generated, with node
// counts), and the spread/binpack strategy. The payload states all of it as
// node requirements.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const deployLocale = read("deploy");
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { deploy: deployLocale, nodes: read("nodes"), common: read("common") } }, defaultNS: "deploy", interpolation: { escapeValue: false } });

const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const NodeSection = (await import("../src/pages/deploy/DeployNodeSection.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const apiOriginal = { ...api };
const { systemConditionsToRequirements } = await import("../src/components/SystemLabelConditions.tsx");

afterEach(() => { cleanup(); Object.assign(api, apiOriginal); });
after(() => { closeChannels(); dom.window.close(); });
const node = deployLocale.workbench.node;

const label = (key: string, value: string, display = value) => ({ key: `gpuworks.ai/${key}`, value, display });
const nodes = [
  { uuid: "a", name: "a100-1", state: "online", gpu_count: 8, gpus: [{ index: 0, name: "A100" }], system_labels: [label("node", "a", "a100-1"), label("vendor", "nvidia", "NVIDIA"), label("gpu-model", "A100"), label("compute-network", "7", "ib-east")] },
  { uuid: "b", name: "a100-2", state: "online", gpu_count: 8, gpus: [{ index: 0, name: "A100" }], system_labels: [label("node", "b", "a100-2"), label("vendor", "nvidia", "NVIDIA"), label("gpu-model", "A100"), label("compute-network", "8", "ib-west")] },
  { uuid: "c", name: "h100-1", state: "online", gpu_count: 8, gpus: [{ index: 0, name: "H100" }], system_labels: [label("node", "c", "h100-1"), label("vendor", "nvidia", "NVIDIA"), label("gpu-model", "H100")] },
] as never[];

// renderSection mounts the node block the way the deploy page does: the
// online nodes' facets in hand, the node endpoints answered by a faithful
// fake of the server.
async function renderSection(values: Record<string, unknown>, onNodeMode: (mode: string) => void = () => {}, eligible?: Array<{ uuid: string; eligible: boolean }>, profile?: import("../src/api.ts").EngineProfile) {
  const fake = installFleet(api as never, nodes as never);
  const fleet = await fake.nodeFacets({ state: ["online"] });
  let form: ReturnType<typeof Form.useForm>[0] | undefined;
  function Harness() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} initialValues={values}>
        <NodeSection fleet={fleet} knownNodes={new Map()} selection={undefined as never} modelReady result={null} loading={false} onNodeMode={onNodeMode as never} eligibleNodes={eligible as never} profile={profile} />
      </Form>
    );
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
  return () => form!;
}

test("a single-node replica chooses automatic or pinned candidates; multi-node and PD are other shapes", async () => {
  const modes: string[] = [];
  await renderSection({ node_selection_mode: "auto", target_vendor: "nvidia" }, (mode) => modes.push(mode), [{ uuid: "a", eligible: true }, { uuid: "b", eligible: true }, { uuid: "c", eligible: false }]);
  assert.ok(screen.getByText(node.single) && screen.getByText(node.multi) && screen.getByText(node.pd));
  assert.ok(screen.getByText(node.autoPoolDescription));
  assert.ok(screen.getByText(i18n.t("workbench.node.poolMatches", { count: 2, total: 3 })));
  const user = userEvent.setup({ document: dom.window.document });
  // "Only these nodes" explains itself behind the help mark on its label.
  assert.equal(screen.queryByText(node.poolNodesHelp), null);
  await user.hover(screen.getByRole("button", { name: read("common").helpTip.label }));
  assert.equal((await screen.findByRole("tooltip")).textContent, node.poolNodesHelp);
  await user.unhover(screen.getByRole("button", { name: read("common").helpTip.label }));
  // The candidate choice is a radio group, reachable by keyboard.
  const manual = screen.getByRole("radio", { name: node.manual });
  manual.focus();
  await user.keyboard(" ");
  assert.deepEqual(modes, ["manual"]);
  await user.click(screen.getByText(node.multi));
  assert.deepEqual(modes, ["manual", "multi"]);
});

test("an explicitly unsupported target profile disables and resets multi-node mode", async () => {
  const modes: string[] = [];
  const form = await renderSection(
    { node_selection_mode: "multi", target_vendor: "amd", nnodes: 2 },
    (mode) => { modes.push(mode); },
    undefined,
    { name: "vllm", multi_node_supported: false } as import("../src/api.ts").EngineProfile,
  );
  await waitFor(() => assert.equal(modes.length > 0, true));
  form().setFieldValue("node_selection_mode", modes.at(-1));
  await waitFor(() => assert.equal(form().getFieldValue("node_selection_mode") === "auto", true));
  assert.equal(screen.getByText(node.multi).closest("button")?.hasAttribute("disabled"), true);
  assert.deepEqual(modes, ["auto"]);
});

test("the strategy is a placement option of automatic candidates, spread by default", async () => {
  const form = await renderSection({ node_selection_mode: "auto", target_vendor: "nvidia" });
  assert.equal(form().getFieldValue("placement_strategy"), "spread");
  fireEvent.click(screen.getByText(node.placementOptions));
  fireEvent.click(await screen.findByText(node.strategyBinpack));
  assert.equal(form().getFieldValue("placement_strategy"), "binpack");
  cleanup();
  await renderSection({ node_selection_mode: "manual", target_vendor: "nvidia" });
  assert.equal(screen.queryByText(node.placementOptions), null, "a pinned node has nothing to spread over");
});

test("hardware and network conditions offer the server's labels with node counts, without node or vendor", async () => {
  const form = await renderSection({ node_selection_mode: "auto", target_vendor: "nvidia" });
  const control = screen.getByText(node.systemConditionsPlaceholder).closest(".ant-select") as HTMLElement;
  fireEvent.mouseDown(control.querySelector(".ant-select-selector") as HTMLElement);
  await waitFor(() => assert.ok(document.querySelectorAll(".ant-select-item-option").length > 0));
  const options = [...document.querySelectorAll(".ant-select-item-option")].map((el) => el.textContent);
  assert.deepEqual(options, [
    i18n.t("workbench.node.systemConditionOption", { key: "Compute network", value: "ib-east", count: 1 }),
    i18n.t("workbench.node.systemConditionOption", { key: "Compute network", value: "ib-west", count: 1 }),
    i18n.t("workbench.node.systemConditionOption", { key: "GPU model", value: "A100", count: 2 }),
    i18n.t("workbench.node.systemConditionOption", { key: "GPU model", value: "H100", count: 1 }),
  ]);
  const dropdown = document.querySelector(".ant-select-dropdown") as HTMLElement;
  fireEvent.click(within(dropdown).getByText(options[2]!));
  fireEvent.click(within(dropdown).getByText(options[3]!));
  assert.deepEqual(form().getFieldValue("system_conditions"), ["gpuworks.ai/gpu-model=A100", "gpuworks.ai/gpu-model=H100"]);
});

test("the payload states labels and hardware as node requirements, and the strategy for a single-node draft", () => {
  assert.deepEqual(systemConditionsToRequirements(["gpuworks.ai/gpu-model=A100", "gpuworks.ai/gpu-model=H100", "gpuworks.ai/compute-network=7", "bad", 3]), {
    "gpuworks.ai/gpu-model": ["A100", "H100"],
    "gpuworks.ai/compute-network": ["7"],
  });
  const { payload } = buildPayload({
    model_id: 1, node_uuid: "a", engine: "vllm", nnodes: 1, placement_strategy: "binpack",
    node_labels: { pool: "inference" }, system_conditions: ["gpuworks.ai/gpu-model=A100", "gpuworks.ai/gpu-model=H100"],
  });
  assert.deepEqual(payload.node_requirements, { "gpuworks.ai/gpu-model": ["A100", "H100"], pool: ["inference"] });
  assert.equal(payload.placement_strategy, "binpack");
  const multi = buildPayload({ model_id: 1, engine: "vllm", nnodes: 2, distributed_nodes: ["a", "b"], placement_strategy: "binpack", units_placement: "manual" }).payload;
  assert.equal(multi.placement_strategy, undefined, "a multi-node draft that names its nodes has nothing to spread");
  assert.equal(multi.node_requirements, undefined);
  const placed = buildPayload({ model_id: 1, engine: "vllm", nnodes: 2, placement_strategy: "binpack" }).payload;
  assert.equal(placed.placement_strategy, "binpack", "a multi-node draft the platform places packs or spreads its nodes");
});

test("automatic candidates can be limited to listed nodes of the target vendor; a pinned draft cannot", async () => {
  const form = await renderSection({ node_selection_mode: "auto", target_vendor: "nvidia" });
  const control = screen.getByText(node.poolNodesPlaceholder).closest(".ant-select") as HTMLElement;
  fireEvent.mouseDown(control.querySelector(".ant-select-selector") as HTMLElement);
  await waitFor(() => assert.ok(document.querySelectorAll(".ant-select-item-option").length > 0));
  const dropdown = [...document.querySelectorAll(".ant-select-dropdown")].at(-1) as HTMLElement;
  // Every node here is NVIDIA; the vendor filter keeps all three.
  assert.deepEqual([...dropdown.querySelectorAll(".ant-select-item-option")].map((el) => el.textContent), ["a100-1", "a100-2", "h100-1"]);
  fireEvent.click(within(dropdown).getByText("a100-1"));
  fireEvent.click(within(dropdown).getByText("h100-1"));
  assert.deepEqual(form().getFieldValue("pool_nodes"), ["a", "c"]);
  cleanup();
  await renderSection({ node_selection_mode: "auto", target_vendor: "ascend" });
  const other = screen.getByText(node.poolNodesPlaceholder).closest(".ant-select") as HTMLElement;
  fireEvent.mouseDown(other.querySelector(".ant-select-selector") as HTMLElement);
  await waitFor(() => assert.ok(document.querySelector(".ant-select-dropdown .ant-empty, .ant-select-dropdown .ant-select-item-empty")));
  assert.equal(document.querySelectorAll(".ant-select-dropdown .ant-select-item-option").length, 0, "no NVIDIA node is offered to an Ascend draft");
  cleanup();
  await renderSection({ node_selection_mode: "manual", target_vendor: "nvidia" });
  assert.equal(screen.queryByText(node.poolNodes), null, "a pinned draft names its one node");
});

test("the listed nodes travel as the node requirement of an automatic draft only", () => {
  const auto = buildPayload({ model_id: 1, engine: "vllm", nnodes: 1, node_selection_mode: "auto", pool_nodes: ["a", "c", ""], node_labels: { pool: "inference" } }).payload;
  assert.deepEqual(auto.node_requirements, { pool: ["inference"], "gpuworks.ai/node": ["a", "c"] });
  const manual = buildPayload({ model_id: 1, engine: "vllm", nnodes: 1, node_selection_mode: "manual", node_uuid: "a", pool_nodes: ["a", "c"] }).payload;
  assert.equal(manual.node_requirements, undefined, "a value left from automatic mode does not leak into a pinned draft");
  const multi = buildPayload({ model_id: 1, engine: "vllm", nnodes: 2, distributed_nodes: ["a", "b"], pool_nodes: ["a"] }).payload;
  assert.equal(multi.node_requirements, undefined);
});
