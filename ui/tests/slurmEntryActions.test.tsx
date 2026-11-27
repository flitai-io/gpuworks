import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { REMOVE_CHECK_COMMAND, UNINSTALL_AGENTS_COMMAND, ehpcEntry, slurmEntry, SLURM_DEFAULTS, SETUP_COMMAND } from "./helpers/platformFixtures.ts";
import { chooseRowAction, rowActions } from "./helpers/rowActions.ts";

// What an administrator does to a Slurm entry (ui/src/pages/slurm/SlurmEntryActions.tsx):
// the actions it offers by what the server says of the entry, revoking, the
// two-step uninstall by hand and by the platform (SlurmUninstallModal.tsx),
// and deleting, with the nodes when the server allows it (SlurmDeleteDialog.tsx).

const en = await createI18n(["slurm"], "en");
const s = en.locale.slurm;
const MORE = en.locale.common.actions.more as string;
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: SlurmEntryActions } = await import("../src/pages/slurm/SlurmEntryActions.tsx");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
import type { SlurmApplyCheck, SlurmApplyOutcome, SlurmCluster, SlurmRemoveOutcome, SlurmRemoveStep } from "../src/slurmApi";

const original = { ...api };
afterEach(async () => {
  for (let open = screen.queryByRole("dialog"); open; open = screen.queryByRole("dialog")) {
    const cancel = within(open).queryAllByRole("button", { name: /^(Cancel|Close)$/ }).at(-1) as HTMLButtonElement | undefined;
    if (!cancel || cancel.disabled) break;
    cancel.click();
    // Compare first: an assert given two DOM nodes formats both into its
    // message on every poll that fails, which held each poll for seconds.
    await waitFor(() => assert.ok(screen.queryByRole("dialog") !== open, "the confirmation closed"));
  }
  message.destroy(); cleanup(); Object.assign(api, original);
});

type Calls = Record<string, unknown[][]>;
function stubApi(results: { remove?: (step: SlurmRemoveStep) => SlurmRemoveOutcome | Error; del?: Error; revoke?: Error } = {}) {
  const calls: Calls = {};
  api.slurmDefaults = (async () => SLURM_DEFAULTS) as never;
  api.deleteSlurmCluster = (async (...args: unknown[]) => { (calls.delete ??= []).push(args); if (results.del) throw results.del; }) as never;
  api.revokeSlurmCluster = (async (...args: unknown[]) => { (calls.revoke ??= []).push(args); if (results.revoke) throw results.revoke; return slurmEntry({ revoked: true }); }) as never;
  api.removeSlurmCluster = (async (...args: unknown[]) => {
    (calls.remove ??= []).push(args);
    const answer = results.remove?.(args[1] as SlurmRemoveStep);
    if (answer instanceof Error) throw answer;
    return answer;
  }) as never;
  return calls;
}
const outcome = (step: SlurmRemoveStep, over: Partial<SlurmRemoveOutcome> = {}): SlurmRemoveOutcome => ({
  step, status: "ok", manager_instance: "i-mgr", wait_seconds: 0, next_step: step === "uninstall_agents" ? "remove_check" : "", cluster: slurmEntry(), ...over,
});

function renderActions(cluster: SlurmCluster) {
  const changes = { count: 0 };
  render(<I18nextProvider i18n={en.i18n}><FeedbackHost /><SlurmEntryActions cluster={cluster} onChanged={() => { changes.count += 1; }} /></I18nextProvider>);
  return { changes, user: userEvent.setup({ document: dom.window.document }) };
}
const group = () => screen.getByRole("group", { name: en.locale.common.resourceTable.actions });
const dialog = async () => within(await screen.findByRole("dialog"));
const noDialog = () => waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the dialog closed"));
// The key goes to the open dialog itself: right after one opens, focus may not have reached it yet.
const escape = () => fireEvent.keyDown(screen.queryAllByRole("dialog").at(-1) ?? document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });

test("an entry made from a cloud account's cluster offers Edit and Check and apply in its row, and folds Copy, View script, Uninstall, Revoke and Delete into More", async () => {
  stubApi();
  renderActions(ehpcEntry());
  assert.deepEqual(await rowActions(group().parentElement as HTMLElement), {
    shown: [s.actions.edit, s.actions.apply, MORE],
    folded: [s.actions.copyCommand, s.actions.script, s.actions.uninstall, s.actions.revoke, s.actions.delete],
  });
});

test("an entry made by hand has no Check and apply to offer: the command is its way in", async () => {
  stubApi();
  renderActions(slurmEntry());
  const names = await rowActions(group().parentElement as HTMLElement);
  assert.deepEqual(names.shown.slice(0, 2), [s.actions.edit, s.actions.copyCommand]);
  assert.equal([...names.shown, ...names.folded].includes(s.actions.apply), false);
});

// What the server says the platform cannot do is shown, not hidden: the action stays in the row, disabled, and says why.
for (const reason of ["no_provider", "provider_unsupported", "cluster_unknown", "cluster_listing_pending", "cluster_listing_failed", "not_slurm_cluster", "revoked"] as const) {
  test(`Check and apply is shown disabled with the server's reason (${reason}) when the platform cannot apply`, async () => {
    stubApi();
    const { user } = renderActions(ehpcEntry({ can_apply: false, can_remove: reason === "revoked", operation_blocked: reason, revoked: reason === "revoked", setup_command: reason === "revoked" ? undefined : SETUP_COMMAND }));
    const apply = within(group()).getByRole("button", { name: s.actions.apply }) as HTMLButtonElement;
    assert.equal(apply.disabled || apply.getAttribute("aria-disabled") === "true", true);
    await user.hover(apply.parentElement as HTMLElement);
    assert.ok(await screen.findByText(s.blocked[reason]), "the reason is the server's code, written in this language");
    await user.click(apply);
    assert.equal(screen.queryAllByRole("dialog").length, 0, "a disabled action opens nothing");
  });
}

test("a revoked entry has no setup command to copy and cannot be revoked again", async () => {
  stubApi();
  renderActions(slurmEntry({ revoked: true, setup_command: undefined }));
  const names = await rowActions(group().parentElement as HTMLElement);
  assert.deepEqual(names.folded, [s.actions.script, s.actions.uninstall, s.actions.revoke, s.actions.delete]);
  const revoke = within(await (async () => { (within(group()).getByRole("button", { name: MORE }) as HTMLButtonElement).click(); return screen.findByRole("menu"); })()).getByRole("menuitem", { name: s.actions.revoke });
  assert.equal(revoke.getAttribute("aria-disabled") === "true" || (revoke as HTMLButtonElement).disabled, true);
});

test("Copy puts the setup command on the clipboard", async () => {
  stubApi();
  const { user } = renderActions(slurmEntry());
  await user.click(within(group()).getByRole("button", { name: s.actions.copyCommand }));
  assert.equal(await navigator.clipboard.readText(), SETUP_COMMAND);
  assert.ok(await screen.findByText(s.actions.commandCopied));
});

test("revoking asks first, says nothing is lost for the nodes already connected, and revokes only on the confirmation; a refusal stays in the dialog", async () => {
  const calls = stubApi();
  const { changes, user } = renderActions(slurmEntry());
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.revoke);
  let asked = await dialog();
  assert.ok(asked.getByText(s.actions.revokeConfirm.replace("{{name}}", "hpc-lab")));
  assert.ok(asked.getByText(s.actions.revokeNote));
  await user.click(asked.getByRole("button", { name: s.actions.cancel }));
  await noDialog();
  assert.equal(calls.revoke, undefined);
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.revoke);
  asked = await dialog();
  await user.click(asked.getByRole("button", { name: s.actions.revoke }));
  await waitFor(() => assert.deepEqual(calls.revoke, [["s-1"]]));
  assert.ok(await screen.findByText(s.actions.revoked.replace("{{name}}", "hpc-lab")));
  assert.equal(changes.count, 1);
});

test("Uninstall of an entry the platform cannot reach shows the two commands in order, with the wait between them, and no button that runs anything", async () => {
  const calls = stubApi();
  const { user } = renderActions(slurmEntry());
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.uninstall);
  const d = await dialog();
  assert.ok(d.getByText(s.uninstall.intro));
  assert.equal(d.getByLabelText(s.uninstall.step1, { selector: "pre" }).textContent, UNINSTALL_AGENTS_COMMAND);
  assert.equal(d.getByLabelText(s.uninstall.step2, { selector: "pre" }).textContent, REMOVE_CHECK_COMMAND);
  assert.ok(await d.findByText("Then wait one health-check interval (5 min) while every node uninstalls its agent."));
  assert.equal(d.queryAllByRole("button", { name: s.uninstall.run.uninstall_agents }).length, 0);
  await user.click(d.getByRole("button", { name: s.command.copy.replace("{{title}}", s.uninstall.step1) }));
  assert.equal(await navigator.clipboard.readText(), UNINSTALL_AGENTS_COMMAND);
  assert.equal(calls.remove, undefined, "nothing is sent to the cluster from here");
  escape();
  await noDialog();
});

test("Uninstall of an entry the platform can reach runs the agents step, counts down the server's wait, then runs the step the server names and offers deleting the entry", async () => {
  const calls = stubApi({ remove: (step) => step === "uninstall_agents" ? outcome(step, { wait_seconds: 1 }) : outcome(step) });
  const { changes, user } = renderActions(ehpcEntry({ node_count: 2 }));
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.uninstall);
  const d = await dialog();
  assert.ok(d.getByText(s.uninstall.byGpuworksNote));
  await user.click(d.getByRole("button", { name: s.uninstall.run.uninstall_agents }));
  assert.deepEqual(calls.remove, [["s-7", "uninstall_agents"]]);
  assert.ok(await d.findByText(s.uninstall.agentsSent));
  assert.ok(await d.findByText(s.uninstall.waiting.replace("{{seconds}}", "1")));
  const next = d.getByRole("button", { name: s.uninstall.run.remove_check }) as HTMLButtonElement;
  assert.equal(next.disabled, true, "the server's wait has not run out");
  await waitFor(() => assert.equal((d.getByRole("button", { name: s.uninstall.run.remove_check }) as HTMLButtonElement).disabled, false));
  await user.click(d.getByRole("button", { name: s.uninstall.run.remove_check }));
  assert.deepEqual(calls.remove.at(-1), ["s-7", "remove_check"]);
  assert.ok(await d.findByText(s.uninstall.removed));
  assert.equal(d.queryAllByRole("button", { name: s.uninstall.run.remove_check }).length, 0);
  assert.equal(changes.count, 2, "the list is read again after each step");
  await user.click(d.getByRole("button", { name: s.uninstall.delete }));
  assert.ok(await screen.findByText(s.delete.title.replace("{{name}}", "ehpc-lab")));
  escape();
  await noDialog();
});

test("a refused uninstall step is said in the operator's language and can be tried again; a permission the cloud key lacks is named", async () => {
  let attempt = 0;
  const calls = stubApi({ remove: (step) => ++attempt === 1 ? new APIError("SLURM_APPLY_PERMISSION_MISSING", { permission: "ecs:RunCommand" }, "x", 424) : outcome(step) });
  const { user } = renderActions(ehpcEntry());
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.uninstall);
  const d = await dialog();
  await user.click(d.getByRole("button", { name: s.uninstall.run.uninstall_agents }));
  assert.ok(await d.findByText(/may not call ecs:RunCommand/));
  await user.click(d.getByRole("button", { name: s.uninstall.run.uninstall_agents }));
  await waitFor(() => assert.equal(calls.remove.length, 2));
  assert.ok(await d.findByText(s.uninstall.agentsSent));
  escape();
  await noDialog();
});

test("deleting an entry with no node is plain: confirm and the entry goes", async () => {
  const calls = stubApi();
  const { changes, user } = renderActions(slurmEntry({ node_count: 0, slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [] }));
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.delete);
  const d = await dialog();
  assert.ok(d.getByText(s.delete.title.replace("{{name}}", "hpc-lab")));
  assert.equal(d.queryAllByRole("checkbox").length, 0);
  await user.click(d.getByRole("button", { name: s.delete.confirm }));
  await waitFor(() => assert.deepEqual(calls.delete, [["s-1", false]]));
  await noDialog();
  assert.equal(changes.count, 1);
});

test("an entry with nodes can be deleted only together with them, and the option is the server's: enabled when allowed, the delete button waiting for it to be chosen", async () => {
  const calls = stubApi();
  const { user } = renderActions(slurmEntry({ node_count: 3, delete_with_nodes_allowed: true, delete_with_nodes_blocker: "" }));
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.delete);
  const d = await dialog();
  assert.ok(d.getByText("This cluster still has 3 nodes."));
  const option = d.getByRole("checkbox", { name: "Delete the cluster together with its 3 nodes" });
  assert.equal(option.getAttribute("aria-disabled"), null);
  const confirm = d.getByRole("button", { name: s.delete.confirm }) as HTMLButtonElement;
  assert.equal(confirm.disabled, true);
  await user.click(option);
  assert.equal(confirm.disabled, false);
  await user.click(confirm);
  await waitFor(() => assert.deepEqual(calls.delete, [["s-1", true]]));
});

for (const [blocker, text] of [["online", s.delete.blocker.online], ["instances", s.delete.blocker.instances]] as const) {
  test(`when the server says deleting with the nodes would fail (${blocker}), the option is shown disabled with its reason and nothing can be deleted`, async () => {
    const calls = stubApi();
    const { user } = renderActions(slurmEntry({ node_count: 3, delete_with_nodes_allowed: false, delete_with_nodes_blocker: blocker }));
    await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.delete);
    const d = await dialog();
    const option = d.getByRole("checkbox", { name: /Delete the cluster together with/ });
    assert.equal(option.getAttribute("aria-disabled"), "true");
    assert.ok(d.getByText(text));
    assert.equal((d.getByRole("button", { name: s.delete.confirm }) as HTMLButtonElement).disabled, true);
    assert.equal(calls.delete, undefined);
    escape();
    await noDialog();
  });
}

test("a refusal of the delete is shown in the dialog and the dialog stays; the way to uninstall first opens the uninstall dialog", async () => {
  const refusal = new APIError("SLURM_CLUSTER_NODES_ONLINE", { count: 2 }, "online", 409);
  stubApi({ del: refusal });
  const { user } = renderActions(slurmEntry({ node_count: 3, delete_with_nodes_allowed: true, delete_with_nodes_blocker: "" }));
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.delete);
  let d = await dialog();
  await user.click(d.getByRole("checkbox", { name: /Delete the cluster together with/ }));
  await user.click(d.getByRole("button", { name: s.delete.confirm }));
  assert.ok(await d.findByText(/2 of its nodes still have an agent connected/));
  await user.click(d.getByRole("button", { name: s.delete.uninstall }));
  await waitFor(() => assert.ok(screen.getByText(s.uninstall.title.replace("{{name}}", "hpc-lab"))));
  escape();
  await noDialog();
});

test("Escape closes the delete dialog and Tab reaches its controls in order", async () => {
  const calls = stubApi();
  const { user } = renderActions(slurmEntry({ node_count: 3, delete_with_nodes_allowed: true, delete_with_nodes_blocker: "" }));
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.delete);
  await dialog();
  const order: string[] = [];
  for (let i = 0; i < 6 && order.length < 4; i += 1) {
    await user.tab();
    const name = document.activeElement?.getAttribute("aria-label") || document.activeElement?.textContent || document.activeElement?.getAttribute("role") || "";
    if (name && !order.includes(name)) order.push(name);
  }
  assert.ok(order.includes(s.delete.uninstall) && order.includes(s.delete.cancel), order.join("|"));
  escape();
  await noDialog();
  assert.equal(calls.delete, undefined);
});

const gres = { added: ["gpu000"], present: [], no_gpu_gres: [] };
const plan = (over: Partial<SlurmApplyCheck> = {}): SlurmApplyCheck => ({
  dry_run: true, status: "ok", manager_instance: "i-mgr", slurm_cluster: "demo",
  gres: { ...gres, would_add: [{ node: "gpu000", files: "/dev/nvidia0", line: "NodeName=gpu000 Name=gpu File=/dev/nvidia0" }] },
  script: { check_script: "/opt/gpuworks-slurm/check.sh", token_file: "/opt/gpuworks-slurm/token", shared: false, fstype: "ext4", copy_to: ["gpu000", "gpu001"], copy_files: ["check.sh", "token"] },
  conf: { path: "/etc/slurm/slurm.conf", changed: true, interval: 300, node_state: "ANY", block: ["HealthCheckProgram=/opt/gpuworks-slurm/check.sh", "HealthCheckInterval=300", "HealthCheckNodeState=ANY"], commented: [], includes: [] },
  configless: true, rearm: false, resume_nodes: [], warnings: [], reach: { hosts: [] }, cluster: ehpcEntry(), ...over,
});
const done = (): SlurmApplyOutcome => ({
  status: "ok", manager_instance: "i-mgr", steps: [{ id: "gres", status: "pass" }], gres, distribute: { shared: true, ok: 0, failed: [] },
  resumed_nodes: [], resume_failed_nodes: [], reach: { hosts: [] }, cluster: ehpcEntry(),
});

test("Edit opens the entry in a drawer, and Check and apply opens a dialog that checks first, so nothing has run until the operator confirms", async () => {
  stubApi();
  const calls: Calls = {};
  api.checkSlurmCluster = (async (...args: unknown[]) => { (calls.check ??= []).push(args); return plan(); }) as never;
  api.applySlurmCluster = (async (...args: unknown[]) => { (calls.apply ??= []).push(args); return done(); }) as never;
  const { changes, user } = renderActions(ehpcEntry());
  await user.click(within(group()).getByRole("button", { name: s.actions.edit }));
  assert.ok(await screen.findByRole("heading", { name: s.form.editTitle }));
  await user.click((await dialog()).getByRole("button", { name: s.form.cancel }));
  await noDialog();
  await user.click(within(group()).getByRole("button", { name: s.actions.apply }));
  const d = await dialog();
  assert.ok(await d.findByRole("heading", { name: s.operation.title.replace("{{name}}", "ehpc-lab") }));
  assert.ok(await d.findByText(s.operation.checked));
  assert.deepEqual(calls.check, [["s-7"]]);
  assert.equal(calls.apply, undefined, "the check changed and ran nothing");
  assert.equal((d.getByRole("button", { name: s.operation.confirm }) as HTMLButtonElement).disabled, false);
  await user.click(d.getByRole("button", { name: s.operation.confirm }));
  assert.ok(await d.findByText(s.apply.done));
  assert.deepEqual(calls.apply, [["s-7"]]);
  assert.equal(changes.count, 1);
  await user.click(d.getAllByRole("button", { name: s.operation.close }).at(-1) as HTMLElement);
  await noDialog();
});

test("View script shows the setup script the controller is served, with a note that the token is left out, and can be copied", async () => {
  stubApi();
  api.slurmClusterScript = (async (uuid: string) => `#!/bin/sh\nTOKEN='REDACTED-ENROLLMENT-TOKEN'\n# for ${uuid}\n`) as never;
  const { user } = renderActions(ehpcEntry());
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.script);
  const d = await dialog();
  assert.ok(await d.findByText(s.script.tokenHidden));
  assert.equal(d.getByLabelText(s.script.title.replace("{{name}}", "ehpc-lab"), { selector: "pre" }).textContent, "#!/bin/sh\nTOKEN='REDACTED-ENROLLMENT-TOKEN'\n# for s-7\n");
  assert.equal(document.body.textContent?.includes("secret-token"), false, "no enrollment token is on the screen");
  await user.click(d.getByRole("button", { name: s.script.copy }));
  assert.ok((await navigator.clipboard.readText()).startsWith("#!/bin/sh"));
  await user.click(d.getAllByRole("button", { name: s.script.close }).at(-1) as HTMLElement);
  await noDialog();
});

test("a script that cannot be read says so with the reason", async () => {
  stubApi();
  api.slurmClusterScript = (async () => { throw new Error("boom"); }) as never;
  const { user } = renderActions(ehpcEntry());
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.script);
  const d = await dialog();
  assert.ok(await d.findByText(s.script.failed));
  assert.ok(d.getByText("boom"));
  escape();
  await noDialog();
});

test("a revoked entry the platform can still reach offers its uninstall steps itself: revoking refuses applying, not removing", async () => {
  const calls = stubApi({ remove: (step) => outcome(step) });
  const { user } = renderActions(ehpcEntry({ revoked: true, can_apply: false, can_remove: true, operation_blocked: "revoked", setup_command: undefined, check_command: undefined }));
  await chooseRowAction(user, group().parentElement as HTMLElement, s.actions.uninstall);
  const d = await dialog();
  assert.ok(d.getByText(s.uninstall.byGpuworksNote));
  await user.click(d.getByRole("button", { name: s.uninstall.run.uninstall_agents }));
  assert.deepEqual(calls.remove, [["s-7", "uninstall_agents"]]);
  escape();
  await noDialog();
});
