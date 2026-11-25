import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { FLEET, installPullFake } from "./helpers/pullFake.ts";

const { i18n, locale } = await createI18n(["settings", "nodes"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const PullProxies = (await import("../src/pages/settings/pull/PullProxies.tsx")).default;

const text = locale.settings.pull.proxies as Record<string, string>;
const rules = locale.settings.pull.rules as Record<string, string>;
const action = locale.settings.pull.action as Record<string, string>;
const saveName = new RegExp(`^(loading)?\\s*${action.save}$`);
const fill = (template: string, values: Record<string, string | number>) => template.replace(/\{\{(\w+)\}\}/g, (_, key) => String(values[key]));

const fakes: { restore(): void }[] = [];
const originals = { nodesPage: api.nodesPage, node: api.node };
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
  Object.assign(api, originals);
});

const PROXIES = [
  { id: 1, http_proxy: "http://proxy-a.corp:3128", https_proxy: "http://proxy-a.corp:3128", no_proxy: "10.0.0.0/8", username: "svc", password: "s3cret", control_plane: true, note: "campus", node_requirements: { site: ["idc-a"] } },
  { id: 2, http_proxy: "", https_proxy: "http://proxy-b.corp:8080", no_proxy: "", username: "", control_plane: false, note: "", node_requirements: {} },
];

function setup(initial: Parameters<typeof installPullFake>[1] = { proxies: PROXIES }, beforeRender?: (fake: ReturnType<typeof installPullFake>) => void) {
  const fake = installPullFake(api as unknown as Record<string, unknown>, initial);
  fakes.push(fake);
  beforeRender?.(fake);
  const nodes = FLEET.map((node) => ({ uuid: node.name, hostname: node.name, name: "" }));
  api.nodesPage = (async () => ({ items: nodes, total: nodes.length, all: nodes.length, labels: [] })) as never;
  api.node = (async (uuid: string) => nodes.find((node) => node.uuid === uuid)) as never;
  render(<I18nextProvider i18n={i18n}><PullProxies /></I18nextProvider>);
  return { fake, user: userEvent.setup({ document: dom.window.document }) };
}
// AntD renders the empty state as a body row (.ant-table-placeholder); it is not an entry, and counting it let
// a wait for "one entry" pass before the created entry arrived.
const rows = () => within(screen.getByRole("region", { name: text.tableLabel })).getAllByRole("row").slice(1)
  .filter((row) => !row.classList.contains("ant-table-placeholder"));
const rowText = (row: HTMLElement) => row.textContent ?? "";
const checkbox = (scope: HTMLElement, name: string) => within(scope).getByRole("checkbox", { name }) as HTMLInputElement;

test("proxies load with a spinner, then show addresses, user with a password mark, management use, nodes and note", async () => {
  const fake = installPullFake(api as unknown as Record<string, unknown>, { proxies: PROXIES });
  fakes.push(fake);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const inner = api.pullProxies;
  api.pullProxies = async () => { await gate; return inner(); };
  render(<I18nextProvider i18n={i18n}><PullProxies /></I18nextProvider>);
  assert.ok(document.querySelector(".ant-spin-spinning"), "a loading indicator shows while the proxies load");
  release();
  await waitFor(() => assert.equal(rows().length, 2));
  const [first, second] = rows().map(rowText);
  assert.match(first, /^1/);
  assert.ok(first.includes("http://proxy-a.corp:3128") && first.includes("10.0.0.0/8"));
  assert.ok(first.includes("svc") && first.includes(text.passwordSet), "the user and the password mark show");
  assert.ok(!first.includes("s3cret"), "the password itself never shows");
  assert.ok(first.includes(text.controlPlane) && first.includes("campus") && first.includes("site=idc-a"));
  assert.ok(first.includes(fill(text.matchedNodes, { count: 3 })));
  assert.ok(second.includes("http://proxy-b.corp:8080") && second.includes(text.allNodes));
  assert.ok(!second.includes(text.passwordSet) && !second.includes(text.controlPlane));
});

test("no proxies shows the empty state", async () => {
  setup({ proxies: [] });
  await screen.findByText(text.empty);
});

test("a failed load explains itself and can be retried", async () => {
  const { user } = setup({ proxies: PROXIES }, (fake) => fake.failNext("pullProxies", new Error("boom")));
  await screen.findByText(i18n.t("pull.loadFailed", { ns: "settings", detail: "boom" }));
  await user.click(screen.getByRole("button", { name: action.retry }));
  await waitFor(() => assert.equal(rows().length, 2));
  assert.equal(screen.queryByText(/boom/), null);
});

test("creating an entry sends the drawer's fields with the password and defaults the management mark on", async () => {
  const { fake, user } = setup({ proxies: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  assert.equal(checkbox(drawer, text.controlPlaneLabel).checked, true, "a new entry without node requirements serves the management node");
  await user.type(within(drawer).getByLabelText(text.httpProxy), "http://proxy.corp:3128");
  await user.type(within(drawer).getByLabelText(text.noProxy), "localhost,.corp");
  await user.type(within(drawer).getByLabelText(text.username), "svc");
  await user.type(within(drawer).getByLabelText(text.password), "pw1");
  assert.equal((within(drawer).getByLabelText(text.password) as HTMLInputElement).type, "password");
  await user.type(within(drawer).getByLabelText(text.note), "main proxy");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.deepEqual(fake.calls.find((call) => call.method === "createPullProxy")?.args[0], {
    http_proxy: "http://proxy.corp:3128", https_proxy: "", no_proxy: "localhost,.corp", username: "svc", password: "pw1",
    control_plane: true, note: "main proxy", node_requirements: {},
  });
  assert.ok(rowText(rows()[0]).includes(text.passwordSet));
  assert.equal(screen.queryByRole("dialog"), null);
});

test("a refusal is shown at its field and the drawer stays open", async () => {
  const { fake, user } = setup({ proxies: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  const field = i18n.t("pull.fields.http_proxy", { ns: "settings" });
  await within(drawer).findByText(i18n.t("pull.invalidField", { ns: "settings", field, detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  await user.type(within(drawer).getByLabelText(text.httpProxy), "http://u:p@proxy.corp:3128");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await within(drawer).findByText(i18n.t("pull.invalidField", { ns: "settings", field: i18n.t("pull.fields.http_proxy", { ns: "settings" }), detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.equal(fake.proxies.length, 0, "nothing was created");
  assert.ok(screen.getByRole("dialog"), "still open");
});

test("editing keeps the stored password when the field stays empty", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  const drawer = await screen.findByRole("dialog");
  assert.equal((within(drawer).getByLabelText(text.password) as HTMLInputElement).value, "", "the stored password is never filled in");
  assert.ok(within(drawer).getByText(text.passwordKeepHelp));
  assert.equal((within(drawer).getByLabelText(text.httpProxy) as HTMLInputElement).value, "http://proxy-a.corp:3128");
  await user.clear(within(drawer).getByLabelText(text.note));
  await user.type(within(drawer).getByLabelText(text.note), "changed");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  const update = fake.calls.find((call) => call.method === "updatePullProxy");
  assert.equal(update?.args[0], 1);
  assert.equal("password" in (update?.args[1] as object), false, "an omitted password keeps the stored one");
  assert.equal(fake.proxies[0].password, "s3cret");
  assert.ok(rowText(rows()[0]).includes("changed") && rowText(rows()[0]).includes(text.passwordSet));
});

test("clearing the stored password sends an empty password", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  const drawer = await screen.findByRole("dialog");
  await user.click(checkbox(drawer, text.clearPassword));
  assert.ok(within(drawer).getByText(text.passwordClearedHelp));
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal((fake.calls.find((call) => call.method === "updatePullProxy")?.args[1] as { password?: string }).password, "");
  assert.equal(fake.proxies[0].password, "");
  await waitFor(() => assert.ok(!rowText(rows()[0]).includes(text.passwordSet)));
});

test("setting a new password replaces the stored one", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.password), "newpw");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal(fake.proxies[0].password, "newpw");
});

test("moving an entry sends the whole new order and a refused reorder says why", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  assert.equal((screen.getByRole("button", { name: fill(text.moveUp, { n: 1 }) }) as HTMLButtonElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: fill(text.moveDown, { n: 2 }) }) as HTMLButtonElement).disabled, true);
  fake.failNext("orderPullProxies", new APIError("PULL_SETTING_INVALID", { field: "order", detail: "proxies changed meanwhile" }, "x", 400));
  await user.click(screen.getByRole("button", { name: fill(text.moveUp, { n: 2 }) }));
  await screen.findByText(i18n.t("pull.invalidField", { ns: "settings", field: i18n.t("pull.fields.order", { ns: "settings" }), detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.ok(rowText(rows()[0]).includes("proxy-a.corp"), "the list is as it was");
  await user.click(screen.getByRole("button", { name: fill(text.moveUp, { n: 2 }) }));
  await waitFor(() => assert.ok(rowText(rows()[0]).includes("proxy-b.corp")));
  assert.deepEqual(fake.calls.filter((call) => call.method === "orderPullProxies")[1].args[0], [2, 1]);
});

test("deleting asks first: cancel keeps the entry, confirm removes it", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(screen.getByRole("button", { name: fill(text.deleteProxy, { n: 2 }) }));
  await screen.findByText(text.deleteConfirm);
  await user.click(screen.getByRole("button", { name: action.cancel }));
  assert.equal(fake.calls.filter((call) => call.method === "deletePullProxy").length, 0);
  assert.equal(rows().length, 2);
  await user.click(screen.getByRole("button", { name: fill(text.deleteProxy, { n: 2 }) }));
  await screen.findByText(text.deleteConfirm);
  const popover = document.querySelector(".ant-popconfirm") as HTMLElement;
  await user.click(within(popover).getByRole("button", { name: action.delete }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.deepEqual(fake.calls.find((call) => call.method === "deletePullProxy")?.args, [2]);
});

test("Escape closes the drawer without saving", async () => {
  const { fake, user } = setup({ proxies: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.httpProxy), "http://p:1");
  fireEvent.keyDown(within(drawer).getByLabelText(text.httpProxy), { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal(fake.calls.filter((call) => call.method === "createPullProxy").length, 0);
});

test("the preview names the entry a node uses, or says it uses none", async () => {
  const { fake, user } = setup({
    proxies: [{ id: 1, http_proxy: "http://proxy-a.corp:3128", https_proxy: "", no_proxy: "", username: "", control_plane: false, note: "", node_requirements: { site: ["idc-a"] } }],
  });
  await waitFor(() => assert.equal(rows().length, 1));
  assert.ok(screen.getByText(text.previewTitle));
  const picker = screen.getByRole("combobox", { name: text.previewPick });
  await user.click(picker);
  await user.click(await screen.findByRole("option", { name: "gpu-a-01" }));
  await screen.findByText(fill(text.previewResult, { n: 1 }));
  assert.deepEqual(fake.calls.find((call) => call.method === "nodePullProxy")?.args, ["gpu-a-01"]);
  await user.click(picker);
  await user.click(await screen.findByRole("option", { name: "gpu-b-01" }));
  await screen.findByText(text.previewNone);
  assert.equal(screen.queryByText(fill(text.previewResult, { n: 1 })), null);
});

test("a failed preview says so", async () => {
  const { fake, user } = setup({ proxies: [] });
  await screen.findByText(text.empty);
  fake.failNext("nodePullProxy", new Error("node gone"));
  await user.click(screen.getByRole("combobox", { name: text.previewPick }));
  await user.click(await screen.findByRole("option", { name: "gpu-a-01" }));
  await screen.findByText(fill(text.previewFailed, { detail: "node gone" }));
});

test("the node selector in the drawer narrows an entry and clears the default management mark's basis", async () => {
  const { fake, user } = setup({ proxies: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  const combo = within(drawer).getByRole("combobox", { name: rules.nodes });
  await user.type(combo, "site=idc-a");
  fireEvent.keyDown(combo, { key: "Enter", keyCode: 13, which: 13 });
  await waitFor(() => assert.equal(checkbox(drawer, text.controlPlaneLabel).checked, false));
  await user.type(within(drawer).getByLabelText(text.httpsProxy), "http://p:1");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(rows().length, 1));
  const body = fake.calls.find((call) => call.method === "createPullProxy")?.args[0] as { control_plane: boolean; node_requirements: unknown };
  assert.deepEqual(body.node_requirements, { site: ["idc-a"] });
  assert.equal(body.control_plane, false);
});

// DELETE keeps surviving positions (TestPullProxiesCRUDWire).
test("a refused proxy deletion preserves the current list numbers", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  fake.failNext("deletePullProxy", new Error("delete refused"));
  await user.click(screen.getByRole("button", { name: fill(text.deleteProxy, { n: 1 }) }));
  await user.click(await screen.findByRole("button", { name: action.delete, exact: true }));
  await screen.findByText("delete refused");
  assert.equal(fake.proxies.length, 2);
  assert.match(rowText(rows()[1]), /^2/);
});

test("deleting an earlier proxy keeps list, preview and node card numbers consistent", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 2));
  const originalDelete = api.deletePullProxy;
  let release!: () => void;
  api.deletePullProxy = async (id) => { await new Promise<void>(resolve => { release = resolve; }); return originalDelete(id); };
  try {
    await user.click(screen.getByRole("button", { name: fill(text.deleteProxy, { n: 1 }) }));
    await user.click(await screen.findByRole("button", { name: action.delete, exact: true }));
    await waitFor(() => assert.ok(release));
    assert.match(rowText(rows()[1]), /^2/, "a pending deletion preserves the surviving number");
    release();
    await waitFor(() => assert.equal(rows().length, 1));
    assert.equal(fake.proxies[0].position, 1);
    assert.match(rowText(rows()[0]), /^2/, "the list must use the server position after deletion");
    assert.ok(within(rows()[0]).getByRole("button", { name: fill(text.deleteProxy, { n: 2 }) }));
    assert.ok(within(rows()[0]).getByRole("button", { name: fill(text.moveUp, { n: 2 }) }));
    await user.click(screen.getByRole("combobox", { name: text.previewPick }));
    await user.click(await screen.findByRole("option", { name: "gpu-b-01" }));
    await screen.findByText(fill(text.previewResult, { n: 2 }));
    const { MemoryRouter } = await import("react-router-dom");
    const NodePullProxy = (await import("../src/components/NodePullProxy.tsx")).default;
    render(<MemoryRouter><I18nextProvider i18n={i18n}><NodePullProxy nodeUUID="gpu-b-01" /></I18nextProvider></MemoryRouter>);
    await screen.findByText(i18n.t("pullProxy.entry", { ns: "nodes", n: 2 }));
  } finally { api.deletePullProxy = originalDelete; }
});
