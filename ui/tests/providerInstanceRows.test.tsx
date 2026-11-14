import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, aliyunKind, capabilities, instance, runpodKind, zstackKind } from "./helpers/platformFixtures.ts";
import { cellUnder, columnTitles, instanceRow } from "./helpers/platformDetail.ts";
import { list } from "./helpers/listPage.ts";

// One instance of an account, as a row of what the account's row opens to
// (ui/src/pages/platforms/InstanceRow.tsx in ui/src/pages/platforms/ProviderResources.tsx):
// what each cell says, which columns the account's table has, how the
// instance is reached and the SSH command to copy, and what can be done with
// it, which the server decides (`actions`): power it off after asking, power
// it on, install the agent, or nothing once it is a compute node. And the
// container runtime note above the table, said where it applies.

const languages = { en: await createI18n(["providers"], "en"), zh: await createI18n(["providers"], "zh") };
const p = languages.en.locale.providers;
const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: ProviderResources } = await import("../src/pages/platforms/ProviderResources.tsx");
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { api } = await import("../src/api.ts");
type CloudProvider = import("../src/api.ts").CloudProvider;
type KindResource = import("../src/api.ts").KindResource;
type ProviderInstance = import("../src/api.ts").ProviderInstance;
type ProviderKindInfo = import("../src/api.ts").ProviderKindInfo;
type SSHConnection = import("../src/api.ts").SSHConnection;

const original = { ...api };
afterEach(() => { message.destroy(); cleanup(); Object.assign(api, original); });

const pods = account(1, "Pods", "runpod");
const hosts = account(2, "Hosts", "zstack");
const ali = account(3, "ali-prod", "aliyun");
/** A kind as GET /api/v1/providers/kinds answers for it: its platform type, and what it connects. */
const kind = (name: string, resources: KindResource[], overrides: Partial<ProviderKindInfo> = {}): ProviderKindInfo => ({
  kind: name, label: { en: name, zh: name }, category: name === "zstack" ? "private_cloud" : name === "newcloud" ? "" : "public_cloud",
  capabilities, credential_fields: [], server_dial_supported: false,
  can_list_clusters: resources.some((resource) => resource.type === "kubernetes"), resources, ...overrides,
});
const KIND_OF: Record<string, ProviderKindInfo> = { runpod: runpodKind, zstack: zstackKind, aliyun: aliyunKind };

/** Serves the account's instance list (and no managed cluster) and counts its reads. */
function serve(instances: ProviderInstance[]) {
  const server = { instances, reads: 0 };
  Object.assign(api, {
    providerInstances: async () => { server.reads += 1; return list(server.instances); },
    providerClusters: async () => list([]),
  });
  return server;
}
// The outcome of an action is said in a toast, which the console's frame hosts.
function mount(options: { provider?: CloudProvider; kindInfo?: ProviderKindInfo | null; language?: keyof typeof languages } = {}) {
  const provider = options.provider ?? pods;
  const kindInfo = options.kindInfo === null ? undefined : options.kindInfo ?? KIND_OF[provider.kind];
  render(
    <I18nextProvider i18n={languages[options.language ?? "en"].i18n}>
      <FeedbackHost />
      <MemoryRouter><ProviderResources provider={provider} kindInfo={kindInfo} refreshKey="1" entries={{}} onEntriesChanged={() => {}} admin /></MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}
/** What a cell says, line by line: its children, or its own text when it has none. */
const lines = (cell: HTMLElement) => cell.children.length ? [...cell.children].map((line) => line.textContent ?? "") : [cell.textContent ?? ""];
/** The toasts on screen, as they read. */
const toasts = () => [...screen.getByRole("list", { name: "Notices" }).children].map((toast) => toast.textContent);
const connection = (overrides: Partial<SSHConnection> = {}): SSHConnection => ({ id: "direct", type: "direct", label: { en: "Direct", zh: "直连" }, host: "203.0.113.5", port: 22, user: "root", install_supported: true, ...overrides });

const rowCopy = {
  en: { instance: "Instance", gpu: "GPU", status: "Status", region: "Region", connection: "Connection", actions: "Actions", running: "Running", stopped: "Stopped", unknown: "Unknown", missing: "Not returned" },
  zh: { instance: "实例", gpu: "GPU", status: "状态", region: "区域", connection: "连接方式", actions: "操作", running: "运行中", stopped: "已停止", unknown: "未知", missing: "未返回" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} an instance's row leads with its name over the vendor's id, then its GPUs as model ×count, its power state and its region; what the vendor did not return says so`, async () => {
    const copy = rowCopy[language];
    serve([
      instance({ gpu_count: 4 }),
      // The vendor gave no name: the id stands in for it, once.
      instance({ external_id: "i-noname", name: "", power: "stopped", gpu_model: "", gpu_count: 2, region: "" }),
      // A power state the console has no word for is shown as the vendor said it; none at all is unknown.
      instance({ external_id: "i-starting", name: "boot-01", power: "starting", gpu_count: 0 }),
      instance({ external_id: "i-silent", name: "mute-01", power: "", gpu_model: "", gpu_count: 0 }),
      instance({ external_id: "i-unknown", name: "lost-01", power: "unknown" }),
    ]);
    mount({ language });
    const row = await instanceRow("train-01");
    assert.deepEqual(await columnTitles(), [copy.instance, copy.gpu, copy.status, copy.region, copy.connection, copy.actions]);

    // The name, and under it the id; the region follows the id there for a detail too narrow for its column.
    const name = await cellUnder(row, copy.instance);
    assert.deepEqual(lines(name), ["train-01", "i-uf6train01 · cn-shanghai"]);
    assert.deepEqual([...name.children].map((line) => line.getAttribute("title")), ["train-01", "i-uf6train01"], "a name or an id cut short is read whole when pointed at");
    assert.equal((await cellUnder(row, copy.gpu)).textContent, "NVIDIA A10 ×4");
    assert.equal((await cellUnder(row, copy.status)).textContent, copy.running);
    assert.equal((await cellUnder(row, copy.region)).textContent, "cn-shanghai");

    const unnamed = await instanceRow("i-noname");
    assert.deepEqual(lines(await cellUnder(unnamed, copy.instance)), ["i-noname"], "the id is not repeated under itself, and no region is made up");
    assert.equal((await cellUnder(unnamed, copy.gpu)).textContent, `${copy.missing} ×2`);
    assert.equal((await cellUnder(unnamed, copy.status)).textContent, copy.stopped);
    assert.equal((await cellUnder(unnamed, copy.region)).textContent, "—");

    assert.equal((await cellUnder(await instanceRow("boot-01"), copy.gpu)).textContent, "NVIDIA A10", "no count, no ×0");
    assert.equal((await cellUnder(await instanceRow("boot-01"), copy.status)).textContent, "starting");
    assert.equal((await cellUnder(await instanceRow("mute-01"), copy.gpu)).textContent, copy.missing);
    assert.equal((await cellUnder(await instanceRow("mute-01"), copy.status)).textContent, copy.unknown);
    assert.equal((await cellUnder(await instanceRow("lost-01"), copy.status)).textContent, copy.unknown);
  });
}

test("an instance without a name still says where it runs under its id; in a private cloud no instance says a region anywhere", async () => {
  serve([instance({ external_id: "i-noname", name: "" }), instance()]);
  mount();
  assert.deepEqual(lines(await cellUnder(await instanceRow("i-noname"), "Instance")), ["i-noname", "cn-shanghai"]);
  cleanup();

  serve([instance({ external_id: "host-7", name: "" }), instance({ kind: "" })]);
  mount({ provider: hosts });
  assert.deepEqual(lines(await cellUnder(await instanceRow("host-7"), "Instance")), ["host-7"]);
  assert.deepEqual(lines(await cellUnder(await instanceRow("train-01"), "Instance")), ["train-01", "i-uf6train01"]);
  assert.equal((await columnTitles()).includes("Region"), false);
  assert.equal(document.body.textContent?.includes("cn-shanghai"), false);
});

// Which optional columns the table has: the price for a kind that has pricing, the region outside a private cloud, and
// the type only where the account's instances are not all of one kind of resource.
const BASE = ["Instance", "GPU", "Status"];
const TAIL = ["Connection", "Actions"];
const columnCases: Array<[string, CloudProvider, ProviderKindInfo | null, string[]]> = [
  ["a public cloud whose instances are of one kind", pods, runpodKind, [...BASE, "Region", ...TAIL]],
  ["a public cloud with VMs and managed Kubernetes: the clusters are no instance kind", ali, aliyunKind, [...BASE, "Region", ...TAIL]],
  ["a private cloud", hosts, zstackKind, [...BASE, ...TAIL]],
  ["a kind with pricing", pods, { ...runpodKind, capabilities: { ...capabilities, has_pricing: true } }, [...BASE, "Price", "Region", ...TAIL]],
  ["an account with two kinds of instance", account(4, "Mixed", "compshare", { resources: [{ type: "vm" }, { type: "container" }] }), kind("compshare", [{ type: "vm" }, { type: "container" }]), [...BASE, "Region", "Type", ...TAIL]],
  ["an account whose kind declares no resource", account(5, "Lab", "newcloud"), kind("newcloud", []), [...BASE, "Region", "Type", ...TAIL]],
  ["a private cloud with pricing and two kinds of instance", account(6, "Stack", "zstack", { resources: [{ type: "host" }, { type: "vm" }] }), { ...zstackKind, capabilities: { ...capabilities, has_pricing: true } }, [...BASE, "Price", "Type", ...TAIL]],
  // The schema could not be read: nothing says the kind has pricing; the account's own row still says what it connects.
  ["an account whose kind schema is missing", pods, null, [...BASE, "Region", ...TAIL]],
];
for (const [what, provider, kindInfo, titles] of columnCases) {
  test(`the table of ${what} has the columns ${titles.join(", ")}`, async () => {
    serve([instance()]);
    mount({ provider, kindInfo });
    await instanceRow("train-01");
    assert.deepEqual(await columnTitles(), titles);
    assert.equal(within(await instanceRow("train-01")).getAllByRole("cell").length, titles.length, "a cell under every title");
  });
}

test("the price reads per hour with two decimals, and the type column says what each instance is: a VM, a container, the vendor's own word, or nothing known", async () => {
  serve([
    instance({ price_per_hr: 1.2 }),
    instance({ external_id: "pod-1", name: "pod-01", kind: "container", price_per_hr: 0 }),
    instance({ external_id: "bm-1", name: "metal-01", kind: "baremetal", price_per_hr: 12.345 }),
    instance({ external_id: "x-1", name: "plain-01", kind: "" }),
  ]);
  mount({ provider: account(4, "Mixed", "compshare", { resources: [{ type: "vm" }, { type: "container" }] }), kindInfo: kind("compshare", [{ type: "vm" }, { type: "container" }], { capabilities: { ...capabilities, has_pricing: true } }) });
  const cells = async (name: string) => [(await cellUnder(await instanceRow(name), "Price")).textContent, (await cellUnder(await instanceRow(name), "Type")).textContent];
  assert.deepEqual(await cells("train-01"), ["¥1.20/hr", "Virtual machine"]);
  assert.deepEqual(await cells("pod-01"), ["¥0.00/hr", "Container"]);
  assert.deepEqual(await cells("metal-01"), ["¥12.35/hr", "baremetal"]);
  assert.deepEqual(await cells("plain-01"), ["¥0.00/hr", "—"]);
});

test("each way to reach an instance is one line: what it is, user@host:port, and a button that copies the ssh command; one that cannot install says it is for the terminal only", async () => {
  serve([instance({
    ssh_connections: [
      connection(),
      // No label from the vendor: the console's word for the type. No user: root.
      connection({ id: "proxy", type: "proxy", label: { en: "", zh: "" }, host: "ssh.vendor.example", port: 40122, user: "", install_supported: false }),
      connection({ id: "direct-2", label: "", host: "198.51.100.9", port: 2222, user: "ubuntu" }),
    ],
  })]);
  const user = mount();
  const cell = await cellUnder(await instanceRow("train-01"), "Connection");
  const ways = [...(cell.firstElementChild as HTMLElement).children] as HTMLElement[];
  assert.deepEqual(ways.map((way) => way.textContent), ["Directroot@203.0.113.5:22", "External platform proxyroot@ssh.vendor.example:40122Terminal only", "Directubuntu@198.51.100.9:2222"]);
  // The address may be cut short: the whole command is its hint.
  assert.equal(within(ways[0]).getByText("root@203.0.113.5:22").getAttribute("title"), "ssh root@203.0.113.5 -p 22");

  const copy = within(cell).getAllByRole("button", { name: "Copy SSH command" });
  assert.equal(copy.length, 3, "one per connection");
  await user.click(copy[0]);
  assert.equal(await navigator.clipboard.readText(), "ssh root@203.0.113.5 -p 22");
  await user.click(copy[1]);
  assert.equal(await navigator.clipboard.readText(), "ssh root@ssh.vendor.example -p 40122");
  await user.click(copy[2]);
  assert.equal(await navigator.clipboard.readText(), "ssh ubuntu@198.51.100.9 -p 2222");
});

test("the copy button works from the keyboard and names itself when pointed at", async () => {
  serve([instance()]);
  const user = mount();
  const copy = within(await cellUnder(await instanceRow("train-01"), "Connection")).getByRole("button", { name: "Copy SSH command" });
  await user.hover(copy);
  assert.equal((await screen.findByRole("tooltip")).textContent, "Copy SSH command");
  await user.unhover(copy);
  copy.focus();
  await user.keyboard("{Enter}");
  await waitFor(async () => assert.equal(await navigator.clipboard.readText(), "ssh root@203.0.113.5 -p 22"));
});

test("a command channel is a way in without SSH: it is named and says so, with no address and nothing to copy; beside it an SSH connection keeps its command", async () => {
  serve([
    instance({ ssh_connections: [connection({ id: "cloud_assistant", type: "cloud_assistant", label: { en: "Cloud Assistant", zh: "云助手" }, host: "", port: 0, user: "" }), connection()] }),
    instance({ external_id: "i-2", name: "assist-only", ssh_connections: [connection({ id: "cloud_assistant", type: "cloud_assistant", label: { en: "", zh: "" }, host: "", port: 0, user: "" })] }),
  ]);
  mount({ provider: ali });
  const both = await cellUnder(await instanceRow("train-01"), "Connection");
  assert.deepEqual([...(both.firstElementChild as HTMLElement).children].map((way) => way.textContent), ["Cloud Assistantno SSH needed", "Directroot@203.0.113.5:22"]);
  assert.equal(within(both).getAllByRole("button", { name: "Copy SSH command" }).length, 1);

  const only = await cellUnder(await instanceRow("assist-only"), "Connection");
  // A channel the vendor gave no name goes by its type.
  assert.equal(only.textContent, "cloud_assistantno SSH needed");
  assert.equal(within(only).queryAllByRole("button").length, 0);
  assert.equal(only.textContent?.includes("@"), false, "a channel has no endpoint to print");
});

test("an instance listed with an address but no connections is reached by plain SSH at that address; one with neither says nothing was returned", async () => {
  serve([
    // As a server from before connections were listed answers.
    instance({ ssh_connections: [], public_ip: "192.0.2.44", ssh_port: 2022, ssh_user: "" }),
    instance({ external_id: "i-2", name: "named-user", ssh_connections: [], public_ip: "192.0.2.45", ssh_port: 22, ssh_user: "ops" }),
    instance({ external_id: "i-3", name: "unreachable", ssh_connections: [], public_ip: "", ssh_port: 0, ssh_user: "" }),
    instance({ external_id: "i-4", name: "no-port", ssh_connections: [], public_ip: "192.0.2.46", ssh_port: 0 }),
  ]);
  const user = mount();
  const first = await cellUnder(await instanceRow("train-01"), "Connection");
  assert.equal(first.textContent, "SSHroot@192.0.2.44:2022");
  await user.click(within(first).getByRole("button", { name: "Copy SSH command" }));
  assert.equal(await navigator.clipboard.readText(), "ssh root@192.0.2.44 -p 2022");
  assert.equal((await cellUnder(await instanceRow("named-user"), "Connection")).textContent, "SSHops@192.0.2.45:22");
  for (const name of ["unreachable", "no-port"]) {
    const cell = await cellUnder(await instanceRow(name), "Connection");
    assert.equal(cell.textContent, "Not returned");
    assert.equal(within(cell).queryAllByRole("button").length, 0);
  }
});

test("what can be done with an instance is what the server allows on it: a running one is powered off, a stopped one on, one without the agent has it installed, and a compute node says it is onboarded", async () => {
  serve([
    instance(),
    instance({ external_id: "i-2", name: "idle-01", power: "stopped", actions: ["power_on", "install", "delete"] }),
    instance({ external_id: "i-3", name: "node-01", node_id: 41, actions: ["power_off"] }),
    // Running, and its kind controls power: yet the server offers nothing, so nothing is offered.
    instance({ external_id: "i-4", name: "bare-01", node_id: 42, actions: [] }),
    // An older server that sends no actions at all.
    { ...instance({ external_id: "i-5", name: "legacy-01" }), actions: undefined as never },
  ]);
  mount({ kindInfo: { ...runpodKind, capabilities: { ...capabilities, can_delete: true } } });
  const offered = async (name: string) => {
    const cell = await cellUnder(await instanceRow(name), "Actions");
    return { buttons: within(cell).queryAllByRole("button").map((button) => button.textContent), onboarded: within(cell).queryAllByText("Onboarded").length };
  };
  assert.deepEqual(await offered("train-01"), { buttons: ["Power off", "Install Agent"], onboarded: 0 });
  assert.deepEqual(await offered("idle-01"), { buttons: ["Power on", "Install Agent"], onboarded: 0 }, "no release button: deleting an instance is not offered here");
  assert.deepEqual(await offered("node-01"), { buttons: ["Power off"], onboarded: 1 });
  assert.deepEqual(await offered("bare-01"), { buttons: [], onboarded: 1 });
  assert.deepEqual(await offered("legacy-01"), { buttons: [], onboarded: 1 });
});

test("powering off asks first: Cancel and Escape send nothing; confirming sends it for that instance, says so, and reads the instances again", async () => {
  const server = serve([instance(), instance({ external_id: "i-2", name: "train-02" })]);
  const sent: unknown[] = [];
  Object.assign(api, { powerProviderInstance: async (...args: unknown[]) => { sent.push(args); server.instances = [instance({ power: "stopped", actions: ["power_on", "install"] }), server.instances[1]]; return { status: "ok" }; } });
  const user = mount();
  const row = await instanceRow("train-01");
  const powerOff = () => within(row).getByRole("button", { name: "Power off" });

  await user.click(powerOff());
  let asking = await screen.findByRole("dialog", { name: "Power off this instance?" });
  assert.deepEqual(within(asking).getAllByRole("button").map((button) => button.textContent), ["Cancel", "Power off"]);
  await user.click(within(asking).getByRole("button", { name: "Cancel" }));
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));
  await user.click(powerOff());
  await screen.findByRole("dialog", { name: "Power off this instance?" });
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));
  assert.deepEqual(sent, [], "neither Cancel nor Escape powers anything off");
  assert.equal(server.reads, 1);

  await user.click(powerOff());
  asking = await screen.findByRole("dialog", { name: "Power off this instance?" });
  await user.click(within(asking).getByRole("button", { name: "Power off" }));
  await waitFor(() => assert.deepEqual(sent, [[1, "i-uf6train01", "off"]], "the account, the instance of the row that was asked about, and off"));
  await waitFor(() => assert.deepEqual(toasts(), ["Power-off request sent"]));
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the question closes once answered"));
  // The list is read again: the row follows what the server now says of the instance.
  await waitFor(() => assert.equal(server.reads, 2));
  await waitFor(async () => assert.equal((await cellUnder(await instanceRow("train-01"), "Status")).textContent, "Stopped"));
  assert.ok(within(await instanceRow("train-01")).getByRole("button", { name: "Power on" }));
});

test("powering on needs no question: one click sends it; while it is on its way the button is out of use and the other rows are not", async () => {
  const server = serve([instance({ power: "stopped", actions: ["power_on", "install"] }), instance({ external_id: "i-2", name: "idle-02", power: "stopped", actions: ["power_on"] })]);
  const sent: unknown[] = [];
  let answer!: () => void;
  Object.assign(api, { powerProviderInstance: (...args: unknown[]) => { sent.push(args); return new Promise<void>((resolve) => { answer = resolve; }); } });
  const user = mount();
  const powerOn = (name: string) => within(screen.getByText(name).closest("tr") as HTMLElement).getByRole("button", { name: "Power on" }) as HTMLButtonElement;
  await instanceRow("train-01");

  await user.click(powerOn("train-01"));
  assert.equal(screen.queryAllByRole("dialog").length, 0, "nothing is asked");
  assert.deepEqual(sent, [[1, "i-uf6train01", "on"]]);
  await waitFor(() => assert.equal(powerOn("train-01").disabled, true, "the request is on its way"));
  assert.equal(powerOn("idle-02").disabled, false);
  await user.click(powerOn("train-01"));
  assert.equal(sent.length, 1, "a second click sends nothing");
  assert.deepEqual(toasts(), []);

  await act(async () => answer());
  await waitFor(() => assert.deepEqual(toasts(), ["Power-on request sent"]));
  await waitFor(() => assert.equal(server.reads, 2));
  await waitFor(() => assert.equal(powerOn("train-01").disabled, false));
});

for (const [action, button, confirm] of [["on", "Power on", false], ["off", "Power off", true]] as const) {
  test(`a refused power-${action} says so with the server's reason, reads nothing again and leaves the button usable`, async () => {
    const server = serve([instance(action === "on" ? { power: "stopped", actions: ["power_on"] } : {})]);
    let attempts = 0;
    Object.assign(api, { powerProviderInstance: async () => { attempts += 1; throw new Error("instance is locked by the vendor"); } });
    const user = mount();
    const row = await instanceRow("train-01");
    await user.click(within(row).getByRole("button", { name: button }));
    if (confirm) await user.click(within(await screen.findByRole("dialog", { name: "Power off this instance?" })).getByRole("button", { name: "Power off" }));
    assert.equal((await screen.findByRole("alert")).textContent, "Action failed: instance is locked by the vendor");
    await waitFor(() => assert.equal((within(row).getByRole("button", { name: button }) as HTMLButtonElement).disabled, false));
    assert.equal(server.reads, 1, "nothing changed, nothing is read again");
    assert.equal(screen.queryAllByText(/request sent/).length, 0);
    if (confirm) await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));

    // It can be tried again.
    await user.click(within(row).getByRole("button", { name: button }));
    if (confirm) await user.click(within(await screen.findByRole("dialog", { name: "Power off this instance?" })).getByRole("button", { name: "Power off" }));
    await waitFor(() => assert.equal(attempts, 2));
  });
}

test("Install Agent opens the install dialog on that instance; cancelling it changes nothing, and closing it after an install reads the instances again, so the row says onboarded", async () => {
  const server = serve([instance(), instance({ external_id: "i-2", name: "train-02" })]);
  const installs: unknown[] = [];
  Object.assign(api, {
    installAgent: async (id: number, body: { external_id: string }) => {
      installs.push([id, body.external_id]);
      server.instances = [server.instances[0], instance({ external_id: "i-2", name: "train-02", node_id: 77, actions: ["power_off"] })];
      return { status: "installing", connect_mode: "agent_dial", checks: [{ name: "precheck", status: "ok", detail: "all 5 checks passed" }, { name: "install agent", status: "ok", detail: "agent started" }, { name: "agent_dial", status: "ok", detail: "agent session established" }] };
    },
  });
  const user = mount();
  await user.click(within(await instanceRow("train-02")).getByRole("button", { name: "Install Agent" }));
  let dialog = await screen.findByRole("dialog", { name: "Install Agent · train-02" });
  await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
  // antd's dialog leaves the page through its closing animation, which jsdom never finishes; what matters is what it did.
  assert.deepEqual([installs, server.reads], [[], 1], "nothing was installed and nothing is read again");

  await user.click(within(await instanceRow("train-02")).getByRole("button", { name: "Install Agent" }));
  dialog = await screen.findByRole("dialog", { name: "Install Agent · train-02" });
  await user.click(within(dialog).getByRole("button", { name: /^(loading)?Install$/ }));
  await waitFor(() => assert.deepEqual(installs, [[1, "i-2"]], "the account and the instance of that row"));
  await user.click(await within(dialog).findByRole("button", { name: "Done" }));
  await waitFor(() => assert.equal(server.reads, 2));
  await waitFor(async () => assert.ok(within(await cellUnder(await instanceRow("train-02"), "Actions")).getByText("Onboarded")));
  assert.equal(within(await instanceRow("train-02")).queryAllByRole("button", { name: "Install Agent" }).length, 0);
  assert.ok(within(await instanceRow("train-01")).getByRole("button", { name: "Install Agent" }), "the other instance is as it was");
});

test("in zh an instance's connections and actions read in Chinese, and the power-off question asks in Chinese", async () => {
  serve([
    instance({ ssh_connections: [connection({ label: { en: "", zh: "" } }), connection({ id: "proxy", type: "proxy", label: { en: "", zh: "" }, install_supported: false }), connection({ id: "ca", type: "cloud_assistant", label: { en: "Cloud Assistant", zh: "云助手" }, host: "", port: 0, user: "" })] }),
    instance({ external_id: "i-2", name: "node-01", power: "stopped", node_id: 5, actions: ["power_on"], ssh_connections: [], public_ip: "", ssh_port: 0 }),
  ]);
  Object.assign(api, { powerProviderInstance: async () => { throw new Error("限额不足"); } });
  const user = mount({ language: "zh", provider: account(4, "Mixed", "compshare", { resources: [{ type: "vm" }, { type: "container" }] }), kindInfo: kind("compshare", [{ type: "vm" }, { type: "container" }], { capabilities: { ...capabilities, has_pricing: true } }) });
  const row = await instanceRow("train-01");
  assert.deepEqual(await columnTitles(), ["实例", "GPU", "状态", "费用", "区域", "类型", "连接方式", "操作"]);
  const ways = (await cellUnder(row, "连接方式")).firstElementChild as HTMLElement;
  assert.deepEqual([...ways.children].map((way) => way.textContent), ["直连root@203.0.113.5:22", "外部平台代理root@203.0.113.5:22仅终端", "云助手无需 SSH"]);
  assert.equal(within(ways).getAllByRole("button", { name: "复制 SSH 命令" }).length, 2);
  assert.equal((await cellUnder(row, "费用")).textContent, "¥0.00/时");
  assert.equal((await cellUnder(row, "类型")).textContent, "虚拟机");
  assert.deepEqual(within(await cellUnder(row, "操作")).getAllByRole("button").map((button) => button.textContent), ["关机", "一键装 Agent"]);

  const node = await instanceRow("node-01");
  assert.equal((await cellUnder(node, "连接方式")).textContent, "未返回");
  assert.equal((await cellUnder(node, "操作")).textContent, "开机已接入");
  await user.click(within(node).getByRole("button", { name: "开机" }));
  assert.equal((await screen.findByRole("alert")).textContent, "操作失败：限额不足");

  await user.click(within(row).getByRole("button", { name: "关机" }));
  const asking = await screen.findByRole("dialog", { name: "确认关闭该实例？" });
  assert.deepEqual(within(asking).getAllByRole("button").map((button) => button.textContent), ["取消", "关机"]);
});

// The runveil note is about container instances: it is said for a kind that connects them, and where the console
// cannot tell what the kind connects.
const NOTE = p.instances.containerRuntimeNote as string;
const noteCases: Array<[string, CloudProvider, ProviderKindInfo | null, boolean]> = [
  ["a kind that connects container instances", pods, kind("runpod", [{ type: "container", product: "Pod" }]), true],
  ["a kind that connects virtual machines and container instances", account(4, "Mixed", "compshare", { resources: [{ type: "vm" }, { type: "container" }] }), kind("compshare", [{ type: "vm" }, { type: "container" }]), true],
  ["an account whose kind the console could not load", pods, null, true],
  // What the server sends for a kind that declares nothing: an empty list, which says nothing either way.
  ["a kind that declares no resources", account(5, "Lab", "newcloud"), kind("newcloud", []), true],
  ["a kind that connects virtual machines and Kubernetes", ali, aliyunKind, false],
  ["a kind that connects hosts", hosts, kind("zstack", [{ type: "host" }]), false],
];
for (const [name, provider, kindInfo, shown] of noteCases) {
  test(`the container runtime note is ${shown ? "shown" : "not shown"} for ${name}`, async () => {
    serve([instance()]);
    mount({ provider, kindInfo });
    await instanceRow("train-01");
    assert.equal(screen.queryAllByText(NOTE).length, shown ? 1 : 0);
    if (shown) assert.equal(screen.getByText(NOTE).closest('[role="status"]')?.textContent, NOTE, "a note, not an error");
  });
}
