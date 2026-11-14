import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { installFleet } from "./helpers/fakeFleet.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { fixturePdModes } from "./helpers/engineFixture.ts";
import { list } from "./helpers/listPage.ts";

// The deployment detail of a multi-node or PD deployment shows who places it
// and switches that (confirm, the API call, a refusal inside the dialog); an
// automatically placed one scales by count with optional first nodes, each
// slot saying whether it is specified or scheduled automatically.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { ReplicasEditor } = await import("../src/pages/DeploymentReplicaActions.tsx");
const { default: PlacementModeControl } = await import("../src/pages/DeploymentPlacementMode.tsx");
const { default: DeploymentPdScaleModal } = await import("../src/pages/DeploymentPdScaleModal.tsx");
type Deployment = import("../src/api.ts").Deployment;

const original = { ...api };
const up = locale.deployments.unitsPlacement;
const rep = locale.deployments.replica;

function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 7, name: "big", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n-a",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas: 1, max_replicas: 8, nnodes: 2,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    units: [{ node_uuid: "n-a", followers: [{ node_uuid: "n-b" }] }],
    ...overrides,
  } as unknown as Deployment;
}

function pdDeployment(overrides: Record<string, unknown> = {}): Deployment {
  return deployment({
    nnodes: undefined, units: undefined,
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail", state: "running", short: false,
      roles: [
        { role: "decode", tp: 2, desired: 1, ready: 1, units: [{ node_uuid: "n-a", gpus: [] }] },
        { role: "prefill", tp: 1, desired: 1, ready: 1, units: [{ node_uuid: "n-b", gpus: [] }] },
      ],
    },
    ...overrides,
  });
}

function stubFleet() {
  installFleet(api as unknown as Record<string, unknown>, [
    { uuid: "n-a", name: "node-a", state: "online", gpu_count: 2, scheduling: "schedulable" },
    { uuid: "n-b", name: "node-b", state: "online", gpu_count: 2, scheduling: "schedulable" },
    { uuid: "n-c", name: "node-c", state: "online", gpu_count: 2, scheduling: "schedulable" },
    { uuid: "n-d", name: "node-d", state: "online", gpu_count: 2, scheduling: "schedulable" },
    { uuid: "n-off", name: "node-off", state: "offline", scheduling: "schedulable" },
  ]);
}

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

// antd keeps a closed select's dropdown mounted, so the options of the select
// opened last are the ones in the last dropdown.
const optionOf = (name: string) => {
  const dropdowns = document.querySelectorAll<HTMLElement>(".ant-select-dropdown");
  return [...(dropdowns[dropdowns.length - 1]?.querySelectorAll<HTMLElement>(".ant-select-item-option") ?? [])].find((el) => el.textContent === name);
};
async function pick(combobox: HTMLElement, name: string) {
  fireEvent.mouseDown(combobox.closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement);
  await waitFor(() => assert.ok(optionOf(name), `option ${name} offered`));
  fireEvent.click(optionOf(name)!);
}

// ---- who places the deployment -------------------------------------------

function mountMode(dep: Deployment, onChanged = () => {}) {
  render(<I18nextProvider i18n={i18n}><PlacementModeControl dep={dep} onChanged={onChanged} /></I18nextProvider>);
  return userEvent.setup();
}

for (const strategy of ["binpack", "spread"] as const) {
  test(`automatic placement shows its ${strategy} strategy before the action to switch modes`, () => {
    mountMode(deployment({ placement_mode: "auto", placement_strategy: strategy }));
    const modeTag = screen.getByText(up.auto);
    const strategyTag = screen.getByText(locale.deployments.detail.placement.strategy[strategy]);
    const switchButton = screen.getByRole("button", { name: up.toManual });
    assert.ok(modeTag.compareDocumentPosition(strategyTag) & Node.DOCUMENT_POSITION_FOLLOWING);
    assert.ok(strategyTag.compareDocumentPosition(switchButton) & Node.DOCUMENT_POSITION_FOLLOWING);
    assert.ok(screen.getByText(up.autoHint));
    assert.equal(screen.queryByRole("button", { name: up.toAuto }), null);
  });
}

test("a manual deployment says so and offers the switch to automatic", () => {
  mountMode(deployment({ placement_mode: "manual" }));
  assert.ok(screen.getByText(up.manual));
  assert.ok(screen.getByText(up.manualHint));
  assert.ok(screen.getByRole("button", { name: up.toAuto }));
});

test("a deployment with no placement mode (single node) shows nothing", () => {
  const { container } = render(<I18nextProvider i18n={i18n}><PlacementModeControl dep={deployment({ nnodes: 1, units: undefined })} onChanged={() => {}} /></I18nextProvider>);
  assert.equal(container.textContent, "");
});

test("switching to automatic explains what changes, then calls the endpoint and refreshes", async () => {
  const calls: Array<[number, string]> = [];
  api.setPlacementMode = (async (id: number, mode: string) => { calls.push([id, mode]); return deployment({ placement_mode: mode }); }) as typeof api.setPlacementMode;
  let changed = 0;
  const user = mountMode(pdDeployment({ placement_mode: "manual" }), () => { changed += 1; });
  await user.click(screen.getByRole("button", { name: up.toAuto }));
  const dialog = await screen.findByRole("dialog");
  assert.ok(within(dialog).getByText(up.confirmTitleAuto));
  assert.ok(within(dialog).getByText(up.confirmAuto), "says the pinned cards are cleared and nothing restarts");
  assert.deepEqual(calls, [], "nothing is sent before the confirmation");
  await user.click(within(dialog).getByRole("button", { name: up.toAuto }));
  await waitFor(() => assert.deepEqual(calls, [[7, "auto"]]));
  await waitFor(() => assert.equal(changed, 1));
  await screen.findByText(up.switchedAuto);
});

test("switching to manual explains that the nodes are fixed, and cancelling sends nothing", async () => {
  const calls: unknown[] = [];
  api.setPlacementMode = (async (...args: unknown[]) => { calls.push(args); return deployment(); }) as typeof api.setPlacementMode;
  const user = mountMode(deployment({ placement_mode: "auto" }));
  await user.click(screen.getByRole("button", { name: up.toManual }));
  const dialog = await screen.findByRole("dialog");
  assert.ok(within(dialog).getByText(up.confirmTitleManual));
  assert.ok(within(dialog).getByText(up.confirmManual));
  await user.click(within(dialog).getByRole("button", { name: locale.deployments.detail.cancel }));
  assert.deepEqual(calls, []);
});

test("a refused switch stays in the dialog with the server's reason and refreshes nothing", async () => {
  api.setPlacementMode = (async () => { throw new APIError("PLACEMENT_MODE_SHAPE", {}, "single node", 400); }) as typeof api.setPlacementMode;
  let changed = 0;
  const user = mountMode(deployment({ placement_mode: "auto" }), () => { changed += 1; });
  await user.click(screen.getByRole("button", { name: up.toManual }));
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("button", { name: up.toManual }));
  await within(dialog).findByText(i18n.t("deployments:unitsPlacement.switchFailed", { detail: i18n.t("common:apiError.PLACEMENT_MODE_SHAPE") }));
  assert.equal(changed, 0);
});

// ---- scaling an automatically placed multi-node deployment ----------------

function mountReplicas(dep: Deployment, onChanged = () => {}) {
  render(<I18nextProvider i18n={i18n}><ReplicasEditor dep={dep} onChanged={onChanged} /></I18nextProvider>);
  return userEvent.setup();
}

let scaled: unknown[] = [];
function stubScale(scale: (body: unknown) => Promise<unknown> = async () => deployment()) {
  scaled = [];
  stubFleet();
  api.scaleReplicas = (async (_id: number, body: unknown) => { scaled.push(body); return scale(body); }) as typeof api.scaleReplicas;
}

const slot = (replica: number, rank: number) => screen.getByRole("combobox", { name: up.scale.rankSlotAria.replace("{{index}}", String(replica)).replace("{{rank}}", String(rank)) });
const setCount = (value: string) => fireEvent.change(screen.getByLabelText(locale.deployments.detail.replicas) as HTMLInputElement, { target: { value } });
const save = () => within(screen.getByRole("dialog")).getByRole("button", { name: rep.apply });

async function openAuto(dep = deployment({ placement_mode: "auto" })) {
  const user = mountReplicas(dep);
  await user.click(screen.getByRole("button", { name: rep.editUnits }));
  await screen.findByRole("dialog");
  return user;
}

test("an automatic deployment scales by count: no per-rank list, no suggested replica", async () => {
  stubScale();
  await openAuto();
  assert.ok(screen.getByText(rep.autoTitle));
  assert.equal(screen.queryByRole("button", { name: rep.suggestUnit }), null, "the suggestion belongs to manual placement");
  assert.equal(screen.queryByRole("button", { name: rep.addUnit }), null);
  assert.equal(screen.queryByText(up.scale.autoNote), null, "nothing to name while the count does not grow");
});

test("a manual deployment keeps its list editor with the suggested replica", async () => {
  stubScale();
  const user = mountReplicas(deployment({ placement_mode: "manual" }));
  await user.click(screen.getByRole("button", { name: rep.editUnits }));
  await screen.findByRole("dialog");
  assert.ok(screen.getByRole("button", { name: rep.suggestUnit }));
  assert.ok(screen.getByRole("button", { name: rep.addUnit }));
});

test("growing offers a slot per rank of each new replica, all scheduled automatically until named", async () => {
  stubScale();
  await openAuto();
  setCount("3");
  assert.ok(screen.getByText(up.scale.autoNote));
  // The rank order rule is behind the help mark beside the note.
  assert.equal(screen.queryByText(up.scale.prefixHelp), null);
  await userEvent.setup().hover(screen.getByRole("button", { name: locale.common.helpTip.label }));
  assert.equal((await screen.findByRole("tooltip")).textContent, up.scale.prefixHelp);
  assert.ok(screen.getByText(up.scale.newReplica.replace("{{index}}", "1")));
  assert.ok(screen.getByText(up.scale.newReplica.replace("{{index}}", "2")));
  assert.equal(screen.getAllByText(up.scale.automatic).length, 4, "2 new replicas x 2 ranks");
  assert.equal(screen.queryAllByText(up.scale.specified).length, 0);
});

test("naming only the first rank of the new replica sends it as place_on and tags the rest automatic", async () => {
  stubScale();
  const user = await openAuto();
  setCount("2");
  await pick(slot(1, 1), "node-c");
  assert.equal(screen.getAllByText(up.scale.specified).length, 1);
  assert.equal(screen.getAllByText(up.scale.automatic).length, 1);
  await user.click(save());
  await waitFor(() => assert.deepEqual(scaled, [{ replicas: 2, place_on: ["n-c"] }]));
});

test("a rank can be named only after the ranks before it, and clearing one clears those after", async () => {
  stubScale();
  await openAuto();
  setCount("3");
  assert.ok(slot(1, 2).closest(".ant-select")!.classList.contains("ant-select-disabled"), "rank 2 waits for rank 1");
  assert.ok(slot(2, 1).closest(".ant-select")!.classList.contains("ant-select-disabled"));
  await pick(slot(1, 1), "node-c");
  await pick(slot(1, 2), "node-d");
  assert.ok(!slot(2, 1).closest(".ant-select")!.classList.contains("ant-select-disabled"));
  assert.equal(screen.getAllByText(up.scale.specified).length, 2);
  // Clearing rank 1 leaves nothing named after it.
  fireEvent.mouseEnter(slot(1, 1).closest(".ant-select")!);
  fireEvent.mouseDown(slot(1, 1).closest(".ant-select")!.querySelector(".ant-select-clear") as HTMLElement);
  await waitFor(() => assert.equal(screen.queryAllByText(up.scale.specified).length, 0));
});

test("growing with nothing named sends the count alone", async () => {
  stubScale();
  const user = await openAuto();
  setCount("2");
  await user.click(save());
  await waitFor(() => assert.deepEqual(scaled, [{ replicas: 2 }]));
});

test("shrinking sends the count alone", async () => {
  stubScale();
  const user = await openAuto(deployment({ placement_mode: "auto", replicas: 3, units: [] }));
  setCount("2");
  await user.click(save());
  await waitFor(() => assert.deepEqual(scaled, [{ replicas: 2 }]));
});

test("an offline node cannot be named", async () => {
  stubScale();
  await openAuto();
  setCount("2");
  fireEvent.mouseDown(slot(1, 1).closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement);
  await waitFor(() => assert.ok(optionOf("node-off")));
  assert.ok(optionOf("node-off")!.classList.contains("ant-select-item-option-disabled"));
});

test("a named node the server refuses is reported by node and reason inside the dialog", async () => {
  stubScale(async () => { throw new APIError("REPLICAS_PLACE_ON_REFUSED", { node: "node-c", reason: "no_group", detail: "" }, "refused", 409); });
  const user = await openAuto();
  setCount("2");
  await pick(slot(1, 1), "node-c");
  await user.click(save());
  await within(screen.getByRole("dialog")).findByText(i18n.t("deployments:replica.scaleFailed", { detail: i18n.t("deployments:replica.placeOn.refused.no_group", { node: "node-c" }) }));
});

test("no group of nodes for the new replica is the server's own explanation", async () => {
  stubScale(async () => { throw new APIError("UNIT_PLACEMENT_NONE", { nodes: 4, unmet: 0, cordoned: 0, offline: 1, busy: 3, no_network: 0, largest: 1 }, "none", 409); });
  const user = await openAuto();
  setCount("2");
  await user.click(save());
  await within(screen.getByRole("dialog")).findByText(i18n.t("deployments:replica.scaleFailed", { detail: "No group of nodes can take this deployment (of 4 nodes): 1 offline · 3 lack free GPUs · largest group sharing a network and GPU architecture: 1" }));
});

// ---- scaling an automatically placed PD deployment ------------------------

const fixtureModes = fixturePdModes("vllm");

function mountPd(dep: Deployment, onChanged = () => {}) {
  stubFleet();
  api.engines = (async () => list([{ name: "vllm", sleep: { supported: false }, quick_form_keys: [], param_specs: [], pd_modes: fixtureModes }])) as unknown as typeof api.engines;
  render(<I18nextProvider i18n={i18n}><DeploymentPdScaleModal open dep={dep} onClose={() => {}} onChanged={onChanged} /></I18nextProvider>);
  return userEvent.setup();
}

const addUnit = (role: "decode" | "prefill") => screen.getByRole("button", { name: locale.deployments.detail.pd.scale.addUnit.replace("{{role}}", locale.deploy.pd.role[role]) });
const unitNode = (role: "decode" | "prefill", index: number) => screen.getByRole("combobox", { name: locale.deploy.pd.unitNodeAria.replace("{{role}}", locale.deploy.pd.role[role]).replace("{{index}}", String(index)) });
const saveRoles = () => within(screen.getByRole("dialog")).getByRole("button", { name: locale.deployments.detail.pd.scale.save });

test("an automatic PD deployment's new unit may go without a node and is tagged scheduled automatically", async () => {
  const calls: unknown[] = [];
  api.updateDeploymentPd = (async (_id: number, body: unknown) => { calls.push(body); return pdDeployment(); }) as typeof api.updateDeploymentPd;
  const user = mountPd(pdDeployment({ placement_mode: "auto" }));
  await screen.findByRole("dialog");
  assert.ok(screen.getByText(up.scale.autoNote));
  // Placed units name no cards here; the existing ones show no tag.
  assert.equal(screen.queryByRole("combobox", { name: new RegExp(`${locale.deploy.pd.role.decode} instance 1 GPUs`) }), null);
  assert.equal(screen.queryAllByText(locale.deploy.pd.autoScheduled).length, 0);
  await user.click(addUnit("decode"));
  await waitFor(() => assert.ok(unitNode("decode", 2)));
  assert.equal(screen.getAllByText(locale.deploy.pd.autoScheduled).length, 1);
  await user.click(saveRoles());
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], {
    roles: {
      decode: { units: [{ node_uuid: "n-a" }, {}] },
      prefill: { units: [{ node_uuid: "n-b" }] },
    },
    fallback: "decode",
  });
});

test("naming a node for the new PD unit tags it specified and sends it as the first placement", async () => {
  const calls: Array<{ roles: { prefill: { units: unknown[] } } }> = [];
  api.updateDeploymentPd = (async (_id: number, body: never) => { calls.push(body); return pdDeployment(); }) as typeof api.updateDeploymentPd;
  const user = mountPd(pdDeployment({ placement_mode: "auto" }));
  await screen.findByRole("dialog");
  await user.click(addUnit("prefill"));
  await waitFor(() => assert.ok(unitNode("prefill", 2)));
  await pick(unitNode("prefill", 2), "node-c · 2 GPU");
  await waitFor(() => assert.equal(screen.getAllByText(locale.deploy.pd.nodeNamed).length, 1));
  await user.click(saveRoles());
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0].roles.prefill.units, [{ node_uuid: "n-b" }, { node_uuid: "n-c" }]);
});

test("a manual PD deployment still needs a node for every unit and offers GPU picks", async () => {
  const calls: unknown[] = [];
  api.updateDeploymentPd = (async (_id: number, body: unknown) => { calls.push(body); return pdDeployment(); }) as typeof api.updateDeploymentPd;
  const user = mountPd(pdDeployment({ placement_mode: "manual" }));
  await screen.findByRole("dialog");
  assert.equal(screen.queryByText(up.scale.autoNote), null);
  await user.click(addUnit("decode"));
  await waitFor(() => assert.ok(unitNode("decode", 2)));
  assert.equal(screen.queryAllByText(locale.deploy.pd.autoScheduled).length, 0);
  assert.ok(screen.getByRole("combobox", { name: locale.deploy.pd.unitGpuAria.replace("{{role}}", locale.deploy.pd.role.decode).replace("{{index}}", "2") }));
  // A manual unit without a node is incomplete: nothing is sent until it has one.
  await user.click(saveRoles());
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.deepEqual(calls, []);
  await pick(unitNode("decode", 2), "node-c · 2 GPU");
  await user.click(saveRoles());
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual((calls[0] as { roles: { decode: { units: unknown[] } } }).roles.decode.units, [{ node_uuid: "n-a" }, { node_uuid: "n-c" }]);
});

test("an unplaceable new PD unit is the server's explanation inside the dialog", async () => {
  api.updateDeploymentPd = (async () => { throw new APIError("UNIT_PLACEMENT_NONE", { nodes: 2, unmet: 0, cordoned: 0, offline: 0, busy: 2, no_network: 0, largest: 0 }, "none", 409); }) as typeof api.updateDeploymentPd;
  const user = mountPd(pdDeployment({ placement_mode: "auto" }));
  await screen.findByRole("dialog");
  await user.click(addUnit("decode"));
  await user.click(saveRoles());
  await within(screen.getByRole("dialog")).findByText(i18n.t("deployments:detail.pd.scale.failed", { detail: "No group of nodes can take this deployment (of 2 nodes): 2 lack free GPUs · largest group sharing a network and GPU architecture: 0" }));
});
