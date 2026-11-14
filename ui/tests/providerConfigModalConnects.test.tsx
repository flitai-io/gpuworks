import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { aliyunKind, capabilities, kindCategories, runpodKind, zstackKind, KUBE_DEFAULTS } from "./helpers/platformFixtures.ts";
import { describedBy, fieldLabels as labelsIn, footerButtons as buttonsIn, freshPopupFramesPerTest, platformChoice } from "./helpers/platformDialog.ts";

// The Add External Platform dialog's choice of platform
// (ui/src/components/ProviderConfigModal.tsx): the platform types the server
// files the kinds under as tabs, a Kubernetes cluster among them for
// whoever may connect one, then the vendors of the chosen type in a list, and
// what the chosen platform connects. The Kubernetes tab puts the cluster form
// in place of the vendor's; what that form does is in
// tests/kubeClusterForm.test.tsx.

const languages = { en: await createI18n(["providers", "kubernetes"], "en"), zh: await createI18n(["providers", "kubernetes"], "zh") };
const p = languages.en.locale.providers;
const k = languages.en.locale.kubernetes;
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { api } = await import("../src/api.ts");
type KindInfo = import("../src/api.ts").ProviderKindInfo;
type CategoryInfo = import("../src/api.ts").ProviderCategoryInfo;
type Provider = import("../src/api.ts").CloudProvider;
type Config = import("../src/api.ts").ProviderEditConfig;

const apiOriginal = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { cleanup(); Object.assign(api, apiOriginal); });

const credential_fields = [{ name: "api_key", label: { en: "API Key", zh: "API Key" }, secret: true, required: true }];
// As GET /api/v1/providers/kinds answers, in the server's order: every kind
// carries its platform type and its resources; a kind that declares neither
// carries an empty type and an empty list.
const aliyun: KindInfo = { ...aliyunKind, credential_fields };
const runpod: KindInfo = { ...runpodKind, credential_fields };
const zstack: KindInfo = { ...zstackKind, credential_fields };
const digitalocean: KindInfo = {
  kind: "digitalocean", label: { en: "DigitalOcean", zh: "DigitalOcean" }, category: "public_cloud", capabilities, credential_fields, server_dial_supported: false,
  can_list_clusters: true, resources: [{ type: "vm", product: "Droplet" }, { type: "kubernetes", product: "DOKS" }],
};
const newcloud: KindInfo = { kind: "newcloud", label: { en: "New Cloud", zh: "新云" }, category: "", capabilities, credential_fields, server_dial_supported: false, resources: [] };
const kindInfos = [zstack, aliyun, newcloud, runpod, digitalocean];

// The platform types come with the kinds, in GET /api/v1/providers/kinds' categories; a test may give its own.
function mount(options: { language?: keyof typeof languages; defaultKind?: string; provider?: Provider; kinds?: KindInfo[]; categories?: CategoryInfo[]; canConnectCluster?: boolean; onClose?: () => void } = {}) {
  // The cluster form asks the server for its defaults when Kubernetes is chosen.
  api.kubeDefaults = async () => KUBE_DEFAULTS;
  render(
    <I18nextProvider i18n={languages[options.language ?? "en"].i18n}>
      <ProviderConfigModal open provider={options.provider} defaultKind={options.defaultKind ?? null} kindInfos={options.kinds ?? kindInfos} categories={options.categories ?? kindCategories(options.kinds ?? kindInfos)} providerLabel="External Platform"
        onClose={options.onClose ?? (() => {})} onSaved={() => {}} canConnectCluster={options.canConnectCluster} />
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

// The platform choice: the types as tabs ("Platform type", 平台类型), then the chosen type's vendors in a combobox named by the
// field's label ("External platform", 外部平台); closed, it shows the chosen platform.
const zhp = languages.zh.locale.providers;
// The tab that is not a vendors' type: a cluster connected on its own, with kubectl.
const K8S = "Kubernetes cluster";
const K8S_ZH = "Kubernetes 集群";
const choice = { en: platformChoice(p.config.type, p.table.column.category), zh: platformChoice(zhp.config.type, zhp.table.column.category) };
const selectedLabel = () => screen.getByRole("combobox", { name: /^(External platform|外部平台)$/ }).textContent ?? "";
/** The tabs of the platform types, each with whether it is the selected one. */
const typeTabs = () => within(screen.getByRole("tablist")).getAllByRole("tab").map((tab) => [tab.textContent, tab.getAttribute("aria-selected") === "true"]);
/** The platform comboboxes in the dialog: one for a vendors' type, none for Kubernetes. */
const platformSelects = () => screen.queryAllByRole("combobox", { name: /^(External platform|外部平台)$/ });

/** What the list named by the given label holds, line by line: a resource's tag and the sentence beside it, or a note alone. */
function connects(label: string): string[][] {
  return within(screen.getByRole("list", { name: label })).getAllByRole("listitem").map((item) => item.children.length ? [...item.children].map((part) => part.textContent ?? "") : [item.textContent ?? ""]);
}
/** What each tag of that list says when pointed at: the kind of resource a product name stands for; nothing when the tag is the kind itself. */
function tagTitles(label: string): Array<string | null> {
  return [...screen.getByRole("list", { name: label }).querySelectorAll('[data-slot="badge"]')].map((tag) => tag.getAttribute("title"));
}
const dialog = () => screen.getByRole("dialog");
/** The names of the buttons in the dialog's footer, left to right. */
const footerButtons = () => buttonsIn(dialog());
/** The labels of the fields the dialog shows, without their required marks. */
const fieldLabels = () => labelsIn(dialog());
// No vendor among them: each node of the cluster reports the vendor of its own cards.
const CLUSTER_FIELDS = [k.drawer.name, k.drawer.serverUrl, k.drawer.nodeSelector];

test("adding a platform lists what its kind connects, one tag per resource named by the vendor's product and how it becomes compute nodes, and follows the chosen kind", async () => {
  const how = p.config.how;
  const user = mount({ defaultKind: "aliyun" });
  // The tag is the vendor's product name alone; what kind of resource that is, is said when it is pointed at.
  await waitFor(() => assert.deepEqual(connects("It connects"), [["ECS", how.vm], ["ACK", how.kubernetes]]));
  assert.deepEqual(tagTitles("It connects"), ["VM", "Kubernetes"]);

  // A resource the vendor has no product name for is the console's word alone, with nothing more to say on hover.
  await choice.en.pickType(user, "Private cloud");
  await waitFor(() => assert.deepEqual(connects("It connects"), [["Host", how.host]]));
  assert.deepEqual(tagTitles("It connects"), [null]);

  // A kind that declares no resources has nothing to say: the row is gone, the form stays.
  await choice.en.pickType(user, "Other");
  await waitFor(() => assert.equal(screen.queryAllByText("It connects").length, 0));
  assert.equal(screen.queryAllByRole("list").length, 0);
  assert.ok(screen.getByText("API Key"));

  // Another vendor of the same type, from the list.
  await choice.en.pickType(user, "Public cloud");
  await choice.en.pick(user, "RunPod");
  await waitFor(() => assert.deepEqual(connects("It connects"), [["Pod", how.container]]));
  assert.deepEqual(tagTitles("It connects"), ["Container"]);
});

test("the platform types are tabs in the order the server lists them: public clouds, then private clouds, then the kinds that declare none under Other; a tab is its type's first platform", async () => {
  const user = mount();
  // Opened on no kind in particular, the dialog is on the first kind the server lists, and on the tab of its type.
  await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
  assert.deepEqual(typeTabs(), [["Public cloud", false], ["Private cloud", true], ["Other", false]]);
  // The two steps are one field: one label, which names the list; the tabs are named for what they are.
  assert.deepEqual(fieldLabels().slice(0, 1), [p.config.type]);
  assert.equal(screen.getByRole("tablist").getAttribute("aria-label"), "Platform type");

  // A tab is its type's first platform, in the server's order, and exactly one tab is the selected one.
  await choice.en.pickType(user, "Public cloud");
  assert.equal(selectedLabel(), "Alibaba Cloud");
  assert.deepEqual(typeTabs(), [["Public cloud", true], ["Private cloud", false], ["Other", false]]);
  await choice.en.pickType(user, "Other");
  assert.equal(selectedLabel(), "New Cloud");
  assert.deepEqual(typeTabs(), [["Public cloud", false], ["Private cloud", false], ["Other", true]]);
  await choice.en.pickType(user, "Private cloud");
  assert.equal(selectedLabel(), "ZStack Cloud");

  // A vendor picked from the list stays on its type's tab.
  await choice.en.pickType(user, "Public cloud");
  await choice.en.pick(user, "DigitalOcean");
  assert.equal(selectedLabel(), "DigitalOcean");
  assert.equal(await choice.en.chosenType(), "Public cloud");
});

// What the list offers after a tab is chosen: each case opens the dialog on a private cloud and goes to the tab.
for (const [type, vendors] of [["Public cloud", ["Alibaba Cloud", "RunPod", "DigitalOcean"]], ["Other", ["New Cloud"]], ["Private cloud", ["ZStack Cloud"]]] as const) {
  test(`on the ${type} tab the list offers that type's vendors and no other, in the server's order, without group titles`, async () => {
    const user = mount();
    await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
    await choice.en.pickType(user, type);
    assert.deepEqual(await choice.en.choices(user), [...vendors]);
    assert.equal(selectedLabel(), vendors[0], "reading the list chooses nothing");
  });
}

// Once the vendors' list has been opened its popup stays mounted. A list whose options are replaced under it resets its
// value to the one it started with, or to none; the dialog must not take that for a choice of platform. The two cases
// below failed that way before each type got a list of its own: the tab snapped back, or the platform became none.
test("after a vendor was picked from the list, a tab still moves to its type's first platform, and back", async () => {
  const user = mount({ defaultKind: "aliyun", canConnectCluster: true });
  await waitFor(() => assert.equal(selectedLabel(), "Alibaba Cloud"));
  await choice.en.pick(user, "RunPod");
  assert.equal(selectedLabel(), "RunPod");
  await choice.en.pickType(user, "Private cloud");
  assert.equal(selectedLabel(), "ZStack Cloud");
  assert.deepEqual(connects("It connects").map(([tag]) => tag), ["Host"]);
  assert.deepEqual(await choice.en.choices(user), ["ZStack Cloud"]);
  // Back on the first type: its first platform, not the one picked there before.
  await choice.en.pickType(user, "Public cloud");
  assert.equal(selectedLabel(), "Alibaba Cloud");
  assert.deepEqual(await choice.en.choices(user), ["Alibaba Cloud", "RunPod", "DigitalOcean"]);
  // Through Kubernetes, which has no list, and out again.
  await choice.en.pickType(user, K8S);
  assert.equal(platformSelects().length, 0);
  await choice.en.pickType(user, "Other");
  assert.equal(selectedLabel(), "New Cloud");
  assert.deepEqual(fieldLabels(), [p.config.type, p.config.name, "API Key"]);
});

test("after the list was opened and closed without a choice, a tab still moves to its type's first platform, with its credential fields, and Add sends that platform", async () => {
  const sent: unknown[] = [];
  api.addProvider = (async (body: unknown) => { sent.push(body); return { id: 1, kind: "aliyun", name: "probe" }; }) as never;
  const user = mount();
  await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
  assert.deepEqual(await choice.en.choices(user), ["ZStack Cloud"]);
  await choice.en.pickType(user, "Public cloud");
  assert.equal(selectedLabel(), "Alibaba Cloud", "a platform, not the list's placeholder");
  assert.deepEqual(fieldLabels(), [p.config.type, p.config.name, "API Key"]);
  await user.type(screen.getByLabelText(/^Name/), "probe");
  await user.type(screen.getByLabelText(/^API Key/), "key");
  await user.click(within(dialog()).getByRole("button", { name: "Add" }));
  await waitFor(() => assert.deepEqual(sent, [{ kind: "aliyun", name: "probe", api_key: "key" }]));
});

test("an account name of spaces only is refused at the field, as an empty one is, and nothing is sent; a real name then is sent", async () => {
  const sent: unknown[] = [];
  api.addProvider = (async (body: unknown) => { sent.push(body); return { id: 1, kind: "aliyun", name: "probe" }; }) as never;
  const user = mount({ defaultKind: "aliyun" });
  await waitFor(() => assert.equal(selectedLabel(), "Alibaba Cloud"));
  const name = screen.getByLabelText(/^Name/) as HTMLInputElement;
  await user.type(name, "   ");
  await user.type(screen.getByLabelText(/^API Key/), "key");
  await user.click(within(dialog()).getByRole("button", { name: "Add" }));
  await waitFor(() => assert.ok(describedBy(screen.getByLabelText(/^Name/)).includes(p.config.nameRequired), "the refusal is said of the name field"));
  assert.deepEqual(sent, [], "spaces are no name");
  assert.equal(screen.queryAllByRole("dialog").length, 1);
  await user.clear(name);
  await user.type(name, "probe");
  await user.click(within(dialog()).getByRole("button", { name: "Add" }));
  await waitFor(() => assert.equal(sent.length, 1));
  assert.equal((sent[0] as { name: string }).name, "probe");
});

test("the dialog opens on the tab of the platform it was opened for: a vendor's card on its type, a refreshed schema leaves the tab and the vendor as chosen", async () => {
  api.kubeDefaults = async () => KUBE_DEFAULTS;
  const view = (kinds: KindInfo[], defaultKind: string) => (
    <I18nextProvider i18n={languages.en.i18n}>
      <ProviderConfigModal open defaultKind={defaultKind} kindInfos={kinds} categories={kindCategories(kinds)} providerLabel="External Platform" onClose={() => {}} onSaved={() => {}} canConnectCluster />
    </I18nextProvider>
  );
  const rendered = render(view(kindInfos, "runpod"));
  const user = userEvent.setup({ document: dom.window.document });
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  assert.deepEqual(typeTabs(), [["Public cloud", true], ["Private cloud", false], ["Other", false], [K8S, false]]);

  // The page polls the schema: a fresh copy of it is not a reason to go back to what the dialog was opened on.
  await choice.en.pickType(user, "Private cloud");
  rendered.rerender(view(structuredClone(kindInfos), "runpod"));
  await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
  assert.equal(await choice.en.chosenType(), "Private cloud");
  await choice.en.pickType(user, K8S);
  rendered.rerender(view(structuredClone(kindInfos), "runpod"));
  assert.equal(await choice.en.chosenType(), K8S);
  assert.equal(platformSelects().length, 0);
});

test("a tab changes the platform as the list does: the name typed so far stays, the credentials are the new kind's defaults", async () => {
  const withDefaults = (kind: KindInfo, endpoint: string): KindInfo => ({ ...kind, credential_fields: [
    ...credential_fields,
    { name: "endpoint", label: { en: "API Endpoint", zh: "API 接口地址" }, secret: false, required: false, default_value: endpoint },
  ] });
  const user = mount({ defaultKind: "runpod", kinds: [withDefaults(runpod, "https://api.runpod.io/v2"), withDefaults(zstack, "http://zstack.example/zstack")] });
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  const input = (label: string) => screen.getByLabelText(new RegExp(`^${label}`)) as HTMLInputElement;
  assert.equal(input("API Endpoint").value, "https://api.runpod.io/v2");
  await user.type(input("Name"), "my cloud");
  await user.type(input("API Key"), "runpod-secret");
  await user.clear(input("API Endpoint"));
  await user.type(input("API Endpoint"), "https://proxy.example/v2");

  await choice.en.pickType(user, "Private cloud");
  assert.equal(selectedLabel(), "ZStack Cloud");
  assert.equal(input("Name").value, "my cloud");
  assert.equal(input("API Key").value, "", "one vendor's key is not offered to another");
  assert.equal(input("API Endpoint").value, "http://zstack.example/zstack", "nor its address");
  assert.equal(input("Name").placeholder, "For example, My ZStack Cloud");

  // And back: the first vendor's defaults again, not what was typed for it before.
  await choice.en.pickType(user, "Public cloud");
  assert.deepEqual([input("Name").value, input("API Key").value, input("API Endpoint").value], ["my cloud", "", "https://api.runpod.io/v2"]);
});

// The kind schema could not be read, so there is no vendor to choose (the page says so above its cards).
for (const who of ["a viewer", "an administrator"] as const) {
  test(`without a platform chosen nothing is added: ${who} whose dialog has no vendor to offer sends no request with Add, and the dialog stays`, async () => {
    const sent: unknown[] = [];
    let saved = 0;
    api.addProvider = (async (body: unknown) => { sent.push(body); return { id: 1, kind: "", name: "n" }; }) as never;
    api.kubeDefaults = async () => KUBE_DEFAULTS;
    render(
      <I18nextProvider i18n={languages.en.i18n}>
        <ProviderConfigModal open defaultKind={null} kindInfos={[]} categories={[]} providerLabel="External Platform" onClose={() => {}} onSaved={() => { saved += 1; }} canConnectCluster={who === "an administrator"} />
      </I18nextProvider>,
    );
    const user = userEvent.setup({ document: dom.window.document });
    const name = await within(dialog()).findByLabelText(/^Name/);
    assert.equal(within(dialog()).queryAllByLabelText(/Key|Endpoint/).length, 0, "no vendor, so no credential is asked for");
    await user.type(name, "n");
    await user.click(within(dialog()).getByRole("button", { name: "Add" }));
    await user.type(name, "{Enter}");
    // Let whatever the click and Enter started settle before saying nothing happened.
    await waitFor(() => assert.equal((within(dialog()).getByRole("button", { name: "Add" }) as HTMLButtonElement).disabled, false));
    assert.deepEqual(sent, [], "an account of no platform is never sent");
    assert.equal(saved, 0);
    assert.equal(screen.queryAllByRole("dialog").length, 1);
    assert.equal((name as HTMLInputElement).value, "n");
  });
}

test("whoever may connect a cluster is offered Kubernetes cluster as the last tab; nobody else is", async () => {
  mount({ canConnectCluster: true });
  await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
  assert.deepEqual(await choice.en.types(), ["Public cloud", "Private cloud", "Other", K8S]);

  cleanup();
  const user = mount({ canConnectCluster: false });
  await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
  assert.deepEqual(await choice.en.types(), ["Public cloud", "Private cloud", "Other"]);
  assert.equal(screen.queryAllByRole("tab", { name: /Kubernetes/ }).length, 0);
  // Nor is it a vendor of some type: Kubernetes is only ever a tab.
  await choice.en.pickType(user, "Public cloud");
  assert.deepEqual(await choice.en.choices(user), ["Alibaba Cloud", "RunPod", "DigitalOcean"]);
});

test("the platform type is chosen from the keyboard: one tab stop, the arrows move along the tabs and choose as they go, and the vendors' list is the next stop", async () => {
  const user = mount({ canConnectCluster: true });
  await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
  const tab = (name: string) => screen.getByRole("tab", { name });
  const stops = () => within(screen.getByRole("tablist")).getAllByRole("tab").filter((each) => each.tabIndex === 0).map((each) => each.textContent);
  // The tabs are one stop for Tab: the selected one.
  assert.deepEqual(stops(), ["Private cloud"]);
  tab("Private cloud").focus();

  await user.keyboard("{ArrowLeft}");
  await waitFor(() => assert.equal(selectedLabel(), "Alibaba Cloud"));
  assert.deepEqual(typeTabs(), [["Public cloud", true], ["Private cloud", false], ["Other", false], [K8S, false]]);
  assert.ok(document.activeElement === tab("Public cloud"), "the focus moves with the choice");
  assert.deepEqual(stops(), ["Public cloud"]);
  assert.deepEqual(connects("It connects").map(([tag]) => tag), ["ECS", "ACK"], "the dialog follows the tab");

  await user.keyboard("{ArrowRight}{ArrowRight}");
  await waitFor(() => assert.equal(selectedLabel(), "New Cloud"));
  assert.equal(await choice.en.chosenType(), "Other");
  assert.ok(document.activeElement === tab("Other"));

  // Tab leaves the tabs for the vendors' list of that type, and Shift+Tab comes back to the selected tab.
  await user.tab();
  assert.ok(document.activeElement === await choice.en.trigger(), "the list is the next stop");
  await user.tab({ shift: true });
  assert.ok(document.activeElement === tab("Other"));
});

test("the vendor is chosen from the keyboard within its type: Enter opens the list on the chosen one, the arrows move, Enter chooses and hands the focus back; Escape leaves the list and the dialog as they were", async () => {
  let closed = 0;
  const user = mount({ defaultKind: "runpod", canConnectCluster: true, onClose: () => { closed += 1; } });
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  const trigger = await choice.en.trigger();
  const marked = (listbox: HTMLElement) => within(listbox).getAllByRole("option").filter((option) => option.getAttribute("aria-selected") === "true").map((option) => option.textContent);
  const active = () => document.activeElement?.textContent;

  let listbox = await choice.en.open(user);
  assert.deepEqual(marked(listbox), ["RunPod"], "the chosen platform is the marked one");
  await waitFor(() => assert.equal(active(), "RunPod", "and the one the keys start from"));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.equal(active(), "DigitalOcean"));
  await user.keyboard("{Enter}");
  await choice.en.closed();
  assert.equal(selectedLabel(), "DigitalOcean");
  await waitFor(() => assert.ok(document.activeElement === trigger, "the focus is back on the list's button"));
  assert.deepEqual(connects("It connects").map(([tag]) => tag), ["Droplet", "DOKS"], "the dialog follows the choice");
  assert.equal(await choice.en.chosenType(), "Public cloud", "still on its type's tab");

  // Escape in the open list closes the list only: the platform and the dialog stay.
  listbox = await choice.en.open(user);
  assert.deepEqual(marked(listbox), ["DigitalOcean"]);
  await waitFor(() => assert.equal(active(), "DigitalOcean"));
  await user.keyboard("{Home}");
  await waitFor(() => assert.equal(active(), "Alibaba Cloud"));
  await user.keyboard("{Escape}");
  await choice.en.closed();
  assert.equal(selectedLabel(), "DigitalOcean");
  assert.equal(closed, 0, "the dialog did not take the Escape for itself");
  assert.equal(screen.queryAllByRole("dialog").length, 1);
});

test("the Kubernetes cluster tab is reached with the arrows like any other and shows the cluster form; from that form Shift+Tab is back on the tab, and the arrows lead to the vendors again", async () => {
  const user = mount({ defaultKind: "newcloud", canConnectCluster: true });
  await waitFor(() => assert.equal(selectedLabel(), "New Cloud"));
  const tab = (name: string) => screen.getByRole("tab", { name });
  tab("Other").focus();
  await user.keyboard("{ArrowRight}");
  await waitFor(() => assert.deepEqual(footerButtons(), [k.drawer.cancel, k.drawer.submit]));
  assert.equal(await choice.en.chosenType(), K8S);
  assert.equal(platformSelects().length, 0);
  // The tabs are the stop before the cluster form's first field: the way back to the vendors is two keys.
  const name = within(dialog()).getByLabelText(new RegExp(`^${k.drawer.name}( \\*)?$`));
  name.focus();
  await user.tab({ shift: true });
  assert.ok(document.activeElement === tab(K8S), "the selected tab is the tabs' one stop");
  await user.keyboard("{ArrowLeft}");
  await waitFor(() => assert.equal(selectedLabel(), "New Cloud"));
  assert.deepEqual(footerButtons(), ["Cancel", "Add"]);
  assert.ok(document.activeElement === tab("Other"));
});

test("the Kubernetes cluster tab puts the cluster form in place of the vendor's, with no vendor left to choose, says what it connects, and swaps the buttons; a vendors' tab brings its form back", async () => {
  const user = mount({ defaultKind: "runpod", canConnectCluster: true });
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  assert.deepEqual(fieldLabels(), [p.config.type, p.config.name, "API Key"]);
  assert.deepEqual(footerButtons(), ["Cancel", "Add"]);
  // The vendor's form: its Add submits it, and the platform choice is not one of its fields.
  const vendorForm = dialog().querySelector("form") as HTMLFormElement;
  const addButton = within(dialog()).getByRole("button", { name: "Add" }) as HTMLButtonElement;
  assert.deepEqual([dialog().querySelectorAll("form").length, addButton.type, addButton.form === vendorForm], [1, "submit", true]);
  assert.equal(vendorForm.contains(await choice.en.trigger()), false);
  assert.equal(vendorForm.contains(screen.getByRole("tablist")), false);

  await choice.en.pickType(user, K8S);
  await waitFor(() => assert.deepEqual(fieldLabels().slice(0, 4), [p.config.type, ...CLUSTER_FIELDS]));
  // Kubernetes is one platform: the tab is the whole choice, and no list is left under it.
  assert.deepEqual(typeTabs(), [["Public cloud", false], ["Private cloud", false], ["Other", false], [K8S, true]]);
  assert.equal(platformSelects().length, 0);
  assert.equal(fieldLabels().filter((label) => label === "API Key").length, 0, "a cluster has no account to sign in to");
  assert.ok(within(dialog()).getByRole("button", { name: k.drawer.advanced }));
  // The cluster itself, and how: one line, though two of the vendors here list their managed clusters from an account.
  assert.deepEqual(connects("It connects"), [["Kubernetes", p.config.howOwnCluster]]);
  assert.equal(within(dialog()).queryAllByText(/adding the cloud account/).length, 0);
  // One form, the cluster's, in place of the vendor's: a form inside a form is not one, and two would both answer Enter.
  const forms = [...dialog().querySelectorAll("form")];
  assert.equal(forms.length, 1);
  assert.equal(within(forms[0]).queryAllByLabelText(/^API Key/).length, 0, "the vendor's fields are gone with its form");
  assert.equal(forms[0].contains(within(dialog()).getByLabelText(new RegExp(`^${k.drawer.serverUrl}`))), true);
  assert.equal(forms[0].contains(screen.getByRole("tablist")), false, "the platform choice stays with the dialog");
  // The cluster form's own buttons: its Connect submits that form.
  assert.deepEqual(footerButtons(), [k.drawer.cancel, k.drawer.submit]);
  const connect = within(dialog()).getByRole("button", { name: k.drawer.submit }) as HTMLButtonElement;
  assert.equal(connect.type, "submit");
  assert.ok(connect.form === forms[0], "the Connect button belongs to the cluster form");

  // Back on the vendors: the type's first platform, with its list and its form.
  await choice.en.pickType(user, "Public cloud");
  await waitFor(() => assert.deepEqual(fieldLabels(), [p.config.type, p.config.name, "API Key"]));
  assert.equal(selectedLabel(), "Alibaba Cloud");
  assert.deepEqual(footerButtons(), ["Cancel", "Add"]);
  assert.equal(dialog().querySelectorAll("form").length, 1);
  assert.deepEqual(connects("It connects").map(([tag]) => tag), ["ECS", "ACK"]);
});

test("a Kubernetes cluster says only what it connects, in one line, whichever vendors the server knows", async () => {
  for (const kinds of [[runpod, zstack], kindInfos, []]) {
    mount({ defaultKind: "kubernetes", canConnectCluster: true, kinds });
    await waitFor(() => assert.deepEqual(connects("It connects"), [["Kubernetes", p.config.howOwnCluster]]));
    assert.deepEqual(tagTitles("It connects"), [null]);
    cleanup();
  }
});

test("opened on Kubernetes the dialog is on the cluster form even when the server offers no vendor: its tab is the only one; for someone who may not connect a cluster it is not", async () => {
  mount({ defaultKind: "kubernetes", canConnectCluster: true, kinds: [] });
  await waitFor(() => assert.deepEqual(fieldLabels().slice(0, 4), [p.config.type, ...CLUSTER_FIELDS]));
  // One type is still shown as a tab: it says what the form below is for.
  assert.deepEqual(typeTabs(), [[K8S, true]]);
  assert.equal(platformSelects().length, 0);
  assert.deepEqual(footerButtons(), [k.drawer.cancel, k.drawer.submit]);

  // The page never asks this of a viewer; if it did, the dialog falls back to the first vendor.
  cleanup();
  mount({ defaultKind: "kubernetes", kinds: [runpod, zstack] });
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  assert.deepEqual(typeTabs(), [["Public cloud", true], ["Private cloud", false]]);
  assert.deepEqual(fieldLabels(), [p.config.type, p.config.name, "API Key"]);
  assert.deepEqual(footerButtons(), ["Cancel", "Add"]);
  assert.equal(dialog().querySelectorAll("form").length, 1);
});

test("in Chinese the platform types, their vendors and what Kubernetes connects read in Chinese", async () => {
  const user = mount({ language: "zh", defaultKind: "aliyun", canConnectCluster: true });
  await waitFor(() => assert.deepEqual(connects("可以接入"), [
    ["ECS", "同步实例后，对选中的实例一键安装 agent。"],
    ["ACK", "列出账号下的托管集群，选择 GPU 节点池后接入，池里的节点自动成为计算节点。"],
  ]));
  // A virtual machine is 虚拟机 here, as everywhere else in the console.
  assert.deepEqual(tagTitles("可以接入"), ["虚拟机", "Kubernetes"]);
  assert.equal(screen.getByRole("tablist").getAttribute("aria-label"), "平台类型");
  assert.deepEqual(typeTabs(), [["公有云", true], ["专有云", false], ["其他", false], [K8S_ZH, false]]);
  assert.equal(selectedLabel(), "阿里云");
  await choice.zh.pickType(user, "专有云");
  assert.equal(selectedLabel(), "ZStack Cloud");
  await choice.zh.pickType(user, "其他");
  assert.equal(selectedLabel(), "新云");

  await choice.zh.pickType(user, K8S_ZH);
  await waitFor(() => assert.deepEqual(connects("可以接入"), [
    ["Kubernetes", "填写下面的信息后得到一条 kubectl apply 命令，在集群里执行，选中的 GPU 节点自动成为计算节点。"],
  ]));
  assert.equal(within(dialog()).queryAllByText(/更省事/).length, 0);
  assert.equal(platformSelects().length, 0);
  assert.deepEqual(footerButtons(), ["取消", "接入"]);
  assert.equal(document.body.textContent?.includes("{{"), false, "no sentence is left with an unfilled placeholder");

  await choice.zh.pickType(user, "公有云");
  assert.deepEqual(await choice.zh.choices(user), ["阿里云", "RunPod", "DigitalOcean"]);
});

test("editing an account shows its platform as a fixed field: no tabs, nothing to choose, nothing it connects, no Kubernetes", async () => {
  const account: Provider = {
    id: 3, name: "ali-prod", kind: "aliyun", category: "public_cloud", state: "valid", instance_count: 5,
    resources: [{ type: "vm", product: "ECS" }, { type: "kubernetes", product: "ACK" }], node_count: 2,
    created_at: "2026-11-01T08:00:00Z", updated_at: "2026-11-01T08:00:00Z",
  };
  const saved: Config = { id: 3, kind: "aliyun", name: "ali-prod", endpoint: "", public_key: "", extra: {} };
  api.provider = async () => saved;
  mount({ provider: account, canConnectCluster: true, defaultKind: "kubernetes" });
  await waitFor(() => assert.equal((screen.getByRole("button", { name: /Save/ }) as HTMLButtonElement).disabled, false));
  const platform = screen.getByLabelText("External platform") as HTMLInputElement;
  assert.deepEqual([platform.value, platform.disabled], ["Alibaba Cloud", true], "the dialog is on the account's kind, which connects two resources");
  assert.equal(screen.queryAllByRole("tablist").length + screen.queryAllByRole("tab").length, 0);
  assert.equal(screen.queryAllByRole("combobox").length, 0);
  assert.equal(screen.queryAllByText("It connects").length, 0);
  assert.equal(screen.queryAllByRole("list").length, 0);
  assert.deepEqual(fieldLabels(), [p.config.type, p.config.name, "API Key"]);
  assert.deepEqual(footerButtons(), ["Cancel", "Save"]);
  assert.equal(dialog().querySelectorAll("form").length, 1);
});

// The platform types, their order and what each offers are the server's (GET /api/v1/providers/kinds categories), not the
// console's: these cases give categories that differ from what the kinds' own fields or a fixed order would make.
test("the tabs are the server's categories in the server's order, each offering the kinds it names in its order", async () => {
  const categories: CategoryInfo[] = [
    { id: "private_cloud", kinds: ["zstack"] },
    { id: "public_cloud", kinds: ["runpod", "aliyun"] },
    { id: "kubernetes", resources: [{ type: "kubernetes" }], kube_cluster: true, kinds: [] },
  ];
  const user = mount({ kinds: [zstack, aliyun, runpod], categories, canConnectCluster: true });
  await waitFor(() => assert.equal(selectedLabel(), "ZStack Cloud"));
  assert.deepEqual(await choice.en.types(), ["Private cloud", "Public cloud", K8S]);
  await choice.en.pickType(user, "Public cloud");
  assert.equal(selectedLabel(), "RunPod", "the tab's first platform is the category's first");
  assert.deepEqual(await choice.en.choices(user), ["RunPod", "Alibaba Cloud"]);
});

test("without a cluster category from the server nobody is offered Kubernetes, not even whoever may connect a cluster; opened on it, the dialog falls back to the first vendor", async () => {
  const categories = kindCategories([runpod, zstack]).filter((category) => !category.kube_cluster);
  mount({ kinds: [runpod, zstack], categories, canConnectCluster: true, defaultKind: "kubernetes" });
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  assert.deepEqual(await choice.en.types(), ["Public cloud", "Private cloud"]);
  assert.equal(screen.queryAllByRole("tab", { name: /Kubernetes/ }).length, 0);
  assert.deepEqual(footerButtons(), ["Cancel", "Add"]);
});

test("the Kubernetes tab is the category the server marks as added with the cluster form, whatever its id, and says what that category connects; an empty one is no tab", async () => {
  const categories: CategoryInfo[] = [
    { id: "public_cloud", kinds: ["runpod"] },
    // A type of no kind and no cluster form has nothing to offer.
    { id: "kubernetes", kinds: [] },
    { id: "own_cluster", resources: [{ type: "kubernetes" }], kube_cluster: true, kinds: [] },
  ];
  mount({ kinds: [runpod], categories, canConnectCluster: true, defaultKind: "own_cluster" });
  await waitFor(() => assert.deepEqual(fieldLabels().slice(0, 4), [p.config.type, ...CLUSTER_FIELDS]));
  assert.deepEqual(typeTabs(), [["Public cloud", false], [K8S, true]]);
  assert.deepEqual(connects("It connects"), [["Kubernetes", p.config.howOwnCluster]]);
  assert.deepEqual(footerButtons(), [k.drawer.cancel, k.drawer.submit]);
});
