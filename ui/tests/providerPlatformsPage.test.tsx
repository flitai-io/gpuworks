import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

// The External Platforms page as one list of public and private clouds: its
// wording in both languages, its loading, empty and failed states, and the
// platform type column and filter, which follow the category the server
// sends with each platform, and the ?category= address that opens it filtered.

const languages = { en: (await createI18n(["providers"], "en")).i18n, zh: (await createI18n(["providers"], "zh")).i18n };
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { api } = await import("../src/api.ts");
type CloudProvider = import("../src/api.ts").CloudProvider;
type ProviderKindInfo = import("../src/api.ts").ProviderKindInfo;

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const capabilities = {
  can_list_instances: true, can_power_control: true, can_install_agent: true, can_create: false,
  can_delete: false, can_inject_user_data: false, has_pricing: false,
};
const runpod: ProviderKindInfo = { kind: "runpod", label: { en: "RunPod", zh: "RunPod" }, capabilities, credential_fields: [], server_dial_supported: false };
const zstack: ProviderKindInfo = { kind: "zstack", label: { en: "ZStack Cloud", zh: "ZStack Cloud" }, capabilities, credential_fields: [], server_dial_supported: true };
// As the server answers: each platform carries the category of its kind.
const account = (id: number, name: string, kind: string): CloudProvider => ({ id, name, kind, category: kind === "zstack" ? "private_cloud" : "public_cloud", state: "valid", instance_count: 1, created_at: "", updated_at: "" });
const pods = account(1, "Pods", "runpod");
const hosts = account(2, "Hosts", "zstack");

/** The list and kind schema endpoints, answering with the collection envelope {items, total}. */
function serve(providers: CloudProvider[], kinds: ProviderKindInfo[] = [runpod, zstack]) {
  Object.assign(api, {
    providers: async () => ({ items: providers, total: providers.length }),
    providerKinds: async () => ({ items: kinds, total: kinds.length }),
    providerInstances: async () => ({ items: [], total: 0 }),
  });
}

function renderPage(language: keyof typeof languages = "en", address = "/providers") {
  return render(<I18nextProvider i18n={languages[language]}><MemoryRouter initialEntries={[address]}><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
}

/** The names of the listed platforms, top to bottom. */
const listed = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[1].textContent);

const emptyText = "No external platform yet. Pick one above to connect it.";

const wording = {
  en: { title: "External Platforms", count: "External platforms: 2", connected: "Connected external platforms", add: "Add External Platform", remove: "Delete", confirm: "Delete this external platform?" },
  zh: { title: "外部平台", count: "2 个外部平台", connected: "已接入的外部平台", add: "添加外部平台", remove: "删除", confirm: "确认删除该外部平台？" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} the page, its delete confirmation and its add dialog speak of external platforms`, async () => {
    const copy = wording[language];
    const deleted: number[] = [];
    serve([pods, hosts]);
    Object.assign(api, { deleteProvider: async (id: number) => { deleted.push(id); } });
    renderPage(language);
    await screen.findByText("Pods");

    assert.ok(screen.getByRole("heading", { name: copy.title }));
    assert.ok(screen.getByText(copy.count));
    assert.ok(screen.getByRole("heading", { name: copy.connected }));

    fireEvent.click(screen.getAllByRole("button", { name: copy.remove })[0]);
    assert.ok(within(await screen.findByRole("tooltip")).getByText(copy.confirm));
    assert.deepEqual(deleted, [], "asking is not deleting");

    fireEvent.click(screen.getByRole("button", { name: copy.add }));
    assert.ok(await screen.findByRole("dialog", { name: copy.add }));
    assert.ok(!document.body.textContent?.includes("{{"), "no sentence is left with an unfilled placeholder");
  });
}

test("a loading list does not claim to be empty; an empty one says so and points to the platforms above", async () => {
  let answer!: (list: { items: CloudProvider[]; total: number }) => void;
  serve([]);
  Object.assign(api, { providers: () => new Promise((resolve) => { answer = resolve; }) });
  renderPage();
  await screen.findByRole("button", { name: "Add RunPod" });
  assert.ok(screen.getByRole("status"));
  assert.equal(screen.queryAllByText(emptyText).length, 0);

  answer({ items: [], total: 0 });
  assert.ok(await screen.findByText(emptyText));
  assert.ok(screen.getByText("External platforms: 0"));
  assert.equal((screen.getByRole("button", { name: "Sync All" }) as HTMLButtonElement).disabled, true);
  assert.equal(screen.queryAllByText(/Accounts: /).length, 0);
});

test("a failed list load names external platforms and the server's reason, is not shown as an empty list, and a retry brings the list", async () => {
  let failing = true;
  serve([pods]);
  Object.assign(api, { providers: async () => { if (failing) throw new Error("upstream unavailable"); return { items: [pods], total: 1 }; } });
  renderPage();

  const warning = await screen.findByText("Failed to load external platforms");
  assert.ok(warning.closest('[role="alert"]')?.textContent?.includes("upstream unavailable"));
  assert.equal(screen.queryAllByText(emptyText).length, 0);

  failing = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  assert.ok(await screen.findByText("Pods"));
  await waitFor(() => assert.equal(screen.queryAllByText("Failed to load external platforms").length, 0));
});

test("when the kind schema cannot be loaded the platforms are still listed with their types and usable", async () => {
  const synced: number[] = [];
  serve([pods, hosts]);
  Object.assign(api, {
    providerKinds: async () => { throw new Error("schema unavailable"); },
    syncProvider: async (id: number) => { synced.push(id); return { count: 4, instances: [] }; },
  });
  renderPage();
  await screen.findByText("Pods");
  assert.ok(await screen.findByText("Provider schema was not loaded"));

  const column = screen.getAllByRole("columnheader").findIndex((header) => header.textContent === "Platform type");
  const rows = screen.getAllByRole("row").slice(1);
  assert.deepEqual(rows.map((row) => within(row).getAllByRole("cell").slice(1, column + 1).map((cell) => cell.textContent)), [["Pods", "RunPod", "Public cloud"], ["Hosts", "ZStack Cloud", "Private cloud"]]);

  fireEvent.click(within(rows[1]).getByRole("button", { name: "Sync" }));
  assert.ok(await screen.findByText("Synchronization complete; 4 instances found"));
  assert.deepEqual(synced, [2]);
});

const typeOptions = async () => {
  fireEvent.click(screen.getByRole("button", { name: "Filter by Platform type" }));
  return within(await screen.findByRole("group", { name: "Filter by Platform type" })).getAllByRole("checkbox");
};

test("the platform type filter offers only the types that are in the list", async () => {
  serve([pods, account(3, "More pods", "runpod")]);
  renderPage();
  await screen.findByText("Pods");
  await screen.findAllByText("Public cloud");

  assert.deepEqual((await typeOptions()).map((option) => option.getAttribute("aria-label")), ["Public cloud"]);
});

test("filtering by platform type leaves only the platforms of that type", async () => {
  serve([pods, hosts, account(3, "More pods", "runpod")]);
  renderPage();
  await screen.findByText("Pods");
  await screen.findByText("Private cloud");
  assert.deepEqual(listed(), ["Pods", "Hosts", "More pods"]);

  const options = await typeOptions();
  assert.deepEqual(options.map((option) => option.getAttribute("aria-label")), ["Public cloud", "Private cloud"]);
  fireEvent.click(options[1]);
  await waitFor(() => assert.deepEqual(listed(), ["Hosts"]));
  fireEvent.click(options[1]);
  fireEvent.click(options[0]);
  await waitFor(() => assert.deepEqual(listed(), ["Pods", "More pods"]));
});

test("?category= opens the list on that platform type, and clearing the filter brings the others back", async () => {
  serve([pods, hosts, account(3, "More pods", "runpod")]);
  renderPage("en", "/providers?category=private_cloud");
  await screen.findByText("Hosts");
  assert.deepEqual(listed(), ["Hosts"]);

  const options = await typeOptions();
  assert.deepEqual(options.map((option) => [option.getAttribute("aria-label"), option.getAttribute("aria-checked")]), [["Public cloud", "false"], ["Private cloud", "true"]]);
  fireEvent.click(options[1]);
  await waitFor(() => assert.deepEqual(listed(), ["Pods", "Hosts", "More pods"]));
});

test("a ?category= no platform has yet still shows as the filter in use, so it can be cleared", async () => {
  serve([pods]);
  renderPage("en", "/providers?category=private_cloud");
  await screen.findByRole("button", { name: "Add RunPod" });
  await waitFor(() => assert.equal(screen.queryAllByText("Pods").length, 0));

  const options = await typeOptions();
  assert.deepEqual(options.map((option) => option.getAttribute("aria-label")), ["Public cloud", "Private cloud"]);
  fireEvent.click(options[1]);
  await waitFor(() => assert.deepEqual(listed(), ["Pods"]));
});

for (const address of ["/providers?category=", "/providers?category=nonsense", "/providers?other=private_cloud"]) {
  test(`${address} names no platform type, so the whole list opens`, async () => {
    serve([pods, hosts]);
    renderPage("en", address);
    await screen.findByText("Pods");
    assert.deepEqual(listed(), ["Pods", "Hosts"]);
    assert.deepEqual((await typeOptions()).map((option) => option.getAttribute("aria-checked")), ["false", "false"]);
  });
}

test("a platform whose kind declares no type shows — and stays out of the type filter", async () => {
  serve([pods, { ...account(4, "Lab", "newcloud"), category: "" as never }]);
  renderPage();
  await screen.findByText("Lab");
  const column = screen.getAllByRole("columnheader").findIndex((header) => header.textContent === "Platform type");
  assert.deepEqual(screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[column].textContent), ["Public cloud", "—"]);
  assert.deepEqual((await typeOptions()).map((option) => option.getAttribute("aria-label")), ["Public cloud"]);
});
