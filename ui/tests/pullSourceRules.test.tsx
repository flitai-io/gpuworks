import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { installPullFake } from "./helpers/pullFake.ts";

const { i18n, locale } = await createI18n(["settings", "nodes"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { api } = await import("../src/api.ts");
const PullSourceRules = (await import("../src/pages/settings/pull/PullSourceRules.tsx")).default;

const text = locale.settings.pull.rules as Record<string, string>;
const action = locale.settings.pull.action as Record<string, string>;
// A button that just finished loading keeps antd's fading spinner (named "loading") in jsdom, where no transition ends.
const saveName = new RegExp(`^(loading)?\\s*${locale.settings.pull.action.save}$`);
const selector = locale.settings.pull.selector as Record<string, string>;
const fill = (template: string, values: Record<string, string | number>) => i18n.t(template.replace(/^/, ""), values);

const fakes: { restore(): void }[] = [];
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
});

const RULES = [
  { id: 1, match: "gpuworks/", endpoints: ["registry.zstack.ai/gpuworks/"], allow_upstream: true, note: "", node_requirements: {} },
  { id: 2, match: "docker.io", endpoints: ["harbor.corp.example/dockerhub-proxy", "mirror.example.com"], allow_upstream: false, note: "campus mirror", node_requirements: { site: ["idc-a"] } },
  { id: 3, match: "quay.io", endpoints: [], allow_upstream: true, note: "", node_requirements: { "gpuworks.ai/provider": ["runpod"] } },
];
const CREDENTIALS = [{ id: 7, host: "harbor.corp.example", username: "ci-pull", has_secret: true }];

function setup(initial: Parameters<typeof installPullFake>[1] = { rules: RULES, credentials: CREDENTIALS }) {
  const fake = installPullFake(api as unknown as Record<string, unknown>, initial);
  fakes.push(fake);
  render(<I18nextProvider i18n={i18n}><PullSourceRules /></I18nextProvider>);
  return { fake, user: userEvent.setup({ document: dom.window.document }) };
}
// Types a label into the selector and commits it with Enter (rc-select reads the legacy `which`).
async function pickLabel(user: ReturnType<typeof userEvent.setup>, combobox: HTMLElement, label: string) {
  await user.type(combobox, label);
  fireEvent.keyDown(combobox, { key: "Enter", keyCode: 13, which: 13 });
}
// antd renders the empty state as a body row (tr.ant-table-placeholder): counting it as a
// rule made "one row" true while a just-saved rule was still waiting for the refetch.
const rows = () => within(screen.getByRole("region", { name: text.tableLabel })).getAllByRole("row").slice(1).filter((row) => !row.classList.contains("ant-table-placeholder"));
const rowText = (row: HTMLElement) => row.textContent ?? "";

test("rules load with a spinner first, then show match, ordered endpoints, upstream, nodes and credential status", async () => {
  const fake = installPullFake(api as unknown as Record<string, unknown>, { rules: RULES, credentials: CREDENTIALS });
  fakes.push(fake);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  const inner = api.pullSourceRules;
  api.pullSourceRules = async () => { await gate; return inner(); };
  render(<I18nextProvider i18n={i18n}><PullSourceRules /></I18nextProvider>);
  assert.ok(document.querySelector(".ant-spin-spinning"), "a loading indicator shows while the rules load");
  release();
  await waitFor(() => assert.equal(rows().length, 3));
  const [first, second, third] = rows().map(rowText);
  assert.match(first, /^1/);
  assert.ok(first.includes("gpuworks/") && first.includes("registry.zstack.ai/gpuworks/"));
  assert.ok(first.includes(text.upstreamAllowed) && first.includes(text.allNodes));
  assert.ok(first.includes(`registry.zstack.ai: ${text.credentialsNone}`), "an unconfigured registry is stated neutrally");
  assert.ok(second.includes("docker.io") && second.includes("campus mirror"));
  assert.ok(second.indexOf("harbor.corp.example/dockerhub-proxy") < second.indexOf("mirror.example.com"), "endpoints keep their order");
  assert.ok(second.includes(text.upstreamOff));
  assert.ok(second.includes("site=idc-a") && second.includes(i18n.t("pull.rules.matchedNodes", { ns: "settings", count: 3 })));
  assert.ok(second.includes(`harbor.corp.example: ${text.credentialsSet}`), "a host with a stored login says so");
  assert.ok(third.includes(text.noEndpoints) && third.includes("gpuworks.ai/provider=runpod"));
});

test("no rules shows the empty state", async () => {
  setup({ rules: [] });
  await screen.findByText(text.empty);
});

test("a failed load explains itself and can be retried", async () => {
  const fake = installPullFake(api as unknown as Record<string, unknown>, { rules: RULES });
  fakes.push(fake);
  fake.failNext("pullSourceRules", new Error("boom"));
  render(<I18nextProvider i18n={i18n}><PullSourceRules /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(i18n.t("pull.loadFailed", { ns: "settings", detail: "boom" }));
  await user.click(screen.getByRole("button", { name: action.retry }));
  await waitFor(() => assert.equal(rows().length, 3));
  assert.equal(screen.queryByText(/boom/), null);
});

test("creating a rule sends the drawer's fields and shows the live node match", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 3));
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.match), "ghcr.io");
  await user.type(within(drawer).getByLabelText(fill(text.endpointN, { n: 1 })), "harbor.corp.example/ghcr");
  await user.click(within(drawer).getByRole("button", { name: text.addEndpoint }));
  await user.type(within(drawer).getByLabelText(fill(text.endpointN, { n: 2 })), "mirror.example.com");
  // The second endpoint moves above the first.
  await user.click(within(drawer).getByRole("button", { name: fill(text.endpointUp, { n: 2 }) }));
  assert.equal((within(drawer).getByLabelText(fill(text.endpointN, { n: 1 })) as HTMLInputElement).value, "mirror.example.com");
  assert.ok(within(drawer).getByText(i18n.t("pull.selector.allNodes", { ns: "settings", count: 6 })));
  // Turning upstream off warns that a total failure fails the pull.
  await user.click(within(drawer).getByRole("switch", { name: text.allowUpstream }));
  assert.ok(within(drawer).getByText(text.noUpstreamWarning));
  // A node selector narrows the rule and the server's count follows.
  const nodes = within(drawer).getByRole("combobox", { name: text.nodes });
  await pickLabel(user, nodes, "site=idc-a");
  await waitFor(() => within(drawer).getByText(i18n.t("pull.selector.matches", { ns: "settings", count: 3, total: 6 })));
  await user.type(within(drawer).getByLabelText(text.note), "github mirror");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(rows().length, 4));
  const create = fake.calls.find((call) => call.method === "createPullSourceRule");
  assert.deepEqual(create?.args[0], {
    match: "ghcr.io",
    endpoints: ["mirror.example.com", "harbor.corp.example/ghcr"],
    allow_upstream: false,
    note: "github mirror",
    node_requirements: { site: ["idc-a"] },
  });
  assert.equal(screen.queryByRole("dialog"), null, "the drawer closes after saving");
});

test("a refusal from the server is shown at its field and the drawer stays open for a fix", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 3));
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  const field = i18n.t("pull.fields.match", { ns: "settings" });
  await within(drawer).findByText(i18n.t("pull.invalidField", { ns: "settings", field, detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.equal(fake.rules.length, 3, "nothing was created");
  await user.type(within(drawer).getByLabelText(text.match), "ghcr.io");
  await user.type(within(drawer).getByLabelText(fill(text.endpointN, { n: 1 })), "bad address");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await within(drawer).findByText(i18n.t("pull.invalidField", { ns: "settings", field: i18n.t("pull.fields.endpoints", { ns: "settings" }), detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.ok(screen.getByRole("dialog"), "still open");
});

test("an edit starts from the stored rule and replaces it", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 3));
  await user.click(within(rows()[1]).getByRole("button", { name: action.edit }));
  const drawer = await screen.findByRole("dialog");
  assert.equal((within(drawer).getByLabelText(text.match) as HTMLInputElement).value, "docker.io");
  assert.equal((within(drawer).getByLabelText(fill(text.endpointN, { n: 2 })) as HTMLInputElement).value, "mirror.example.com");
  assert.equal(within(drawer).getByRole("switch", { name: text.allowUpstream }).getAttribute("aria-checked"), "false");
  await user.click(within(drawer).getByRole("button", { name: fill(text.endpointRemove, { n: 2 }) }));
  await user.click(within(drawer).getByRole("switch", { name: text.allowUpstream }));
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  const update = fake.calls.find((call) => call.method === "updatePullSourceRule");
  assert.equal(update?.args[0], 2);
  assert.deepEqual(update?.args[1], {
    match: "docker.io", endpoints: ["harbor.corp.example/dockerhub-proxy"], allow_upstream: true, note: "campus mirror", node_requirements: { site: ["idc-a"] },
  });
});

test("moving a rule up sends the whole new order and shows it", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 3));
  assert.equal((screen.getByRole("button", { name: fill(text.moveUp, { match: "gpuworks/" }) }) as HTMLButtonElement).disabled, true, "the first rule cannot move up");
  assert.equal((screen.getByRole("button", { name: fill(text.moveDown, { match: "quay.io" }) }) as HTMLButtonElement).disabled, true, "the last rule cannot move down");
  await user.click(screen.getByRole("button", { name: fill(text.moveUp, { match: "quay.io" }) }));
  await waitFor(() => assert.ok(rowText(rows()[1]).includes("quay.io")));
  assert.deepEqual(fake.calls.find((call) => call.method === "orderPullSourceRules")?.args[0], [1, 3, 2]);
  assert.deepEqual(rows().map((row) => rowText(row).slice(0, 1)), ["1", "2", "3"]);
});

test("a refused reorder keeps the list as it was and says why", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 3));
  const { APIError } = await import("../src/apiError.ts");
  fake.failNext("orderPullSourceRules", new APIError("PULL_SETTING_INVALID", { field: "order", detail: "rules changed meanwhile" }, "x", 400));
  await user.click(screen.getByRole("button", { name: fill(text.moveDown, { match: "gpuworks/" }) }));
  await screen.findByText(i18n.t("pull.invalidField", { ns: "settings", field: i18n.t("pull.fields.order", { ns: "settings" }), detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.ok(rowText(rows()[0]).includes("gpuworks/"));
});

test("deleting asks first: cancel keeps the rule, confirm removes it", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 3));
  await user.click(screen.getByRole("button", { name: fill(text.deleteRule, { match: "quay.io" }) }));
  const confirm = await screen.findByText(text.deleteConfirm);
  assert.ok(confirm);
  await user.click(screen.getByRole("button", { name: action.cancel }));
  assert.equal(fake.calls.filter((call) => call.method === "deletePullSourceRule").length, 0);
  assert.equal(rows().length, 3);
  await user.click(screen.getByRole("button", { name: fill(text.deleteRule, { match: "quay.io" }) }));
  await user.click(await screen.findByRole("button", { name: action.delete, hidden: false }).then(() => {
    const popover = document.querySelector(".ant-popconfirm") as HTMLElement;
    return within(popover).getByRole("button", { name: action.delete });
  }));
  await waitFor(() => assert.equal(rows().length, 2));
  assert.deepEqual(fake.calls.find((call) => call.method === "deletePullSourceRule")?.args, [3]);
});

test("Escape closes the drawer without saving and Enter in the match field saves", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 3));
  await user.click(screen.getByRole("button", { name: text.new }));
  let drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.match), "ghcr.io");
  fireEvent.keyDown(within(drawer).getByLabelText(text.match), { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal(fake.calls.filter((call) => call.method === "createPullSourceRule").length, 0);

  await user.click(screen.getByRole("button", { name: text.new }));
  drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.match), "ghcr.io{Enter}");
  await waitFor(() => assert.equal(fake.calls.filter((call) => call.method === "createPullSourceRule").length, 1));
  await waitFor(() => assert.equal(rows().length, 4));
});

test("the selector says so when no node carries the picked labels", async () => {
  const { user } = setup({ rules: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  await pickLabel(user, within(drawer).getByRole("combobox", { name: text.nodes }), "site=nowhere");
  await within(drawer).findByText(selector.noMatch);
});

test("a stored login lookup that fails leaves the credential column blank rather than claiming no credentials", async () => {
  const fake = installPullFake(api as unknown as Record<string, unknown>, { rules: RULES, credentials: CREDENTIALS });
  fakes.push(fake);
  fake.failNext("credentials", new Error("forbidden"));
  render(<I18nextProvider i18n={i18n}><PullSourceRules /></I18nextProvider>);
  await waitFor(() => assert.equal(rows().length, 3));
  assert.ok(!rowText(rows()[0]).includes(text.credentialsNone));
});

const SITE_RULE = { id: 4, match: "https://huggingface.co", endpoints: ["https://hf-mirror.com"], allow_upstream: true, control_plane: true, note: "", node_requirements: {} };

test("the type column tells image rules from model site rules and marks sites the management node uses", async () => {
  setup({ rules: [...RULES, SITE_RULE] });
  await waitFor(() => assert.equal(rows().length, 4));
  assert.ok(rowText(rows()[0]).includes(text.kindImage) && !rowText(rows()[0]).includes(text.kindUrl));
  const site = rowText(rows()[3]);
  assert.ok(site.includes(text.kindUrl) && site.includes("https://huggingface.co") && site.includes(text.controlPlaneMark));
});

test("a new rule chooses its type first: image has no management option, a model site does and defaults it on", async () => {
  const { fake, user } = setup({ rules: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  const type = within(drawer).getByRole("radiogroup", { name: text.type });
  assert.equal((within(type).getByRole("radio", { name: text.kindImage }) as HTMLInputElement).checked, true);
  assert.equal(within(drawer).queryByRole("checkbox", { name: text.controlPlane }), null, "image rules cannot serve the management node");
  assert.equal(within(drawer).getByLabelText(text.match).getAttribute("placeholder"), text.matchPlaceholder);
  assert.ok(within(drawer).getByText(text.docsHint));
  assert.equal(within(drawer).queryByRole("link"), null, "the docs hint is plain text");

  await user.click(within(type).getByRole("radio", { name: text.kindUrl }));
  assert.equal(within(drawer).getByLabelText(text.match).getAttribute("placeholder"), text.matchPlaceholderUrl);
  assert.ok(within(drawer).getByText(text.matchHelpUrl));
  const control = within(drawer).getByRole("checkbox", { name: text.controlPlane }) as HTMLInputElement;
  assert.equal(control.checked, true);
  assert.ok(within(drawer).getByText(text.controlPlaneHelp));
  await user.type(within(drawer).getByLabelText(text.match), "https://huggingface.co");
  await user.type(within(drawer).getByLabelText(fill(text.endpointN, { n: 1 })), "https://hf-mirror.com");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.deepEqual(fake.calls.find((call) => call.method === "createPullSourceRule")?.args[0], {
    match: "https://huggingface.co", endpoints: ["https://hf-mirror.com"], allow_upstream: true, control_plane: true, note: "", node_requirements: {},
  });
  assert.ok(rowText(rows()[0]).includes(text.kindUrl));
});

test("a site rule narrowed to some nodes starts without the management mark, and the choice can be turned off", async () => {
  const { fake, user } = setup({ rules: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  await user.click(within(drawer).getByRole("radio", { name: text.kindUrl }));
  await pickLabel(user, within(drawer).getByRole("combobox", { name: text.nodes }), "site=idc-a");
  await waitFor(() => assert.equal((within(drawer).getByRole("checkbox", { name: text.controlPlane }) as HTMLInputElement).checked, false));
  await user.click(within(drawer).getByRole("checkbox", { name: text.controlPlane }));
  await user.click(within(drawer).getByRole("checkbox", { name: text.controlPlane }));
  await user.type(within(drawer).getByLabelText(text.match), "https://huggingface.co");
  await user.type(within(drawer).getByLabelText(fill(text.endpointN, { n: 1 })), "https://hf-mirror.com");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.equal((fake.calls.find((call) => call.method === "createPullSourceRule")?.args[0] as { control_plane: boolean }).control_plane, false);
});

test("editing a rule keeps its type fixed and starts from the stored management setting", async () => {
  const { fake, user } = setup({ rules: [{ ...SITE_RULE, control_plane: false }, RULES[0]] });
  await waitFor(() => assert.equal(rows().length, 2));
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  let drawer = await screen.findByRole("dialog");
  const type = within(drawer).getByRole("radiogroup", { name: text.type });
  assert.equal((within(type).getByRole("radio", { name: text.kindUrl }) as HTMLInputElement).checked, true);
  assert.equal((within(type).getByRole("radio", { name: text.kindImage }) as HTMLInputElement).disabled, true);
  assert.equal((within(drawer).getByRole("checkbox", { name: text.controlPlane }) as HTMLInputElement).checked, false, "an edit never re-applies the new-rule default");
  await user.click(within(drawer).getByRole("checkbox", { name: text.controlPlane }));
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal((fake.calls.find((call) => call.method === "updatePullSourceRule")?.args[1] as { control_plane: boolean }).control_plane, true);

  await user.click(within(rows()[1]).getByRole("button", { name: action.edit }));
  drawer = await screen.findByRole("dialog");
  assert.equal((within(drawer).getByRole("radio", { name: text.kindImage }) as HTMLInputElement).checked, true);
  assert.equal(within(drawer).queryByRole("checkbox", { name: text.controlPlane }), null);
});

test("a refused control_plane is shown at its checkbox", async () => {
  const { fake, user } = setup({ rules: [] });
  await screen.findByText(text.empty);
  fake.failNext("createPullSourceRule", new (await import("../src/apiError.ts")).APIError("PULL_SETTING_INVALID", { field: "control_plane", detail: "control_plane applies to model site rules only" }, "x", 400));
  await user.click(screen.getByRole("button", { name: text.new }));
  const drawer = await screen.findByRole("dialog");
  await user.click(within(drawer).getByRole("radio", { name: text.kindUrl }));
  await user.type(within(drawer).getByLabelText(text.match), "https://huggingface.co");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await within(drawer).findByText(i18n.t("pull.invalidField", { ns: "settings", field: i18n.t("pull.fields.control_plane", { ns: "settings" }), detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.ok(screen.getByRole("dialog"));
});
