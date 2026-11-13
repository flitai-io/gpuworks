import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

// Creating a multi-node or PD deployment the platform places: the request
// states placement_mode "auto" and names no node or card, the evaluation's
// preview is not what gets created, and a draft the server found nothing to
// place on is reported in the server's words instead of being sent.

const { i18n, locale } = await createI18n(["deploy"], "en");
const { cleanup, render, screen, waitFor, act } = await import("@testing-library/react");
const { Form, Input, message } = await import("antd");
const { default: useDeploySubmit } = await import("../src/pages/deploy/useDeploySubmit.ts");
const { default: DeploySubmitBar } = await import("../src/pages/deploy/DeploySubmitBar.tsx");
const { api } = await import("../src/api.ts");
const { APIError, apiErrorText } = await import("../src/apiError.ts");
type EvaluateResponse = import("../src/api.ts").EvaluateResponse;
const originalCreate = api.createDeployment;
afterEach(() => { cleanup(); message.destroy(); api.createDeployment = originalCreate; });
after(() => dom.window.close());

const base = { model_id: 3, name: "big", served_name: "big", target_vendor: "nvidia", engine: "vllm" };
const pdRoles = { decode: { tp: 2, units: [{ node_uuid: "stale", gpus: [0, 1] }, {}] }, prefill: { tp: 1, units: [{}] } };
const evaluated = (extra: Record<string, unknown> = {}) => ({
  ok: true, image: "fixture:test", auto_placed: true, preflight: { ok: true }, ...extra,
}) as unknown as EvaluateResponse;

function mount(values: Record<string, unknown>, result: EvaluateResponse) {
  const requests: Array<Record<string, unknown>> = [];
  let pending: Promise<void> | undefined;
  api.createDeployment = (async (body: Record<string, unknown>) => { requests.push(body); return { id: 1 }; }) as never;
  function Harness() {
    const [form] = Form.useForm();
    const { submit, submitting } = useDeploySubmit({ form, nodes: [], evalErr: null, evaluating: false, evalResult: result, onCreated: () => {} });
    return <Form form={form} initialValues={{ ...base, ...values }}>
      <Form.Item name="name"><Input /></Form.Item>
      <DeploySubmitBar submitting={submitting} disabled={false} submit={() => { pending = submit(); }} />
    </Form>;
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
  const click = async () => {
    await act(async () => {
      screen.getByRole("button", { name: locale.deploy.page.deploy as string }).click();
      await pending;
    });
  };
  return { requests, click };
}

test("an automatic PD create states auto and the counts, with no node, card or primary node", async () => {
  const run = mount({ node_selection_mode: "pd", nnodes: 1, pd_mode: "vllm-nixl", pd_fallback: "decode", pd_roles: pdRoles, placement_strategy: "binpack", node_uuid: "stale" }, evaluated());
  await run.click();
  assert.equal(run.requests.length, 1);
  const body = run.requests[0] as Record<string, any>;
  assert.equal(body.placement_mode, "auto");
  assert.equal(body.placement_strategy, "binpack");
  assert.equal(body.node_uuid, undefined);
  assert.deepEqual(body.placement.pd.roles, { decode: { tp: 2, units: [{}, {}] }, prefill: { tp: 1, units: [{}] } });
  assert.equal(body.placement.gpus, undefined);
});

test("a manual PD create names its units' nodes and says neither auto nor manual", async () => {
  const run = mount({ node_selection_mode: "pd", nnodes: 1, units_placement: "manual", pd_mode: "vllm-nixl", pd_roles: { decode: { tp: 1, units: [{ node_uuid: "n1", gpus: [0] }] }, prefill: { tp: 1, units: [{ node_uuid: "n2" }] } } }, evaluated({ auto_placed: undefined }));
  await run.click();
  const body = run.requests[0] as Record<string, any>;
  assert.equal("placement_mode" in body, false);
  assert.deepEqual(body.placement.pd.roles.decode.units, [{ node_uuid: "n1", gpus: [0] }]);
});

test("an automatic multi-node create names no node, keeps placement.tp, and is not held to a node count", async () => {
  const run = mount({ node_selection_mode: "multi", nnodes: 3, tp: 8, distributed_nodes: ["a"], node_uuid: "a", master_addr: "10.0.0.1", placement_strategy: "binpack", gpu_indexes: [0] },
    evaluated({ rank_commands: [{ node_uuid: "a" }, { node_uuid: "b" }, { node_uuid: "c" }] }));
  await run.click();
  assert.equal(run.requests.length, 1, "one named node of three is not an error when the platform picks");
  const body = run.requests[0] as Record<string, any>;
  assert.equal(body.placement_mode, "auto");
  assert.equal(body.nnodes, 3);
  assert.deepEqual(body.placement, { tp: 8 });
  for (const key of ["node_uuids", "node_uuid", "master_addr"]) assert.equal(key in body, false, key);
});

test("a manual multi-node create still needs a node for every rank", async () => {
  const run = mount({ node_selection_mode: "multi", nnodes: 3, units_placement: "manual", tp: 8, distributed_nodes: ["a"] }, evaluated({ auto_placed: undefined }));
  await run.click();
  assert.deepEqual(run.requests, []);
  await screen.findByText(locale.deploy.action.distributedNodeCount.replace("{{count}}", "3"));
});

test("a group the server found nothing to place on is reported as the server says it, and nothing is sent", async () => {
  const none = { field: "placement_mode", code: "UNIT_PLACEMENT_NONE", params: { nodes: 5, unmet: 1, cordoned: 0, offline: 0, busy: 4, no_network: 0, largest: 1 } };
  const run = mount({ node_selection_mode: "multi", nnodes: 2, tp: 4 }, evaluated({ ok: false, preflight: undefined, errors: [none] }));
  await run.click();
  assert.deepEqual(run.requests, []);
  await screen.findByText(apiErrorText((key, options) => i18n.t(`common:${key}`, options), new APIError(none.code, none.params, "none", 409)));
});

test("a create the server refuses for lack of nodes shows its reason", async () => {
  const run = mount({ node_selection_mode: "pd", nnodes: 1, pd_mode: "vllm-nixl", pd_roles: pdRoles }, evaluated());
  api.createDeployment = (async () => { throw new APIError("UNIT_PLACEMENT_NONE", { nodes: 2, unmet: 0, cordoned: 0, offline: 0, busy: 2, no_network: 0, largest: 0 }, "none", 409); }) as never;
  await run.click();
  await waitFor(() => assert.ok(document.body.textContent?.includes("No group of nodes can take this deployment (of 2 nodes): 2 lack free GPUs")));
});
