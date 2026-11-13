import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

// A card the vendor CLI does not list (passed through to a VM, no driver,
// or one the driver could not bring up) has index -1, and a node can hold
// several. Each place that lists cards must tell them apart, must not call
// them "GPU -1", and must not offer them for work.

class Events { addEventListener() {} removeEventListener() {} close() {} }
// The detail drawer mounts metric charts; a canvas that answers every call keeps them alive.
const anyContext: unknown = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: (await import("./helpers/dom.ts")).dom.window.localStorage, configurable: true });

(await import("./helpers/dom.ts")).dom.window.HTMLCanvasElement.prototype.getContext = (() => anyContext) as never;

const { i18n } = await createI18n(["gpus", "nodes", "deploy"], "en");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: GPUPage } = await import("../src/pages/GPUPage.tsx");
const { default: NodeGpuTab } = await import("../src/pages/nodes/NodeGpuTab.tsx");
const { default: DeployResourceStep } = await import("../src/pages/deploy/DeployResourceStep.tsx");
const { GPUGrid } = await import("../src/pages/deploy/DeployChoiceCards.tsx");
type GPUT = import("../src/api.ts").GPU;
type PhysicalGPUT = import("../src/api.ts").PhysicalGPU;
type NodeT = import("../src/api.ts").Node;

const originals = { gpusPage: api.gpusPage, dgpus: api.dgpus, metricsQuery: api.metricsQuery };
const errors: string[] = [];
const consoleError = console.error;
beforeEach(() => {
  errors.length = 0;
  console.error = (...args: unknown[]) => { errors.push(args.map(String).join(" ")); };
});
afterEach(() => {
  console.error = consoleError;
  cleanup();
  Object.assign(api, originals);
});

const noKeyWarning = () => {
  const dup = errors.filter((message) => /same key|unique "key"|Encountered two children/i.test(message));
  assert.deepEqual(dup, [], "React reported duplicate or missing keys");
};

const card = (over: Partial<GPUT>): GPUT => ({
  index: -1, uuid: "", name: "GA102 [GeForce RTX 3090]", compute_capability: "", arch_family: "", vendor: "nvidia",
  memory_total_bytes: 24 << 30, numa_node: -1, schedulable: false, ...over,
} as GPUT);

const passthrough = card({ ownership: "passthrough", pcie_bdf: "0000:1b:00.0" });
const noDriver = card({ ownership: "no_driver", pcie_bdf: "0000:1a:00.0" });
const served = card({ index: 0, uuid: "GPU-aaa", pcie_bdf: "0000:3b:00.0", schedulable: true, health_status: "OK" });
const busy = card({
  index: 1, uuid: "GPU-bbb", pcie_bdf: "0000:3c:00.0", schedulable: false, health_status: "OK",
  foreign_processes: [{ pid: 4242, name: "python3", memory_bytes: 8 << 30 }],
});
const gpus = [passthrough, noDriver, served, busy];

test("the GPU list keys cards without an index apart, never says GPU -1, and opens no detail for them", async () => {
  const physical = (g: GPUT): PhysicalGPUT => ({
    ...g, node_uuid: "n1", node_name: "gpu-a", node_state: "online",
    allocation_state: g.schedulable ? "available" : "unusable", allocated_bytes: 0, available_bytes: 0, occupants: [], load: {},
  } as unknown as PhysicalGPUT);
  api.dgpus = async () => ({ items: [] });
  api.gpusPage = async (query) => ({
    items: gpus.map(physical), page: query.page, page_size: query.page_size, total: gpus.length,
    overview: { total: 4, health: { ok: 4, warning: 0, error: 0 }, allocation: { allocated: 0, allocatable: 1, unusable: 3, unknown: 0 } },
    facets: { nodes: [{ uuid: "n1", name: "gpu-a" }], models: [], vendors: ["nvidia"] },
  } as never);
  render(<I18nextProvider i18n={i18n}><MemoryRouter><GPUPage /></MemoryRouter></I18nextProvider>);
  await waitFor(() => assert.equal(screen.getAllByRole("row").slice(1).length, 4));

  assert.equal(screen.queryByText(/GPU -1/), null, "a card with no device number is not called GPU -1");
  assert.ok(screen.getByText("0000:1b:00.0"), "named by its PCI address instead");
  assert.ok(screen.getByText("0000:1a:00.0"));
  assert.ok(screen.getByText("GPU 0"), "a numbered card keeps its number");
  noKeyWarning();

  // Each row says why its card takes no work, beside the verdict: these
  // cards have no detail page, so the row is the only place to say it.
  const reasonOf = (bdf: string) => {
    const row = [...screen.getAllByRole("row").slice(1)].find((r) => r.textContent?.includes(bdf))!;
    return row.textContent ?? "";
  };
  assert.ok(reasonOf("0000:1b:00.0").includes("Passed through"), "the passed-through card says so in its row");
  assert.ok(reasonOf("0000:1a:00.0").includes("No driver bound"), "the card without a driver says so in its row");
  assert.ok(reasonOf("GPU 1").includes("In use outside the platform"), "the card held outside the platform says so in its row");

  // A card the driver never answered for has no detail drawer and no charts.
  const rows = [...screen.getAllByRole("row").slice(1)];
  const bare = rows.filter((row) => row.textContent?.includes("0000:1b:00.0") || row.textContent?.includes("0000:1a:00.0"));
  assert.equal(bare.length, 2);
  for (const row of bare) {
    assert.ok(!row.classList.contains("gpu-clickable-row"), "the row is not offered as clickable");
    assert.ok((within(row).getByRole("button", { name: "Details" }) as HTMLButtonElement).disabled, "its details button is disabled");
    fireEvent.click(row);
  }
  assert.equal(document.querySelector(".gpu-detail-drawer"), null, "no detail drawer opened");
  const numbered = rows.find((row) => row.textContent?.includes("GPU 0"))!;
  assert.ok(numbered.classList.contains("gpu-clickable-row"));
  fireEvent.click(numbered);
  await waitFor(() => assert.ok(screen.queryByRole("dialog", { name: /GPU 0/ })));
});

test("the node's GPU list keys cards apart, reads no metrics for them, and tags them like the card table", async () => {
  api.metricsQuery = (async () => ({
    items: [
      { labels: { node: "n1", gpu: "0", gpu_uuid: "GPU-aaa" }, points: [[Date.now(), 42]] },
      // Whatever is filed under -1 belongs to no card in particular.
      { labels: { node: "n1", gpu: "-1" }, points: [[Date.now(), 99]] },
    ],
  })) as never;
  const node = { uuid: "n1", hostname: "gpu-a", state: "online", os: "linux", gpu_count: 4, gpus } as unknown as NodeT;
  render(<I18nextProvider i18n={i18n}><MemoryRouter><NodeGpuTab node={node} onUpdated={() => {}} /></MemoryRouter></I18nextProvider>);
  await screen.findByText("42%");
  assert.ok(screen.getByText("0000:1b:00.0"));
  assert.ok(screen.getByText("0000:1a:00.0"));
  assert.equal(screen.queryByText(/GPU\s?-1/), null);
  assert.equal(screen.queryByText("99%"), null, "no card was handed the -1 series");
  assert.ok(screen.getByText("Passed through"));
  assert.ok(screen.getByText("No driver bound"));
  assert.ok(screen.getByText("In use outside the platform"), "a card held by someone else's process is not shown as faulty");
  assert.equal(screen.queryByText("Unhealthy"), null);
  assert.equal(screen.queryByText("Fault"), null);
  noKeyWarning();
});

test("the deploy GPU select greys out cards that cannot be picked and says why", async () => {
  const user = userEvent.setup({ document });
  const node = { uuid: "n1", hostname: "gpu-a", state: "online", gpu_count: 2, gpus } as unknown as NodeT;
  render(
    <I18nextProvider i18n={i18n}>
      <Form layout="vertical" initialValues={{ node_selection_mode: "manual", nnodes: 1 }}>
        <DeployResourceStep selectedNode={node} result={{ gpu_candidates: { node_uuid: node.uuid, items: [{ index: 0, available: true }, { index: 1, available: false, reason: { code: "gpus_held_outside_platform" } }] } } as import("../src/api.ts").EvaluateResponse} />
      </Form>
    </I18nextProvider>,
  );
  await user.click(screen.getByRole("combobox", { name: /GPU Selection/ }));
  const options = [...document.querySelectorAll<HTMLElement>(".ant-select-item-option")];
  assert.equal(options.length, 4);
  const disabled = options.filter((option) => option.classList.contains("ant-select-item-option-disabled"));
  assert.equal(disabled.length, 3, "the two unnumbered cards and the one somebody else is using");
  const text = options.map((option) => option.textContent ?? "").join("\n");
  assert.ok(!/GPU -1/.test(text), text);
  assert.match(text, /0000:1b:00\.0.*Passed through/);
  assert.match(text, /0000:1a:00\.0.*No driver bound/);
  assert.match(text, /GPU 1 .*In use outside the platform/);
  const open = options.find((option) => option.textContent?.startsWith("GPU 0 ·"))!;
  assert.ok(!open.classList.contains("ant-select-item-option-disabled"));
  noKeyWarning();
});

test("the manual GPU grid disables unnumbered and unschedulable cards", () => {
  const node = { uuid: "n1", hostname: "gpu-a", state: "online", gpu_count: 4, gpus } as unknown as NodeT;
  render(
    <I18nextProvider i18n={i18n}>
      <Form initialValues={{ node_selection_mode: "manual", gpu_indexes: [] }}><GPUGrid node={node} /></Form>
    </I18nextProvider>,
  );
  const buttons = within(screen.getByRole("group", { name: "GPU selection" })).getAllByRole("button") as HTMLButtonElement[];
  assert.equal(buttons.length, 4);
  assert.deepEqual(buttons.map((button) => button.disabled), [true, true, false, true]);
  assert.deepEqual(buttons.map((button) => button.textContent), ["0000:1b:00.0", "0000:1a:00.0", "0", "1"]);
  assert.match(buttons[0].title, /Passed through/);
  assert.match(buttons[3].title, /In use outside the platform/);
  noKeyWarning();
  fireEvent.click(buttons[0]);
});
