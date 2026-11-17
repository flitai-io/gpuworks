import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, capabilities, cloudEntry, kubeEntry, runpodKind as runpod, servePlatforms, zstackKind as zstack } from "./helpers/platformFixtures.ts";
import { field, footerButtons, freshPopupFramesPerTest } from "./helpers/platformDialog.ts";

// The External Platforms page (ui/src/pages/CloudProvidersPage.tsx) as one
// list of public and private clouds: its wording in both languages, its
// loading, empty and failed states, what a row's name and its three actions
// (edit, sync, delete) do, and the platform type column and filter, which
// follow the category the server sends with each platform, and the
// ?category= address that opens it filtered.

const locales = { en: await createI18n(["providers"], "en"), zh: await createI18n(["providers"], "zh") };
const languages = { en: locales.en.i18n, zh: locales.zh.i18n };
const p = locales.en.locale.providers;
const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { api } = await import("../src/api.ts");
type ProviderEditConfig = import("../src/api.ts").ProviderEditConfig;
type CloudProvider = import("../src/api.ts").CloudProvider;
type ProviderKindInfo = import("../src/api.ts").ProviderKindInfo;

const original = { ...api };
await freshPopupFramesPerTest();
// The confirmations are queued outside the tree: one a failing test leaves open would open in the next.
afterEach(async () => {
  for (let open = screen.queryByRole("dialog"); open; open = screen.queryByRole("dialog")) {
    const cancel = within(open).queryAllByRole("button", { name: /^(Cancel|取消)$/ }).at(-1) as HTMLButtonElement | undefined;
    if (!cancel || cancel.disabled) break;
    cancel.click();
    await waitFor(() => assert.notEqual(screen.queryByRole("dialog"), open));
  }
  message.destroy(); cleanup(); Object.assign(api, original);
});

// As the server answers: each platform carries the category, resources and node count of its kind.
const pods = account(1, "Pods", "runpod", { instance_count: 1 });
const hosts = account(2, "Hosts", "zstack", { instance_count: 1 });

/** The session, the two lists and the kind schema; no cluster is connected on its own. */
function serve(providers: CloudProvider[], kinds: ProviderKindInfo[] = [runpod, zstack]) {
  return servePlatforms(api, { providers, kinds, clusters: [] });
}

// The outcome of a row's action is said in a toast, which the console's frame hosts.
function renderPage(language: keyof typeof languages = "en", address = "/providers") {
  const view = render(<I18nextProvider i18n={languages[language]}><FeedbackHost /><MemoryRouter initialEntries={[address]}><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  return { ...view, user: userEvent.setup({ document: dom.window.document }) };
}
/** The row of the platform with that name. */
const rowOf = (name: string) => screen.getAllByRole("row").find((row) => within(row).queryAllByRole("cell")[1]?.textContent === name) as HTMLElement;
/** A row's action button, by its name. */
const action = (name: string, label: string) => within(rowOf(name)).getByRole("button", { name: label }) as HTMLButtonElement;
/** The delete confirmation: a dialog named by its question, which names the account. */
const confirmation = (name: string, language: keyof typeof languages = "en") => screen.findByRole("dialog", { name: languages[language].t("table.deleteConfirm", { ns: "providers", name }) });
/** What the confirmation says below its question, paragraph by paragraph. */
const consequences = (dialog: HTMLElement) => [...dialog.querySelectorAll("p")].map((paragraph) => paragraph.textContent);
const noConfirmation = () => waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the confirmation closed"));
/** The toasts on screen, as they read. */
const toasts = () => [...screen.getByRole("list", { name: "Notices" }).children].map((toast) => toast.textContent);

/** The names of the listed platforms, top to bottom. */
const listed = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[1].textContent);

const emptyText = "No external platform yet. Pick one above to connect it.";

const wording = {
  en: { title: "External Platforms", count: "External platforms: 2", quick: "Quick Connect", connected: "Connected external platforms", add: "Add External Platform", remove: "Delete", cancel: "Cancel", confirm: "Delete external platform \"Pods\"?" },
  zh: { title: "外部平台", count: "2 个外部平台", quick: "快速接入", connected: "已接入的外部平台", add: "添加外部平台", remove: "删除", cancel: "取消", confirm: "确认删除外部平台「Pods」？" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} the page, its delete confirmation and its add dialog speak of external platforms`, async () => {
    const copy = wording[language];
    const deleted: number[] = [];
    serve([pods, hosts]);
    Object.assign(api, { deleteProvider: async (id: number) => { deleted.push(id); } });
    const { user } = renderPage(language);
    await screen.findByText("Pods");

    assert.ok(screen.getByRole("heading", { name: copy.title }));
    assert.ok(screen.getByText(copy.count));
    // The page's two blocks, each a section under its own second-level heading.
    assert.deepEqual(screen.getAllByRole("heading", { level: 2 }).filter((heading) => heading.closest("section")).map((heading) => heading.textContent), [copy.quick, copy.connected]);
    assert.equal(document.querySelectorAll("section").length, 2);

    await user.click(screen.getAllByRole("button", { name: copy.remove })[0]);
    const asking = await screen.findByRole("dialog", { name: copy.confirm });
    assert.deepEqual(footerButtons(asking), [copy.cancel, copy.remove]);
    assert.deepEqual(deleted, [], "asking is not deleting");
    await user.click(within(asking).getByRole("button", { name: copy.cancel }));
    await noConfirmation();

    await user.click(screen.getByRole("button", { name: copy.add }));
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
  assert.equal(screen.queryAllByText(/Compute nodes: /).length, 0, "nothing to sum up");
});

test("a failed list load names external platforms and the server's reason, is not shown as an empty list, and a retry brings the list", async () => {
  let failing = true;
  serve([pods]);
  Object.assign(api, { providers: async () => { if (failing) throw new Error("upstream unavailable"); return { items: [pods], total: 1 }; } });
  renderPage();

  // A notice above the cards with the server's reason; the list below says it failed in its own place.
  const warning = await screen.findByText("Failed to load external platforms");
  assert.ok(warning.closest('[role="status"]')?.textContent?.includes("upstream unavailable"));
  assert.equal(screen.queryAllByText(emptyText).length, 0);

  failing = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  assert.ok(await screen.findByText("Pods"));
  await waitFor(() => assert.equal(screen.queryAllByText("Failed to load external platforms").length, 0));
});

/** The list's toolbar: what sits on its left, and the buttons on its right. */
function toolbar() {
  const syncAll = screen.getByRole("button", { name: "Sync All" });
  const left = syncAll.closest(".resource-table-toolbar-left") as HTMLElement;
  const right = left.parentElement?.querySelector(".resource-table-toolbar-right") as HTMLElement;
  return { syncAll, left, right, rightButtons: within(right).queryAllByRole("button").map((button) => button.getAttribute("aria-label") ?? button.textContent) };
}

test("the list's toolbar leads with Sync All and when an account last synced, the latest of them; on its right only the column settings, no refresh or update time", async () => {
  serve([
    account(1, "Pods", "runpod", { synced_at: "2026-11-09T01:00:00Z" }),
    account(2, "Hosts", "zstack", { synced_at: "2026-11-12T06:30:00Z" }),
    account(4, "Fresh", "runpod", { synced_at: undefined }),
  ]);
  renderPage();
  await screen.findByText("Fresh");
  const { syncAll, left, rightButtons } = toolbar();
  assert.ok(left.contains(syncAll), "Sync All is on the left");
  assert.equal(left.textContent, `Sync AllLast synced: ${formatTime("2026-11-12T06:30:00Z")}`, "the latest sync of all the accounts");
  assert.deepEqual(rightButtons, ["Customize columns"]);
  assert.equal(screen.queryAllByRole("button", { name: "Refresh data" }).length, 0, "the list keeps itself current");
  assert.equal(screen.queryAllByText(/^Updated /).length, 0);
});

test("with no account ever synced the toolbar has Sync All alone", async () => {
  serve([account(1, "Pods", "runpod", { synced_at: undefined })]);
  renderPage();
  await screen.findByText("Pods");
  assert.equal(toolbar().left.textContent, "Sync All");
  assert.equal(screen.queryAllByText(/^Last synced/).length, 0);
});

test("when the list cannot be read the toolbar offers a refresh beside the column settings, and the failure a Retry", async () => {
  let failing = true;
  serve([pods]);
  Object.assign(api, { providers: async () => { if (failing) throw new Error("upstream unavailable"); return { items: [pods], total: 1 }; } });
  renderPage();
  await screen.findByText("Failed to load external platforms");
  await waitFor(() => assert.deepEqual(toolbar().rightButtons, ["Refresh data", "Customize columns"]));
  failing = false;
  fireEvent.click(screen.getByRole("button", { name: "Refresh data" }));
  assert.ok(await screen.findByText("Pods"));
  await waitFor(() => assert.deepEqual(toolbar().rightButtons, ["Customize columns"], "read again: the refresh goes"));
});

test("when the kind schema cannot be loaded the platforms are still listed with their types and usable", async () => {
  const synced: number[] = [];
  serve([pods, hosts]);
  Object.assign(api, {
    providerKinds: async () => { throw new Error("schema unavailable"); },
    syncProvider: async (id: number) => { synced.push(id); return { count: 4, instances: [] }; },
  });
  const { user } = renderPage();
  await screen.findByText("Pods");
  assert.ok(await screen.findByText("The external platform types could not be loaded"));

  const column = screen.getAllByRole("columnheader").findIndex((header) => header.textContent === "Platform type");
  const rows = screen.getAllByRole("row").slice(1);
  assert.deepEqual(rows.map((row) => within(row).getAllByRole("cell").slice(1, column + 1).map((cell) => cell.textContent)), [["Pods", "RunPod", "Public cloud"], ["Hosts", "ZStack Cloud", "Private cloud"]]);

  // Without the schema nothing says a kind cannot list its instances: every account can be synchronized.
  assert.deepEqual(["Pods", "Hosts"].map((name) => action(name, "Sync").disabled), [false, false]);
  await user.click(within(rows[1]).getByRole("button", { name: "Sync" }));
  assert.ok(await screen.findByText("Synchronization complete; 4 instances found"));
  assert.deepEqual(synced, [2]);
});

// What GET /api/v1/providers/<id> answers for an edit.
const savedConfig = (provider: CloudProvider): ProviderEditConfig => ({ id: provider.id, kind: provider.kind, name: provider.name, endpoint: "", public_key: "", extra: {} });

test("a platform's name is its name and nothing else: not a button or a link, and clicking it opens nothing; the pencil in its actions opens the edit dialog on that account", async () => {
  const read: number[] = [];
  serve([pods, hosts]);
  Object.assign(api, { provider: async (id: number) => { read.push(id); return savedConfig(id === 1 ? pods : hosts); } });
  const { user } = renderPage();
  await screen.findByText("Pods");

  const name = within(rowOf("Hosts")).getAllByRole("cell")[1];
  assert.equal(name.textContent, "Hosts");
  assert.equal(name.children.length, 0, "plain text in its cell");
  assert.equal(within(name).queryAllByRole("button").length + within(name).queryAllByRole("link").length, 0);
  await user.click(name);
  await user.click(screen.getByText("Hosts"));
  assert.equal(screen.queryAllByRole("dialog").length, 0, "clicking the name edits nothing");
  assert.deepEqual(read, [], "and reads no saved configuration");
  assert.equal(within(rowOf("Hosts")).getByRole("button", { name: "Expand row" }).getAttribute("aria-expanded"), "false", "nor does it open the row");

  // Every account has the three actions, in this order; each is an icon named for what it does.
  assert.deepEqual(within(within(rowOf("Hosts")).getAllByRole("cell").at(-1) as HTMLElement).getAllByRole("button").map((button) => button.getAttribute("aria-label")), ["Edit", "Sync", "Delete"]);
  await user.click(action("Hosts", "Edit"));
  const dialog = await screen.findByRole("dialog", { name: "Edit External Platform" });
  await waitFor(() => assert.equal((within(dialog).getByLabelText(field("Name")) as HTMLInputElement).value, "Hosts"));
  assert.deepEqual(read, [2], "the account of that row, read afresh");
  const platform = within(dialog).getByLabelText("External platform") as HTMLInputElement;
  assert.deepEqual([platform.value, platform.disabled], ["ZStack Cloud", true]);
  await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));

  // The other row's pencil edits the other account.
  await user.click(action("Pods", "Edit"));
  await waitFor(async () => assert.equal((within(await screen.findByRole("dialog", { name: "Edit External Platform" })).getByLabelText(field("Name")) as HTMLInputElement).value, "Pods"));
  assert.deepEqual(read, [2, 1]);
});

test("saving an edit closes the dialog and reads the list again", async () => {
  const server = serve([pods]);
  const updates: unknown[] = [];
  Object.assign(api, {
    provider: async () => savedConfig(pods),
    updateProvider: async (id: number, body: { name: string }) => { updates.push({ id, body }); server.providers = [{ ...pods, name: body.name }]; return { id, kind: pods.kind, name: body.name }; },
  });
  const { user } = renderPage();
  await screen.findByText("Pods");
  await user.click(action("Pods", "Edit"));
  const dialog = await screen.findByRole("dialog", { name: "Edit External Platform" });
  const save = within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement;
  await waitFor(() => assert.equal(save.disabled, false));
  const name = within(dialog).getByLabelText(field("Name"));
  await user.clear(name);
  await user.type(name, "Pods EU");
  const before = server.reads.providers;
  await user.click(save);
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));
  assert.deepEqual(updates, [{ id: 1, body: { name: "Pods EU" } }]);
  assert.ok(await screen.findByText("Pods EU"));
  assert.equal(server.reads.providers, before + 1);
});

test("each action says what it is when pointed at; a kind that cannot list its instances has its sync out of use, saying it is coming", async () => {
  // As the server's schema says of a kind whose driver lists no instances yet.
  serve([pods, hosts], [runpod, { ...zstack, capabilities: { ...capabilities, can_list_instances: false } }]);
  const synced: number[] = [];
  Object.assign(api, { syncProvider: async (id: number) => { synced.push(id); return { count: 0, instances: [] }; } });
  const { user } = renderPage();
  await screen.findByText("Pods");
  await waitFor(() => assert.equal(action("Hosts", "Sync").disabled, true, "the schema has arrived"));
  assert.equal(action("Pods", "Sync").disabled, false);
  assert.deepEqual([action("Hosts", "Edit").disabled, action("Hosts", "Delete").disabled], [false, false], "the account can still be edited and deleted");

  const hint = async (button: HTMLElement) => {
    await user.hover(button.parentElement as HTMLElement);
    const text = (await screen.findByRole("tooltip")).textContent;
    await user.unhover(button.parentElement as HTMLElement);
    await waitFor(() => assert.equal(screen.queryAllByRole("tooltip").length, 0));
    return text;
  };
  assert.equal(await hint(action("Pods", "Edit")), "Edit");
  assert.equal(await hint(action("Pods", "Sync")), "Sync");
  assert.equal(await hint(action("Pods", "Delete")), "Delete");
  // A disabled button takes no pointer; the hint is on what holds it, and says why nothing happens.
  assert.equal(await hint(action("Hosts", "Sync")), "Coming Soon");
  await user.click(action("Hosts", "Sync"));
  assert.deepEqual(synced, []);
  // Sync All leaves it out too.
  await user.click(screen.getByRole("button", { name: "Sync All" }));
  assert.ok(await screen.findByText("Synchronized 1 external platforms that support synchronization"));
  assert.deepEqual(synced, [1]);
});

test("while an account is being synchronized its sync button is out of use, the other rows' are not; the outcome is a toast and the list is read again", async () => {
  const server = serve([pods, hosts]);
  let answer!: (result: { count: number; instances: never[] }) => void;
  const synced: number[] = [];
  Object.assign(api, { syncProvider: (id: number) => { synced.push(id); return new Promise((resolve) => { answer = resolve; }); } });
  const { user } = renderPage();
  await screen.findByText("Pods");
  await waitFor(() => assert.equal(action("Pods", "Sync").disabled, false));
  await user.click(action("Pods", "Sync"));
  await waitFor(() => assert.equal(action("Pods", "Sync").disabled, true, "the request is on its way"));
  assert.equal(action("Hosts", "Sync").disabled, false);
  await user.click(action("Pods", "Sync"));
  assert.deepEqual(synced, [1], "a second click sends nothing");
  assert.deepEqual(toasts(), []);

  const before = server.reads.providers;
  await act(async () => answer({ count: 7, instances: [] }));
  assert.ok(await screen.findByText("Synchronization complete; 7 instances found"));
  await waitFor(() => assert.equal(action("Pods", "Sync").disabled, false));
  assert.equal(server.reads.providers, before + 1);
});

test("a failed sync says so with the server's reason and leaves the button usable", async () => {
  serve([pods]);
  Object.assign(api, { syncProvider: async () => { throw new Error("credential expired"); } });
  const { user } = renderPage();
  await screen.findByText("Pods");
  await user.click(action("Pods", "Sync"));
  assert.equal((await screen.findByRole("alert")).textContent, "Synchronization failed: credential expired");
  await waitFor(() => assert.equal(action("Pods", "Sync").disabled, false));
});

test("deleting asks first: Cancel and Escape delete nothing; confirming deletes that account, says so and reads both lists again", async () => {
  const server = serve([pods, hosts]);
  const deleted: number[] = [];
  Object.assign(api, { deleteProvider: async (id: number) => { deleted.push(id); server.providers = [pods]; } });
  const { user } = renderPage();
  await screen.findByText("Hosts");

  await user.click(action("Hosts", "Delete"));
  let asking = await confirmation("Hosts");
  assert.deepEqual(footerButtons(asking), ["Cancel", "Delete"]);
  await user.click(within(asking).getByRole("button", { name: "Cancel" }));
  await noConfirmation();

  await user.click(action("Hosts", "Delete"));
  await confirmation("Hosts");
  await user.keyboard("{Escape}");
  await noConfirmation();
  assert.deepEqual(deleted, [], "neither Cancel nor Escape deletes");
  assert.deepEqual(listed(), ["Pods", "Hosts"]);

  await user.click(action("Hosts", "Delete"));
  asking = await confirmation("Hosts");
  const before = { ...server.reads };
  await user.click(within(asking).getByRole("button", { name: "Delete" }));
  await waitFor(() => assert.deepEqual(deleted, [2], "the account of the row that was asked about"));
  await waitFor(() => assert.deepEqual(toasts(), ["Deleted"]));
  await waitFor(() => assert.deepEqual(listed(), ["Pods"]));
  await noConfirmation();
  // Its clusters went with it: the cluster list is read again too.
  assert.deepEqual(server.reads, { providers: before.providers + 1, clusters: before.clusters + 1 });
});

test("a refused delete says so with the server's reason, and the account stays listed and can be deleted again", async () => {
  const server = serve([pods, hosts]);
  let refuse = true;
  const attempts: number[] = [];
  Object.assign(api, { deleteProvider: async (id: number) => { attempts.push(id); if (refuse) throw new Error("2 nodes still joined through it"); server.providers = [hosts]; } });
  const { user } = renderPage();
  await screen.findByText("Pods");
  await user.click(action("Pods", "Delete"));
  const before = { ...server.reads };
  await user.click(within(await confirmation("Pods")).getByRole("button", { name: "Delete" }));
  assert.equal((await screen.findByRole("alert")).textContent, "Delete failed: 2 nodes still joined through it");
  assert.deepEqual(listed(), ["Pods", "Hosts"]);
  assert.deepEqual(server.reads, before, "nothing changed, nothing is read again");
  assert.equal(screen.queryAllByText("Deleted").length, 0);

  refuse = false;
  await noConfirmation();
  await user.click(action("Pods", "Delete"));
  await user.click(within(await confirmation("Pods")).getByRole("button", { name: "Delete" }));
  await waitFor(() => assert.deepEqual(listed(), ["Hosts"]));
  assert.deepEqual(attempts, [1, 1]);
});

test("while a delete is on its way its confirmation stays and cannot be sent twice", async () => {
  serve([pods]);
  let answer!: () => void;
  const attempts: number[] = [];
  Object.assign(api, { deleteProvider: (id: number) => { attempts.push(id); return new Promise<void>((resolve) => { answer = resolve; }); } });
  const { user } = renderPage();
  await screen.findByText("Pods");
  await user.click(action("Pods", "Delete"));
  const asking = await confirmation("Pods");
  const confirm = within(asking).getByRole("button", { name: "Delete" }) as HTMLButtonElement;
  await user.click(confirm);
  await waitFor(() => assert.equal(confirm.disabled, true));
  assert.equal((within(asking).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled, true);
  await user.click(confirm);
  await user.keyboard("{Escape}");
  assert.deepEqual(attempts, [1]);
  assert.equal(screen.queryAllByRole("dialog").length, 1, "the question stays until the server answers");
  await act(async () => answer());
  await noConfirmation();
  assert.ok(await screen.findByText("Deleted"));
});

// Deleting an account says what becomes of what came through it: always that nothing in the cloud is released, and
// when entries were made from its clusters (KubeCluster.source.provider_id), how many and what they become.
test("deleting an account says nothing in the cloud is released; one clusters were connected from also says how many become clusters of their own; confirming deletes that account", async () => {
  const ali = account(3, "ali-prod", "aliyun");
  const fromAli = [cloudEntry(), cloudEntry({ id: 6, uuid: "e-6", name: "ack-lab", source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-lab" } })];
  // Neither is ali-prod's: one of another account, one made by hand.
  const others = [cloudEntry({ id: 7, uuid: "e-7", name: "do-prod", source: { provider_id: 8, provider_name: "do", provider_kind: "digitalocean", cluster_id: "c-do" } }), kubeEntry()];
  servePlatforms(api, { providers: [pods, ali], clusters: [...fromAli, ...others] });
  const deleted: number[] = [];
  Object.assign(api, { deleteProvider: async (id: number) => { deleted.push(id); } });
  const { user } = renderPage();
  await screen.findByText("prod-gpu");

  await user.click(action("Pods", "Delete"));
  let asking = await confirmation("Pods");
  assert.deepEqual(consequences(asking), [p.table.deleteConsequence], "no cluster came through it");
  await user.click(within(asking).getByRole("button", { name: "Cancel" }));
  await noConfirmation();

  await user.click(action("ali-prod", "Delete"));
  asking = await confirmation("ali-prod");
  assert.deepEqual(consequences(asking), [p.table.deleteConsequence, p.table.deleteClusterConsequence.replace("{{count}}", "2")]);
  assert.deepEqual(deleted, [], "asking is not deleting");
  await user.click(within(asking).getByRole("button", { name: "Delete" }));
  await waitFor(() => assert.deepEqual(deleted, [3]));
  await noConfirmation();
});

test("in zh the confirmation names the account and counts the clusters connected from it", async () => {
  const ali = account(3, "ali-prod", "aliyun");
  servePlatforms(api, { providers: [ali], clusters: [cloudEntry()] });
  const { user } = renderPage("zh");
  await screen.findByText("ali-prod");
  await user.click(action("ali-prod", "删除"));
  const asking = await screen.findByRole("dialog", { name: "确认删除外部平台「ali-prod」？" });
  const zhp = locales.zh.locale.providers;
  assert.deepEqual(consequences(asking), [zhp.table.deleteConsequence, zhp.table.deleteClusterConsequence.replace("{{count}}", "1")]);
  assert.deepEqual(footerButtons(asking), ["取消", "删除"]);
});

test("filters that match no platform say so in place of the empty list, and Clear filters brings every platform back", async () => {
  serve([pods, account(3, "More pods", "runpod")]);
  const { user } = renderPage("en", "/providers?category=private_cloud");
  await screen.findByRole("button", { name: "Add RunPod" });
  assert.ok(await screen.findByText(p.platforms.filteredEmpty));
  assert.equal(screen.queryAllByText(emptyText).length, 0, "the list is not empty, only filtered");
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => assert.deepEqual(listed(), ["Pods", "More pods"]));
  assert.equal(screen.queryAllByText(p.platforms.filteredEmpty).length, 0);
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
