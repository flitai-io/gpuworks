// Sync in an opened account's row on the External Platforms page
// (ui/src/pages/CloudProvidersPage.tsx handing its own sync to
// ui/src/pages/platforms/ProviderResources.tsx): offered for a kind that lists
// instances, the same sync as the row's action (its toasts included), busy
// while it runs; once done the instances and the clusters are read again.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { list } from "./helpers/listPage.ts";
import { account, aliyunKind, capabilities, cloudCluster, instance, servePlatforms } from "./helpers/platformFixtures.ts";
import { groupOf } from "./helpers/platformDetail.ts";
import { freshPopupFramesPerTest } from "./helpers/platformDialog.ts";

const { i18n, locale } = await createI18n(["providers", "kubernetes"], "en");
const p = locale.providers;
const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { api } = await import("../src/api.ts");
type ProviderKindInfo = import("../src/api.ts").ProviderKindInfo;

const original = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { message.destroy(); cleanup(); Object.assign(api, original); });

const ali = account(3, "ali-prod", "aliyun");
// A kind the server cannot list instances of.
const racksKind: ProviderKindInfo = {
  kind: "racks", label: { en: "Racks" }, category: "private_cloud", capabilities: { ...capabilities, can_list_instances: false }, credential_fields: [],
  server_dial_supported: true, resources: [{ type: "host" }],
};
const racks = account(5, "rack-a", "racks", { category: "private_cloud", resources: [{ type: "host" }] });

/** The page opened on one account's row; the sync is held until `finish` (or `fail`) answers it. */
function mount(address: string) {
  servePlatforms(api, { providers: [ali, racks], kinds: [aliyunKind, racksKind] });
  const seen = { synced: [] as number[], instanceReads: 0, clusterReads: 0 };
  let synced = false;
  let pending: { resolve: (v: { count: number }) => void; reject: (e: Error) => void } | null = null;
  Object.assign(api, {
    syncProvider: (id: number) => { seen.synced.push(id); return new Promise((resolve, reject) => { pending = { resolve, reject }; }); },
    providerInstances: async () => { seen.instanceReads += 1; return list(synced ? [instance()] : []); },
    providerClusters: async () => { seen.clusterReads += 1; return list(synced ? [cloudCluster("c-gpu", "ack-gpu"), cloudCluster("c-new", "ack-new")] : [cloudCluster("c-gpu", "ack-gpu")]); },
  });
  render(<I18nextProvider i18n={i18n}><FeedbackHost /><MemoryRouter initialEntries={[address]}><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  return {
    seen,
    user: userEvent.setup({ document: dom.window.document }),
    finish: async (count: number) => { synced = true; await act(async () => pending?.resolve({ count })); },
    fail: async (error: Error) => { await act(async () => pending?.reject(error)); },
  };
}
const toasts = () => [...(screen.queryByRole("list", { name: "Notices" })?.children ?? [])].map((toast) => toast.textContent);
/** The Sync above the opened account's table (the row's own Sync is an icon button named the same, in the row). */
const detailSync = () => screen.getAllByRole("button", { name: p.table.sync }).filter((button) => button.textContent === p.table.sync);
const busy = (button: HTMLElement) => button.hasAttribute("disabled") || button.getAttribute("aria-busy") === "true" || button.hasAttribute("data-loading");

test("an opened Alibaba Cloud account offers Sync above its table; it syncs the account, is busy meanwhile, and once done the instances and the clusters are both read again", async () => {
  const { seen, user, finish } = mount("/providers?open=p3");
  await groupOf("ack-gpu");
  const [sync] = detailSync();
  assert.ok(sync, "Sync above the table");
  assert.equal(screen.queryAllByRole("button", { name: "Refresh" }).length, 0, "no separate refresh");
  const before = { instances: seen.instanceReads, clusters: seen.clusterReads };
  await user.click(sync);
  assert.deepEqual(seen.synced, [3], "the account is synchronized");
  await waitFor(() => assert.equal(busy(detailSync()[0]), true, "busy while it runs"));
  assert.deepEqual({ instances: seen.instanceReads, clusters: seen.clusterReads }, before, "nothing read before the sync is done");

  await finish(1);
  assert.ok(await screen.findByText("train-01"), "the instances it found are listed");
  assert.ok(await groupOf("ack-new"), "the clusters are read from the cloud again");
  assert.equal(seen.instanceReads, before.instances + 1);
  assert.equal(seen.clusterReads, before.clusters + 1, "once");
  await waitFor(() => assert.equal(busy(detailSync()[0]), false));
  await waitFor(() => assert.deepEqual(toasts(), [p.message.syncDone.replace("{{count}}", "1")], "the same toast as the row's Sync"));
});

test("Sync above the table is pressed with the keyboard too", async () => {
  const { seen, user } = mount("/providers?open=p3");
  await groupOf("ack-gpu");
  detailSync()[0].focus();
  await user.keyboard("{Enter}");
  assert.deepEqual(seen.synced, [3]);
});

test("a failed sync from above the table says why, and the account stays open with what it had", async () => {
  const { seen, user, fail } = mount("/providers?open=p3");
  await groupOf("ack-gpu");
  const instanceReads = seen.instanceReads;
  await user.click(detailSync()[0]);
  await fail(new Error("aliyun: InvalidAccessKeyId"));
  await waitFor(() => assert.deepEqual(toasts(), [p.message.syncFailed.replace("{{detail}}", "aliyun: InvalidAccessKeyId")]));
  await waitFor(() => assert.equal(busy(detailSync()[0]), false, "usable again"));
  assert.ok(await groupOf("ack-gpu"));
  assert.equal(seen.instanceReads, instanceReads, "nothing to read again");
});

test("an opened account of a kind that cannot list instances has no Sync above its table", async () => {
  mount("/providers?open=p5");
  // The toolbar says when the accounts last synced; the opened row of a kind that cannot sync says nothing of it.
  await waitFor(() => assert.equal(screen.getAllByText(/^Last synced: /).length, 1));
  assert.deepEqual(detailSync(), []);
  const row = screen.getAllByRole("row").find((each) => within(each).queryAllByRole("cell")[1]?.textContent === "rack-a") as HTMLElement;
  assert.equal((within(row).getByRole("button", { name: p.table.sync }) as HTMLButtonElement).disabled, true, "nor can the row's own");
});
