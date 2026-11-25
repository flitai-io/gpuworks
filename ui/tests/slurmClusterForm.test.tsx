import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { describedBy, field, footerButtons as buttonsIn, freshPopupFramesPerTest } from "./helpers/platformDialog.ts";
import { SETUP_COMMAND, SLURM_CATEGORY, SLURM_DEFAULTS, slurmEntry } from "./helpers/platformFixtures.ts";

// The form that connects a Slurm cluster (ui/src/pages/slurm/useSlurmClusterForm.tsx,
// its rules in ui/src/pages/slurm/slurmClusterForm.ts), where the console shows
// it for a cluster of its own: in the Add External Platform dialog with Slurm
// chosen, and, editing an entry, in a drawer. What it shows when it opens,
// what it refuses, what it sends and what it presents once the entry exists.
// An E-HPC cluster of an account opens it prefilled: tests/slurmHpcGroup.test.tsx.

const { i18n, locale } = await createI18n(["slurm", "providers"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { default: SlurmClusterDrawer } = await import("../src/pages/slurm/SlurmClusterDrawer.tsx");
import type { SlurmClusterRequest, SlurmClusterUpdate, SlurmDefaults } from "../src/slurmApi";

const f = locale.slurm.form;
const originals = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { cleanup(); Object.assign(api, originals); });

type Calls = Record<string, unknown[][]>;
function stubApi(options: { defaults?: SlurmDefaults | Error; create?: unknown; update?: unknown } = {}) {
  const calls: Calls = {};
  const record = (name: string, result: unknown) => async (...args: unknown[]) => {
    (calls[name] ??= []).push(args);
    if (result instanceof Error) throw result;
    return typeof result === "function" ? (result as (...a: unknown[]) => unknown)(...args) : result;
  };
  api.slurmDefaults = record("slurmDefaults", options.defaults ?? SLURM_DEFAULTS) as never;
  api.createSlurmCluster = record("createSlurmCluster", options.create ?? ((body: SlurmClusterRequest) => slurmEntry({ id: 9, uuid: "s-9", name: body.name, slurm_cluster_name: "", node_count: 0, slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [] }))) as never;
  api.updateSlurmCluster = record("updateSlurmCluster", options.update ?? ((uuid: string, body: SlurmClusterUpdate) => ({ cluster: slurmEntry({ ...body } as never), changed: Object.keys(body).sort() }))) as never;
  return calls;
}

/** The dialog as the External Platforms page holds it for an administrator who picked the Slurm card. */
function Harness({ seen }: { seen: { closed: number; created: number } }) {
  const [open, setOpen] = React.useState(true);
  return (
    <I18nextProvider i18n={i18n}>
      <ProviderConfigModal open={open} defaultKind="slurm" kindInfos={[]} categories={[SLURM_CATEGORY]} providerLabel={i18n.t("platforms.label", { ns: "providers" })} canConnectCluster
        onClose={() => { seen.closed += 1; setOpen(false); }} onSaved={() => {}} onClusterCreated={() => { seen.created += 1; }} />
    </I18nextProvider>
  );
}
function openForm() {
  const seen = { closed: 0, created: 0 };
  render(<Harness seen={seen} />);
  return { seen, user: userEvent.setup({ document: dom.window.document }) };
}
const dialog = () => within(screen.getByRole("dialog"));
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });
const noDialog = () => waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the dialog closed"));
const input = (label: string) => dialog().getByLabelText(field(label)) as HTMLInputElement;

test("the dialog offers Slurm as a platform type with nothing more to choose, says what it connects, and opens on the form with the management node address prefilled", async () => {
  stubApi();
  openForm();
  const form = dialog();
  assert.equal(form.getByRole("tab", { name: locale.providers.config.slurmGroup }).getAttribute("aria-selected"), "true");
  assert.equal(form.queryAllByRole("combobox").length, 0, "a cluster is one platform");
  assert.ok(form.getByText(locale.providers.config.howSlurmCluster));
  const address = await form.findByLabelText(field(f.serverUrl)) as HTMLInputElement;
  assert.equal(address.value, "http://localhost");
  assert.ok(describedBy(address).includes(f.serverUrlHelp));
  assert.deepEqual(buttonsIn(screen.getByRole("dialog")), [f.cancel, f.submit]);
});

test("the advanced section shows the server's defaults as hints with every field left empty, and the ranges the server enforces", async () => {
  stubApi();
  const { user } = openForm();
  await dialog().findByLabelText(field(f.serverUrl));
  await user.click(dialog().getByText(f.advanced));
  await waitFor(() => assert.equal(input(f.runAs).placeholder, "root"));
  for (const [label, hint] of [[f.runAs, "root"], [f.installDir, "/var/lib/gpuworks"], [f.scriptDir, "/opt/gpuworks-slurm"], [f.idleReturn, "300"], [f.pendingTimeout, "600"]]) {
    const control = label === f.idleReturn || label === f.pendingTimeout ? (dialog().getAllByRole("spinbutton") as HTMLInputElement[])[label === f.idleReturn ? 0 : 1] : input(label);
    assert.deepEqual([control.value, control.placeholder], ["", hint], label);
  }
  const [idle, pending] = dialog().getAllByRole("spinbutton") as HTMLInputElement[];
  assert.deepEqual([idle.min, idle.max, pending.min, pending.max], ["1", "86400", "30", "86400"]);
  assert.ok(dialog().getByText(f.idleReturnHelpRange.replace("{{min}}", "1").replace("{{max}}", "86400")));
  assert.ok(dialog().getByText(f.runAsHelp), "non-root accounts have known limits");
  assert.ok(dialog().getByText(f.scriptDirHelp), "same path on every node, ideally shared");
});

test("a number outside the server's range is brought to its bound when the field is left", async () => {
  const calls = stubApi();
  const { user } = openForm();
  await dialog().findByLabelText(field(f.serverUrl));
  await user.click(dialog().getByText(f.advanced));
  const [idle, pending] = dialog().getAllByRole("spinbutton") as HTMLInputElement[];
  await user.type(idle, "999999");
  await user.tab();
  assert.equal(idle.value, "86400");
  await user.type(pending, "5");
  await user.tab();
  assert.equal(pending.value, "30");
  await user.type(input(f.name), "lab");
  await user.click(dialog().getByRole("button", { name: f.submit }));
  await waitFor(() => assert.equal(calls.createSlurmCluster?.length, 1));
  assert.deepEqual(calls.createSlurmCluster[0][0], { name: "lab", server_url: "http://localhost", idle_return_seconds: 86400, pending_timeout_seconds: 30 });
});

test("a form without a name, without a management node address or with one that is no URL is refused before any request", async () => {
  const calls = stubApi();
  const { seen, user } = openForm();
  await user.click(await dialog().findByRole("button", { name: f.submit }));
  assert.ok(await dialog().findByText(f.nameRequired));
  await user.type(input(f.name), "lab");
  await user.clear(input(f.serverUrl));
  await user.click(dialog().getByRole("button", { name: f.submit }));
  assert.ok(await dialog().findByText(f.serverUrlRequired));
  await user.type(input(f.serverUrl), "gpuworks.example");
  await user.click(dialog().getByRole("button", { name: f.submit }));
  assert.ok(await dialog().findByText(f.serverUrlInvalid));
  assert.equal(calls.createSlurmCluster, undefined);
  assert.equal(seen.created, 0);
});

test("Enter in a field submits; only what was typed is sent, trimmed, and the entry's setup command is then shown with what it does", async () => {
  const calls = stubApi({ create: (body: SlurmClusterRequest) => slurmEntry({ id: 9, uuid: "s-9", name: body.name, slurm_cluster_name: "", node_count: 0, slurm_gpu_nodes: 0, missing_count: 0, missing_nodes: [] }) });
  const { seen, user } = openForm();
  await user.type(input(f.name), "  lab  ");
  await user.click(dialog().getByText(f.advanced));
  await user.type(input(f.accountUser), "gwks");
  await user.type(input(f.sbatchArgs), "-p gpu");
  await user.type(input(f.scriptDir), "/shared/gw{Enter}");
  await waitFor(() => assert.equal(calls.createSlurmCluster?.length, 1));
  assert.deepEqual(calls.createSlurmCluster[0][0], { name: "lab", server_url: "http://localhost", account_user: "gwks", script_dir: "/shared/gw", sbatch_args: "-p gpu" });
  assert.equal(seen.created, 1);
  const done = within(screen.getByRole("dialog"));
  assert.ok(await done.findByText(locale.slurm.created.title.replace("{{name}}", "lab")));
  assert.equal(done.queryAllByLabelText(field(f.name)).length, 0, "the form is gone");
  assert.equal(done.getByLabelText(locale.slurm.setup.command, { selector: "pre" }).textContent, SETUP_COMMAND);
  for (const key of ["script", "distribute", "conf", "reconfigure"] as const) assert.ok(done.getByText(locale.slurm.setup.step[key]));
  assert.ok(await done.findByText("Every GPU node then joins by itself within one to one and a half health-check intervals (an interval is 5 min)."));
  assert.deepEqual(buttonsIn(screen.getByRole("dialog")), [f.close]);
  await waitFor(() => assert.equal(document.activeElement?.textContent, f.close));
  await user.click(done.getByRole("button", { name: locale.slurm.command.copy.replace("{{title}}", locale.slurm.setup.command) }));
  assert.equal(await navigator.clipboard.readText(), SETUP_COMMAND);
});

test("the server's refusals land where they belong: a taken name on the name field, an invalid field on its own field with the advanced section opened, anything else above the form", async () => {
  const taken = new APIError("SLURM_CLUSTER_NAME_TAKEN", { name: "lab" }, "taken", 409);
  const invalid = new APIError("SLURM_CLUSTER_INVALID", { field: "account_user", reason: "must be a login name" }, "bad", 400);
  const down = new APIError("INTERNAL_ERROR", {}, "database is locked", 500);
  const answers = [taken, invalid, down];
  const calls = stubApi({ create: () => { throw answers[calls.createSlurmCluster.length - 1]; } });
  const { user } = openForm();
  await user.type(input(f.name), "lab");
  await user.click(dialog().getByRole("button", { name: f.submit }));
  assert.ok(await dialog().findByText("Another Slurm cluster is already named lab."));
  assert.equal(describedBy(input(f.name)).includes("lab"), true);
  await user.click(dialog().getByRole("button", { name: f.submit }));
  assert.ok(await dialog().findByText("The Slurm cluster's account_user cannot be used: must be a login name."));
  assert.ok(input(f.accountUser), "the advanced section opened on the refused field");
  await user.click(dialog().getByRole("button", { name: f.submit }));
  assert.ok(await dialog().findByText(/database is locked/));
});

test("defaults that could not be read are said, and the form still sends only what was typed", async () => {
  const calls = stubApi({ defaults: new Error("down") });
  const { user } = openForm();
  assert.ok(await dialog().findByText(f.defaultsFailed));
  await user.type(input(f.name), "lab");
  await user.click(dialog().getByRole("button", { name: f.submit }));
  await waitFor(() => assert.equal(calls.createSlurmCluster?.length, 1));
});

test("Escape closes the dialog before anything was made, and Cancel does the same", async () => {
  stubApi();
  const { seen, user } = openForm();
  await dialog().findByLabelText(field(f.name));
  escape();
  await noDialog();
  assert.equal(seen.closed, 1);
  cleanup();
  const again = openForm();
  await again.user.click(await dialog().findByRole("button", { name: f.cancel }));
  await noDialog();
  assert.equal(again.seen.closed, 1);
});

// Editing an entry, in a drawer.
function renderEdit(entry = slurmEntry()) {
  const seen = { edited: 0, closed: 0 };
  render(<I18nextProvider i18n={i18n}><SlurmClusterDrawer open edit={entry} onClose={() => { seen.closed += 1; }} onCreated={() => {}} onEdited={() => { seen.edited += 1; }} /></I18nextProvider>);
  return { seen, user: userEvent.setup({ document: dom.window.document }) };
}

test("editing shows the entry; the server address, the agents' account and the two directories are shown and fixed, the rest can change", async () => {
  stubApi();
  const { user } = renderEdit(slurmEntry({ account_user: "gwks" }));
  const form = within(screen.getByRole("dialog"));
  assert.equal(form.getByRole("heading", { name: f.editTitle }) !== null, true);
  assert.equal((form.getByLabelText(field(f.name)) as HTMLInputElement).value, "hpc-lab");
  assert.equal(input(f.serverUrl).disabled, true);
  assert.equal(input(f.serverUrl).value, "https://gpuworks.example");
  await user.click(form.getByText(f.advanced));
  assert.deepEqual([f.runAs, f.installDir, f.scriptDir].map((label) => input(label).disabled), [true, true, true]);
  assert.deepEqual([f.runAs, f.installDir, f.scriptDir].map((label) => input(label).value), ["root", "/var/lib/gpuworks", "/opt/gpuworks-slurm"]);
  assert.deepEqual([f.accountUser, f.sbatchArgs].map((label) => input(label).disabled), [false, false]);
  assert.equal(input(f.accountUser).value, "gwks");
  assert.equal(form.getAllByText(f.fixedAfterCreate).length, 4);
});

test("saving an edit sends only the fields that changed and says what changed and that the agents pick it up within the report interval", async () => {
  const calls = stubApi();
  const { seen, user } = renderEdit();
  const form = within(screen.getByRole("dialog"));
  await user.clear(form.getByLabelText(field(f.name)));
  await user.type(form.getByLabelText(field(f.name)), "hpc-lab-2");
  await user.click(form.getByText(f.advanced));
  await user.clear(input(f.sbatchArgs));
  await user.click(form.getByRole("button", { name: f.save }));
  await waitFor(() => assert.equal(calls.updateSlurmCluster?.length, 1));
  assert.deepEqual(calls.updateSlurmCluster[0], ["s-1", { name: "hpc-lab-2", sbatch_args: "" }], "an emptied text clears it; nothing else is sent");
  assert.equal(seen.edited, 1);
  assert.ok(await screen.findByText(locale.slurm.edit.savedTitle.replace("{{name}}", "hpc-lab-2")));
  assert.ok(await screen.findByText(/within 10 min\./));
  assert.deepEqual(within(screen.getByRole("dialog")).getAllByRole("listitem").map((item) => item.textContent), [locale.slurm.edit.field.name, locale.slurm.edit.field.sbatch_args]);
});

test("an edit that changes nothing closes without a request", async () => {
  const calls = stubApi();
  const { seen, user } = renderEdit();
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: f.save }));
  await waitFor(() => assert.equal(seen.closed, 1));
  assert.equal(calls.updateSlurmCluster, undefined);
});
