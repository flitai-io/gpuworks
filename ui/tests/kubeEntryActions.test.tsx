import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { cloudEntry, kubeEntry, APPLY_COMMAND } from "./helpers/platformFixtures.ts";

// What an administrator does to a cluster entry (ui/src/pages/kubernetes/KubeEntryActions.tsx),
// wherever its buttons sit: the row of a cluster connected on its own, or the
// header of a cluster's group in an account's table. The common actions stay in
// the row (Edit; Apply for an entry made from an account's cluster, Copy the
// connect command for one made by hand) and the rest fold into More, built on
// RowActions (tests/rowActions.test.tsx). The checked apply and removal
// themselves are in tests/providerClusterEntries.test.tsx; the edit drawer the
// Edit button opens is in tests/kubeEdit.test.tsx. Uninstalling by hand, the
// revoke and delete confirmations and the delete confirmation of an entry that
// still has nodes are here.

const en = await createI18n(["kubernetes"], "en");
const zh = await createI18n(["kubernetes"], "zh");
const k = en.locale.kubernetes;
const MORE = en.locale.common.actions.more as string;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: KubeEntryActions } = await import("../src/pages/kubernetes/KubeEntryActions.tsx");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
import type { KubeCluster } from "../src/api";

const original = { ...api };
// The confirmations are queued outside the tree: one a failing test leaves open would open in the next.
afterEach(async () => {
  for (let open = screen.queryByRole("dialog"); open; open = screen.queryByRole("dialog")) {
    const cancel = within(open).queryAllByRole("button", { name: k.actions.cancel }).at(-1) as HTMLButtonElement | undefined;
    if (!cancel || cancel.disabled) break;
    cancel.click();
    await waitFor(() => assert.notEqual(screen.queryByRole("dialog"), open));
  }
  message.destroy(); cleanup(); Object.assign(api, original);
});

const prod = kubeEntry();
// The kubectl commands the server gives an administrator to uninstall an entry gpuworks cannot remove itself.
const REMOVE = ["kubectl delete daemonset gpuworks-agent -n gpuworks-system", "kubectl delete pods -n gpuworks-system -l app.kubernetes.io/part-of=gpuworks"];
const byHand = kubeEntry({ remove_commands: REMOVE });
const retired = kubeEntry({ id: 3, uuid: "c-3", name: "retired", revoked: true });

type Calls = Record<string, unknown[][]>;
/** The entry endpoints the actions call, recording each call; a result that is an Error is thrown (a list is used in turn). */
function stubApi(results: { manifest?: string | Error; revoke?: KubeCluster | Error | Array<KubeCluster | Error>; remove?: Error | Array<Error | undefined> } = {}) {
  const calls: Calls = {};
  const record = (name: string, result: unknown) => async (...args: unknown[]) => {
    (calls[name] ??= []).push(args);
    const now = Array.isArray(result) ? result[calls[name].length - 1] : result;
    if (now instanceof Error) throw now;
    return now;
  };
  api.kubeManifest = record("kubeManifest", results.manifest ?? "apiVersion: apps/v1\nkind: DaemonSet\n") as never;
  api.revokeKubeCluster = record("revokeKubeCluster", results.revoke ?? { ...prod, revoked: true }) as never;
  api.deleteKubeCluster = record("deleteKubeCluster", results.remove) as never;
  return calls;
}

function renderActions(cluster: KubeCluster, language: "en" | "zh" = "en") {
  const changes = { count: 0 };
  // The outcome of an action is said in a toast, and the confirmations are
  // dialogs: the console's frame hosts both.
  const view = render(
    <I18nextProvider i18n={(language === "en" ? en : zh).i18n}>
      <FeedbackHost />
      <KubeEntryActions cluster={cluster} onChanged={() => { changes.count += 1; }} />
    </I18nextProvider>,
  );
  return { view, changes, user: userEvent.setup({ document: dom.window.document }) };
}
const row = () => within(screen.getByRole("group", { name: en.locale.common.resourceTable.actions }));
const rowZh = () => within(screen.getByRole("group", { name: zh.locale.common.resourceTable.actions }));
const button = (name: string) => row().getByRole("button", { name }) as HTMLButtonElement;
/** The buttons left in the row, by name. */
const offered = (scope = row) => scope().getAllByRole("button").map((each) => each.getAttribute("aria-label"));
// More opens on a click, as nodesPage's row menus do (user-event's pointer
// sequence races Base UI's open-on-press under load).
const openMore = async (name = MORE, scope = row) => {
  (scope().getByRole("button", { name }) as HTMLButtonElement).click();
  return within(await screen.findByRole("menu"));
};
const noMenu = () => waitFor(() => assert.equal(screen.queryAllByRole("menu").length, 0, "the menu closed"));
/** The items folded into More, by name, and closes the menu again. */
const folded = async (name = MORE, scope = row) => {
  const menu = await openMore(name, scope);
  const names = menu.getAllByRole("menuitem").map((each) => each.getAttribute("aria-label"));
  fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape", code: "Escape", keyCode: 27 });
  await noMenu();
  return names;
};
/** Chooses an item from More. */
const choose = async (user: ReturnType<typeof userEvent.setup>, name: string) => {
  const menu = await openMore();
  await user.click(menu.getByRole("menuitem", { name }));
};
// user-event does not set keyCode, which the dialogs read; the focused element receives the key.
const dialog = () => within(screen.getByRole("dialog"));
const confirmation = async () => within(await screen.findByRole("dialog"));
const noDialog = () => waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the dialog closed"));
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });
const names = (...keys: Array<keyof typeof k.actions>) => keys.map((key) => k.actions[key]);

test("an entry made by hand keeps Edit and Copy connect command in the row and folds the rest into More, the destructive ones last", async () => {
  stubApi();
  renderActions(prod);
  assert.deepEqual(offered(), [...names("edit", "copyCommand"), MORE]);
  assert.deepEqual(await folded(), names("manifest", "revoke", "delete"));
  const menu = await openMore();
  assert.equal(menu.getAllByRole("separator").length, 1, "the destructive ones sit under a rule");
  assert.deepEqual(menu.getAllByRole("menuitem").map((each) => each.className.includes("text-fg-danger")), [false, true, true], "and are drawn red");
});

test("an entry made from an account's cluster keeps Edit and Apply in the row; the connect command, manifest and Remove fold into More", async () => {
  stubApi();
  renderActions(cloudEntry());
  assert.deepEqual(offered(), [...names("edit", "apply"), MORE]);
  assert.deepEqual(await folded(), names("copyCommand", "manifest", "remove", "revoke", "delete"));
});

test("Uninstall is offered in More when gpuworks cannot remove an entry and the server gives the commands; an entry sent without a command has no Copy", async () => {
  stubApi();
  const each = async (cluster: KubeCluster) => {
    const view = renderActions(cluster);
    const result = { row: offered(), menu: await folded() };
    view.view.unmount();
    return result;
  };
  assert.deepEqual(await each(byHand), { row: [...names("edit", "copyCommand"), MORE], menu: names("manifest", "uninstall", "revoke", "delete") });
  assert.deepEqual(await each(cloudEntry({ remove_commands: REMOVE })), { row: [...names("edit", "apply"), MORE], menu: names("copyCommand", "manifest", "remove", "revoke", "delete") },
    "gpuworks removes it itself: its own remove flow, no commands to run");
  assert.deepEqual(await each(cloudEntry({ remove_commands: REMOVE, can_remove: false, can_apply: false, operation_blocked: "private_api" })),
    { row: [...names("edit", "apply"), MORE], menu: names("copyCommand", "manifest", "remove", "uninstall", "revoke", "delete") }, "a cloud entry gpuworks cannot reach is uninstalled by hand");
  // The server sends the command, which carries the token, to administrators alone.
  assert.deepEqual(await each({ ...prod, apply_command: undefined }), { row: [...names("edit"), MORE], menu: names("manifest", "revoke", "delete") });
});

test("the connect command of an entry made by hand is copied from its button and says so; a revoked entry's cannot be", async () => {
  stubApi();
  const first = renderActions(prod);
  await first.user.click(button(k.actions.copyCommand));
  assert.equal(await navigator.clipboard.readText(), APPLY_COMMAND);
  assert.ok(await screen.findByText(k.actions.commandCopied));
  first.view.unmount();

  renderActions(retired);
  assert.equal(button(k.actions.copyCommand).disabled, true, "a revoked token connects nothing");
});

test("the connect command of a cloud entry is copied from More and says so in the console's language", async () => {
  stubApi();
  const { user } = renderActions(cloudEntry(), "zh");
  const menu = await openMore("更多", rowZh);
  await user.click(menu.getByRole("menuitem", { name: "复制接入命令" }));
  assert.equal(await navigator.clipboard.readText(), APPLY_COMMAND);
  assert.ok(await screen.findByText("接入命令已复制"));
});

test("the manifest opens in a dialog with a copy button and closes from the keyboard; a failed load says so", async () => {
  const calls = stubApi();
  const first = renderActions(prod);
  await choose(first.user, k.actions.manifest);
  const modal = within(await screen.findByRole("dialog"));
  assert.ok(modal.getByText(k.manifest.title.replace("{{name}}", "prod-gpu")));
  assert.ok(await modal.findByText(/kind: DaemonSet/));
  assert.deepEqual(calls.kubeManifest, [["c-1"]]);
  await first.user.click(modal.getByRole("button", { name: k.manifest.copy }));
  assert.equal(await navigator.clipboard.readText(), "apiVersion: apps/v1\nkind: DaemonSet\n");
  escape();
  await noDialog();
  first.view.unmount();

  stubApi({ manifest: new Error("manifest 404") });
  const second = renderActions(prod);
  await choose(second.user, k.actions.manifest);
  const failed = within(await screen.findByRole("dialog"));
  assert.ok(await failed.findByText(k.manifest.failed));
  assert.ok(failed.getByText("manifest 404"));
  assert.equal(failed.queryAllByRole("button", { name: k.manifest.copy }).length, 0, "nothing to copy");
  await second.user.click(failed.getAllByRole("button", { name: k.manifest.close }).at(-1) as HTMLElement);
  await noDialog();
});

test("revoking asks in a dialog, says what it does, calls the API for that entry only on confirm and tells its owner; a revoked entry offers no second revoke", async () => {
  const calls = stubApi();
  const first = renderActions(prod);
  await choose(first.user, k.actions.revoke);
  const ask = await confirmation();
  assert.ok(ask.getByText(k.actions.revokeConfirm.replace("{{name}}", "prod-gpu")));
  assert.ok(ask.getByText(k.actions.revokeHelp));
  assert.equal(calls.revokeKubeCluster, undefined, "asking is not revoking");
  const ok = ask.getByRole("button", { name: k.actions.revoke });
  assert.ok(ok.querySelector('[class*="bg-destructive-action"]'), "the confirming button is red");
  await first.user.click(ok);
  await waitFor(() => assert.deepEqual(calls.revokeKubeCluster, [["c-1"]]));
  await waitFor(() => assert.equal(first.changes.count, 1));
  assert.ok(await screen.findByText(k.actions.revoked.replace("{{name}}", "prod-gpu")));
  await noDialog();
  first.view.unmount();

  renderActions(retired);
  const menu = await openMore();
  assert.equal(menu.getByRole("menuitem", { name: k.actions.revoke }).getAttribute("aria-disabled"), "true");
});

test("cancelling the revoke dialog calls nothing; Escape cancels it too", async () => {
  const calls = stubApi();
  const { user, changes } = renderActions(prod);
  await choose(user, k.actions.revoke);
  await user.click((await confirmation()).getByRole("button", { name: k.actions.cancel }));
  await noDialog();
  await choose(user, k.actions.revoke);
  await confirmation();
  escape();
  await noDialog();
  assert.equal(calls.revokeKubeCluster, undefined);
  assert.equal(changes.count, 0);
});

test("a failed revoke keeps the dialog open on the reason; it can be tried again, and cancelled", async () => {
  const calls = stubApi({ revoke: [new Error("token store unavailable"), { ...prod, revoked: true }] });
  const { user, changes } = renderActions(prod);
  await choose(user, k.actions.revoke);
  await user.click((await confirmation()).getByRole("button", { name: k.actions.revoke }));
  const alert = await dialog().findByRole("alert");
  assert.equal(alert.textContent, `${k.actions.failed}: token store unavailable`);
  assert.equal(changes.count, 0, "nothing changed");
  await waitFor(() => assert.equal((dialog().getByRole("button", { name: k.actions.revoke }) as HTMLButtonElement).disabled, false));
  await user.click(dialog().getByRole("button", { name: k.actions.revoke }));
  await noDialog();
  assert.equal(calls.revokeKubeCluster.length, 2, "tried again");
  assert.equal(changes.count, 1);

  stubApi({ revoke: new Error("token store unavailable") });
  await choose(user, k.actions.revoke);
  await user.click((await confirmation()).getByRole("button", { name: k.actions.revoke }));
  await dialog().findByRole("alert");
  await user.click(dialog().getByRole("button", { name: k.actions.cancel }));
  await noDialog();
  assert.equal(changes.count, 1);
});

test("deleting asks in a dialog, says what stays in the cluster, calls the API on confirm and tells its owner", async () => {
  const calls = stubApi();
  const { user, changes } = renderActions(cloudEntry());
  await choose(user, k.actions.delete);
  const ask = await confirmation();
  assert.ok(ask.getByText(k.actions.deleteConfirm.replace("{{name}}", "ack-gpu")));
  assert.ok(ask.getByText(k.actions.deleteHelp));
  assert.equal(ask.queryAllByText(/cannot be deleted yet/).length, 0, "an entry without nodes just confirms");
  assert.deepEqual(ask.getAllByRole("button").map((each) => each.textContent).filter((name) => name !== en.locale.common.actions.close), names("cancel", "delete"));
  assert.equal(calls.deleteKubeCluster, undefined);
  const ok = ask.getByRole("button", { name: k.actions.delete });
  assert.ok(ok.querySelector('[class*="bg-destructive-action"]'), "the confirming button is red");
  await user.click(ok);
  await waitFor(() => assert.deepEqual(calls.deleteKubeCluster, [["e-5"]]));
  await waitFor(() => assert.equal(changes.count, 1));
  assert.ok(await screen.findByText(k.actions.deleted.replace("{{name}}", "ack-gpu")));
  await noDialog();
});

test("cancelling the delete dialog calls nothing", async () => {
  const calls = stubApi();
  const { user, changes } = renderActions(prod);
  await choose(user, k.actions.delete);
  await user.click((await confirmation()).getByRole("button", { name: k.actions.cancel }));
  await noDialog();
  assert.equal(calls.deleteKubeCluster, undefined);
  assert.equal(changes.count, 0);
});

test("an entry that still has nodes cannot be deleted: the product error stays in the dialog, its owner is not told of a change, and it can be tried again", async () => {
  const calls = stubApi({ remove: [new APIError("KUBE_CLUSTER_HAS_NODES", undefined, "cluster has nodes", 409), undefined] });
  const { user, changes } = renderActions(prod);
  await choose(user, k.actions.delete);
  await user.click((await confirmation()).getByRole("button", { name: k.actions.delete }));
  const alert = await dialog().findByRole("alert");
  assert.equal(alert.textContent, `${k.actions.failed}: ${en.locale.common.apiError.KUBE_CLUSTER_HAS_NODES}`);
  assert.deepEqual(calls.deleteKubeCluster, [["c-1"]]);
  assert.equal(changes.count, 0, "the entry is still there");
  await waitFor(() => assert.equal((dialog().getByRole("button", { name: k.actions.delete }) as HTMLButtonElement).disabled, false, "and can be tried again"));
  await user.click(dialog().getByRole("button", { name: k.actions.delete }));
  await noDialog();
  assert.equal(calls.deleteKubeCluster.length, 2);
  assert.equal(changes.count, 1);
});

test("applying and removing are offered for an entry made from an account's cluster and follow the server's can_apply and can_remove, with its reason", async () => {
  stubApi();
  const state = async (cluster: KubeCluster) => {
    const view = renderActions(cluster);
    const apply = row().queryAllByRole("button", { name: k.actions.apply }).map((each) => (each as HTMLButtonElement).disabled);
    const menu = await openMore();
    const remove = menu.queryAllByRole("menuitem", { name: k.actions.remove }).map((each) => each.getAttribute("aria-disabled") === "true");
    view.view.unmount();
    return [apply, remove];
  };
  assert.deepEqual(await state(cloudEntry()), [[false], [false]]);
  assert.deepEqual(await state(cloudEntry({ can_apply: false, can_remove: false, operation_blocked: "private_api" })), [[true], [true]]);
  assert.deepEqual(await state(cloudEntry({ revoked: true, can_apply: false, can_remove: true, operation_blocked: "revoked" })), [[true], [false]], "a revoked entry can still be removed from its cluster");
  assert.deepEqual(await state(prod), [[], []], "gpuworks has no way into a cluster connected by hand");

  const { user } = renderActions(cloudEntry({ can_apply: false, can_remove: false, operation_blocked: "private_api" }));
  await user.hover(button(k.actions.apply).parentElement as HTMLElement);
  assert.ok(await screen.findByText(k.blocked.private_api), "the reason is the server's code, written in this language");
  const menu = await openMore();
  assert.equal(menu.getByRole("menuitem", { name: k.actions.remove }).getAttribute("title"), k.blocked.private_api, "a disabled Remove says the same reason");
});

test("Apply and Remove from More open the checked operation", async () => {
  stubApi();
  api.kubeClusterOperation = (async (_uuid: string, _op: string, dryRun: boolean) => ({ dry_run: dryRun, results: [] })) as never;
  const { user } = renderActions(cloudEntry());
  await user.click(button(k.actions.apply));
  assert.ok(await screen.findByText(k.operation.applyTitle.replace("{{name}}", "ack-gpu")));
  escape();
  await noDialog();
  await choose(user, k.actions.remove);
  assert.ok(await screen.findByText(k.operation.removeTitle.replace("{{name}}", "ack-gpu")));
});

test("in zh the actions are named in Chinese and the confirmation asks in Chinese", async () => {
  stubApi();
  const { user } = renderActions(cloudEntry(), "zh");
  assert.deepEqual(offered(rowZh), ["编辑", "应用到集群", "更多"]);
  assert.deepEqual(await folded("更多", rowZh), ["复制接入命令", "查看 manifest", "从集群卸载", "吊销", "删除"]);
  const menu = await openMore("更多", rowZh);
  await user.click(menu.getByRole("menuitem", { name: "吊销" }));
  const ask = await confirmation();
  assert.ok(ask.getByText("吊销 ack-gpu 的接入令牌？"));
  assert.ok(ask.getByText("已接入的节点不受影响，新节点不能再用这条命令接入。"));
  await user.click(ask.getByRole("button", { name: "取消" }));
  await noDialog();
});

test("Uninstall lists the commands that remove the agents and the pods they started, and what follows; it sends nothing and closes from the keyboard", async () => {
  const calls = stubApi();
  const { user } = renderActions(byHand);
  await choose(user, k.actions.uninstall);
  const modal = await confirmation();
  assert.ok(await modal.findByText(k.uninstall.title.replace("{{name}}", "prod-gpu")));
  assert.ok(modal.getByText(k.uninstall.intro));
  assert.ok(modal.getByText(k.uninstall.next));
  assert.equal(modal.getByLabelText("Command 1").textContent, REMOVE[0]);
  assert.equal(modal.getByLabelText("Command 2").textContent, REMOVE[1]);
  await user.click(modal.getByRole("button", { name: "Copy command 2" }));
  assert.equal(await navigator.clipboard.readText(), REMOVE[1]);
  assert.deepEqual(Object.keys(calls), [], "nothing is sent from the modal");
  escape();
  await noDialog();
  await choose(user, k.actions.uninstall);
  await dialog().findByLabelText("Command 1");
  await user.click(dialog().getAllByRole("button", { name: k.uninstall.close }).at(-1) as HTMLElement);
  await noDialog();
});

test("a revoked entry can still be uninstalled by hand", async () => {
  stubApi();
  const { user } = renderActions({ ...byHand, revoked: true });
  const menu = await openMore();
  assert.equal(menu.getByRole("menuitem", { name: k.actions.uninstall }).getAttribute("aria-disabled"), null);
  await user.click(menu.getByRole("menuitem", { name: k.actions.uninstall }));
  assert.ok(await dialog().findByLabelText("Command 1"));
});

test("deleting an entry that has nodes says it cannot be deleted yet and offers Uninstall when gpuworks cannot remove it, which opens the commands", async () => {
  const calls = stubApi();
  const { user } = renderActions({ ...byHand, node_count: 4 });
  await choose(user, k.actions.delete);
  const ask = await confirmation();
  assert.ok(ask.getByText(k.actions.deleteHasNodes_other.replace("{{count}}", "4")));
  assert.ok(ask.getByText(k.actions.deleteHelp));
  assert.equal(ask.queryAllByRole("button", { name: k.actions.remove }).length, 0);
  await user.click(ask.getByRole("button", { name: k.actions.uninstall }));
  assert.equal((await screen.findByLabelText("Command 1")).textContent, REMOVE[0], "the uninstall commands open");
  assert.equal(screen.queryAllByText(k.actions.deleteConfirm.replace("{{name}}", "prod-gpu")).length, 0, "in place of the delete dialog");
  assert.equal(calls.deleteKubeCluster, undefined, "nothing was deleted");
});

test("deleting an entry with nodes that gpuworks can remove offers its checked remove flow", async () => {
  const calls = stubApi();
  api.kubeClusterOperation = (async (_uuid: string, _op: string, dryRun: boolean) => ({ dry_run: dryRun, results: [{ kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "delete" }] })) as never;
  const { user } = renderActions(cloudEntry({ node_count: 1, vendors: [{ vendor: "nvidia", nodes: 1 }] }));
  await choose(user, k.actions.delete);
  const ask = await confirmation();
  assert.ok(ask.getByText(k.actions.deleteHasNodes_one.replace("{{count}}", "1")));
  assert.equal(ask.queryAllByRole("button", { name: k.actions.uninstall }).length, 0);
  await user.click(ask.getByRole("button", { name: k.actions.remove }));
  assert.ok(await screen.findByText(k.operation.removeTitle.replace("{{name}}", "ack-gpu")));
  assert.equal(screen.queryAllByText(k.actions.deleteConfirm.replace("{{name}}", "ack-gpu")).length, 0, "in place of the delete dialog");
  assert.equal(calls.deleteKubeCluster, undefined);
});

test("deleting an entry with nodes and no way to uninstall it from here only says so", async () => {
  stubApi();
  const { user } = renderActions(prod);
  await choose(user, k.actions.delete);
  const ask = await confirmation();
  assert.ok(ask.getByText(k.actions.deleteHasNodes_other.replace("{{count}}", "3")));
  assert.deepEqual(ask.getAllByRole("button").map((each) => each.textContent).filter((name) => name !== en.locale.common.actions.close), names("cancel", "delete"), "nothing to uninstall with");
  await user.click(ask.getByRole("button", { name: k.actions.cancel }));
  await noDialog();
});
