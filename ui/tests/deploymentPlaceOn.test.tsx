import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { installFleet } from "./helpers/fakeFleet.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// Growing a single-node deployment may name the nodes the new replicas start
// on first (place_on), as many as are added; the server's refusals are worded
// by node and reason.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { ReplicasEditor } = await import("../src/pages/DeploymentReplicaActions.tsx");
type Deployment = import("../src/api.ts").Deployment;

const r = locale.deployments.replica;
const original = { ...api };

function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n-a",
    desired_state: "active", runtime_state: "running", sleepable: false, replicas: 1, max_replicas: 8, nnodes: 1,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    ...overrides,
  } as unknown as Deployment;
}

let scaled: unknown[] = [];

function stub(scale: (body: unknown) => Promise<unknown> = async () => deployment()) {
  scaled = [];
  installFleet(api as unknown as Record<string, unknown>, [
    { uuid: "n-a", name: "node-a", state: "online", scheduling: "schedulable" },
    { uuid: "n-b", name: "node-b", state: "online", scheduling: "schedulable" },
    { uuid: "n-c", name: "node-c", state: "online", scheduling: "schedulable" },
    { uuid: "n-off", name: "node-off", state: "offline", scheduling: "schedulable" },
  ]);
  api.scaleReplicas = (async (_id: number, body: unknown) => { scaled.push(body); return scale(body); }) as typeof api.scaleReplicas;
}

function mount(dep = deployment(), onChanged = () => {}) {
  render(<I18nextProvider i18n={i18n}><ReplicasEditor dep={dep} onChanged={onChanged} /></I18nextProvider>);
  return userEvent.setup();
}

const count = () => screen.getByLabelText(locale.deployments.detail.replicas) as HTMLInputElement;
function setCount(value: string) {
  fireEvent.change(count(), { target: { value } });
}
const picker = () => screen.getByRole("combobox", { name: r.placeOn.label });
const apply = () => screen.getByText(r.apply).closest("button") as HTMLButtonElement;

// optionOf finds an offered option of the open picker by its node name.
const optionOf = (name: string) => [...document.querySelectorAll<HTMLElement>(".ant-select-item-option")].find((el) => el.textContent === name);

async function choose(...names: string[]) {
  for (const name of names) {
    // A multiple select stays open after a pick; opening it again would close it.
    if (!optionOf(name)) fireEvent.mouseDown(picker().closest(".ant-select")!.querySelector(".ant-select-selector") as HTMLElement);
    await waitFor(() => assert.ok(optionOf(name), `option ${name} offered`));
    fireEvent.click(optionOf(name)!);
  }
}

afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

test("the node choice appears only while growing, limited to the replicas added", async () => {
  stub();
  mount(deployment({ replicas: 2 }));
  assert.equal(screen.queryByRole("combobox", { name: r.placeOn.label }), null);
  setCount("2");
  assert.equal(screen.queryByRole("combobox", { name: r.placeOn.label }), null);
  setCount("1");
  assert.equal(screen.queryByRole("combobox", { name: r.placeOn.label }), null);
  setCount("3");
  assert.ok(picker());
  assert.ok(screen.getByText(i18n.t("deployments:replica.placeOn.placeholder", { count: 1 })));
  setCount("5");
  assert.ok(screen.getByText(i18n.t("deployments:replica.placeOn.placeholder", { count: 3 })));
  assert.ok(screen.getByText(r.placeOn.hint));
});

test("growing sends the chosen nodes as place_on and nothing else changes", async () => {
  stub();
  let changed = 0;
  const user = mount(deployment(), () => { changed += 1; });
  setCount("3");
  await choose("node-b");
  await user.click(screen.getByRole("button", { name: locale.deployments.detail.cancel }));
  assert.deepEqual(scaled, []);
  setCount("3");
  await choose("node-b", "node-c");
  await user.click(apply());
  await waitFor(() => assert.deepEqual(scaled, [{ replicas: 3, place_on: ["n-b", "n-c"] }]));
  await waitFor(() => assert.equal(changed, 1));
});

test("growing without choosing a node sends the count alone", async () => {
  stub();
  const user = mount();
  setCount("2");
  await user.click(apply());
  await waitFor(() => assert.deepEqual(scaled, [{ replicas: 2 }]));
});

test("no more nodes than replicas added can be chosen, and a node that is offline cannot", async () => {
  stub();
  mount();
  setCount("2");
  await choose("node-b");
  await waitFor(() => assert.ok(optionOf("node-c")));
  assert.ok(optionOf("node-c")!.classList.contains("ant-select-item-option-disabled"));
  assert.ok(optionOf("node-off")!.classList.contains("ant-select-item-option-disabled"));
});

test("choices beyond a count lowered afterwards are dropped from the order", async () => {
  stub();
  const user = mount();
  setCount("3");
  await choose("node-b", "node-c");
  setCount("2");
  await user.click(apply());
  await waitFor(() => assert.deepEqual(scaled, [{ replicas: 2, place_on: ["n-b"] }]));
});

test("a refused first placement names the node and why", async () => {
  stub(async () => { throw new APIError("REPLICAS_PLACE_ON_REFUSED", { node: "node-off", reason: "offline" }, "node node-off cannot take a new replica: offline", 409); });
  const user = mount();
  setCount("2");
  await choose("node-b");
  await user.click(apply());
  await screen.findByText(i18n.t("deployments:replica.scaleFailed", { detail: i18n.t("deployments:replica.placeOn.refused.offline", { node: "node-off" }) }));
});

test("a node without room gives the scheduler's reason; a requirements refusal gives the server's detail", async () => {
  stub(async () => {
    throw new APIError("REPLICAS_PLACE_ON_REFUSED", { node: "node-b", reason: "no_room", code: "no_fit", params: { gpus: 1, tp: 1, pp: 1, gpu_count: 0, required_gib: "20", available_gib: "0" } }, "no room", 409);
  });
  const user = mount();
  setCount("2");
  await choose("node-b");
  await user.click(apply());
  const reason = i18n.t("deploy:schedule.no_fit", { gpus: 1, tp: 1, pp: 1, gpu_count: 0, required_gib: "20", available_gib: "0" });
  await screen.findByText(i18n.t("deployments:replica.scaleFailed", { detail: i18n.t("deployments:replica.placeOn.refused.no_room", { node: "node-b", detail: reason }) }));
  cleanup();

  stub(async () => { throw new APIError("REPLICAS_PLACE_ON_REFUSED", { node: "node-b", reason: "requirements", detail: "needs label zone=a" }, "requirements", 409); });
  const second = mount();
  setCount("2");
  await choose("node-b");
  await second.click(apply());
  await screen.findByText(i18n.t("deployments:replica.scaleFailed", { detail: i18n.t("deployments:replica.placeOn.refused.requirements", { node: "node-b", detail: "needs label zone=a" }) }));
});

test("naming more nodes than replicas are added is refused by the server in its own words", async () => {
  stub(async () => { throw new APIError("REPLICAS_PLACE_ON_TOO_MANY", { count: 2, maximum: 1 }, "too many", 400); });
  const user = mount();
  setCount("2");
  await choose("node-b");
  await user.click(apply());
  await screen.findByText(i18n.t("deployments:replica.scaleFailed", { detail: i18n.t("common:apiError.REPLICAS_PLACE_ON_TOO_MANY", { count: 2, maximum: 1 }) }));
});

test("a multi-node deployment keeps its list editor and has no first-placement choice", async () => {
  stub();
  mount(deployment({ nnodes: 2, replicas: 1, units: [{ node_uuid: "n-a", followers: [{ node_uuid: "n-b" }] }] }));
  assert.ok(screen.getByRole("button", { name: r.editUnits }));
  assert.equal(screen.queryByRole("combobox", { name: r.placeOn.label }), null);
});
