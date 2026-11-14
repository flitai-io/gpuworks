// What the External Platforms page (ui/src/pages/CloudProvidersPage.tsx) does
// once an account is added in its dialog (ui/src/components/ProviderConfigModal.tsx,
// which closes on every save and names a new account to the page): the
// address becomes ?open=p<id>, so the new account's row opens on what it
// holds; a kind that lists instances is synchronized once, as the row's Sync
// does (its toast included); the clusters are read when the row opens.
// Editing an account changes neither the address nor syncs.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { list } from "./helpers/listPage.ts";
import { account, aliyunKind, capabilities, cloudCluster, instance, servePlatforms } from "./helpers/platformFixtures.ts";
import { groupOf, instanceRow } from "./helpers/platformDetail.ts";
import { freshPopupFramesPerTest } from "./helpers/platformDialog.ts";

const { i18n, locale } = await createI18n(["providers", "kubernetes"], "en");
const p = locale.providers;
const { MemoryRouter, useLocation } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { api } = await import("../src/api.ts");
type CloudProvider = import("../src/api.ts").CloudProvider;
type ProviderKindInfo = import("../src/api.ts").ProviderKindInfo;

const original = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { message.destroy(); cleanup(); Object.assign(api, original); });

const credential_fields = [{ name: "api_key", label: { en: "API Key" }, secret: true, required: true }];
const aliyun: ProviderKindInfo = { ...aliyunKind, credential_fields };
// A kind the server cannot list instances of: hosts are connected some other way.
const hostsOnly: ProviderKindInfo = {
  kind: "racks", label: { en: "Racks" }, category: "private_cloud", capabilities: { ...capabilities, can_list_instances: false }, credential_fields,
  server_dial_supported: true, resources: [{ type: "host" }],
};

/** Where the page's address is now. */
const where = { search: "" };
function Address() {
  where.search = useLocation().search;
  return null;
}

/**
 * The page with its lists served; Add makes account 7 of the kind chosen and
 * lists it from then on. A sync records the account and, once it has run, the
 * account lists its instance; `syncError` makes it fail.
 */
function mount({ providers = [] as CloudProvider[], syncError = null as Error | null } = {}) {
  const server = servePlatforms(api, { providers, kinds: [aliyun, hostsOnly] });
  const seen = { added: [] as unknown[], synced: [] as number[], instanceReads: 0, clusterReads: [] as number[] };
  let synced = false;
  Object.assign(api, {
    addProvider: async (body: { kind: string; name: string }) => {
      seen.added.push(body);
      const made = account(7, body.name, body.kind);
      server.providers = [...(server.providers as CloudProvider[]), made];
      return made;
    },
    syncProvider: async (id: number) => {
      seen.synced.push(id);
      if (syncError) throw syncError;
      synced = true;
      return { count: 1 };
    },
    providerInstances: async () => { seen.instanceReads += 1; return list(synced ? [instance()] : []); },
    providerClusters: async (id: number) => { seen.clusterReads.push(id); return list([cloudCluster("c-gpu", "ack-gpu")]); },
  });
  where.search = "";
  render(
    <I18nextProvider i18n={i18n}>
      <FeedbackHost />
      <MemoryRouter initialEntries={["/providers"]}><Address /><CloudProvidersPage /></MemoryRouter>
    </I18nextProvider>,
  );
  return { server, seen, user: userEvent.setup({ document: dom.window.document }) };
}

const toasts = () => [...(screen.queryByRole("list", { name: "Notices" })?.children ?? [])].map((toast) => toast.textContent);
/** The row of the platform with that name. */
const rowOf = (name: string) => screen.getAllByRole("row").find((row) => within(row).queryAllByRole("cell")[1]?.textContent === name) as HTMLElement | undefined;
/** Whether a row is open: its own fold button says so. */
const opened = (name: string) => {
  const row = rowOf(name);
  const fold = row && [...row.querySelectorAll("button[aria-expanded]")].find((button) => !button.hasAttribute("aria-haspopup"));
  return fold?.getAttribute("aria-expanded") === "true";
};
/** Opens the add dialog from the kind's card, fills it in and presses Add. */
async function addFromCard(user: ReturnType<typeof userEvent.setup>, kind: string, name: string) {
  await user.click(await screen.findByRole("button", { name: `Add ${kind}` }));
  const dialog = await screen.findByRole("dialog", { name: "Add External Platform" });
  await waitFor(() => assert.ok(within(dialog).getByLabelText(/^Name/)));
  fireEvent.change(within(dialog).getByLabelText(/^Name/), { target: { value: name } });
  fireEvent.change(within(dialog).getByLabelText(/^API Key/), { target: { value: "key" } });
  await user.click(within(dialog).getByRole("button", { name: "Add" }));
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the dialog closes once the account is added"));
}

test("adding an Alibaba Cloud account closes the dialog and opens its row: it is synchronized once, so its instances are listed, and its clusters are read", async () => {
  const { seen, user } = mount();
  await screen.findByRole("button", { name: "Add Alibaba Cloud" });
  await addFromCard(user, "Alibaba Cloud", "ali-new");
  assert.deepEqual(seen.added, [{ kind: "aliyun", name: "ali-new", api_key: "key" }]);
  await waitFor(() => assert.equal(where.search, "?open=p7", "the address names the new account's row"));
  await waitFor(() => assert.ok(rowOf("ali-new"), "the list is read again and holds the account"));
  await waitFor(() => assert.ok(opened("ali-new"), "and its row is open"));
  await waitFor(() => assert.deepEqual(seen.synced, [7], "one sync, of the new account"));
  assert.ok(await instanceRow("train-01"), "the instances the sync found are listed");
  assert.ok(await groupOf("ack-gpu"), "and the account's clusters, read as the row opened");
  assert.ok(seen.clusterReads.every((id) => id === 7) && seen.clusterReads.length >= 1);
  await waitFor(() => assert.deepEqual(toasts(), [p.message.syncDone.replace("{{count}}", "1")], "the sync says how it went, as the row's Sync does"));
  assert.deepEqual(seen.synced, [7], "only once");
});

test("adding an account of a kind that lists no instances opens its row without a sync", async () => {
  const { seen, user } = mount();
  await addFromCard(user, "Racks", "rack-a");
  await waitFor(() => assert.equal(where.search, "?open=p7"));
  await waitFor(() => assert.ok(opened("rack-a")));
  assert.deepEqual(seen.synced, []);
  assert.deepEqual(toasts(), [], "nothing to say of a sync that did not run");
});

test("a failed first sync says why, and the new account's row stays open", async () => {
  const { seen, user } = mount({ syncError: new Error("aliyun: InvalidAccessKeyId") });
  await addFromCard(user, "Alibaba Cloud", "ali-new");
  await waitFor(() => assert.deepEqual(seen.synced, [7]));
  await waitFor(() => assert.deepEqual(toasts(), [p.message.syncFailed.replace("{{detail}}", "aliyun: InvalidAccessKeyId")]));
  assert.equal(where.search, "?open=p7");
  await waitFor(() => assert.ok(opened("ali-new"), "the row is open all the same"));
  assert.ok(await groupOf("ack-gpu"), "its clusters are still read");
});

test("saving an edited account closes the dialog without changing the address or syncing", async () => {
  const existing = account(3, "ali-prod", "aliyun");
  const { seen, user } = mount({ providers: [existing] });
  const updates: unknown[] = [];
  Object.assign(api, {
    provider: async () => ({ id: 3, kind: "aliyun", name: "ali-prod", extra: {} }),
    updateProvider: async (id: number, body: { name: string }) => { updates.push([id, body]); return { id, kind: "aliyun", name: body.name }; },
  });
  await waitFor(() => assert.ok(rowOf("ali-prod")));
  await user.click(within(rowOf("ali-prod") as HTMLElement).getByRole("button", { name: p.table.edit }));
  const dialog = await screen.findByRole("dialog");
  const save = within(dialog).getByRole("button", { name: p.config.save }) as HTMLButtonElement;
  await waitFor(() => assert.equal(save.disabled, false));
  await user.click(save);
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));
  assert.equal(updates.length, 1);
  assert.equal(where.search, "", "the address is left as it was");
  assert.equal(opened("ali-prod"), false, "no row is opened");
  assert.deepEqual(seen.synced, []);
});
