import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, cloudEntry, kubeEntry, servePlatforms } from "./helpers/platformFixtures.ts";

// The External Platforms page as one list of accounts and of the Kubernetes
// clusters connected on their own (ui/src/pages/platforms/platformRows.ts):
// which entries are rows, what each row says it connects and how many compute
// nodes it brought, the filters and addresses that narrow the list, and what
// is left of it when the cluster list cannot be read.

const languages = { en: (await createI18n(["providers", "kubernetes"], "en")).i18n, zh: (await createI18n(["providers", "kubernetes"], "zh")).i18n };
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { api } = await import("../src/api.ts");

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

// An Alibaba Cloud account with 5 ECS instances and one ACK cluster connected:
// 2 nodes joined on its instances and 4 through the cluster, which the server
// counts with the account.
const ali = account(3, "ali-prod", "aliyun", { instance_count: 5, node_count: 6 });
const pods = account(1, "Pods", "runpod", { instance_count: 2 });
const hosts = account(2, "Hosts", "zstack", { instance_count: 3, node_count: 1 });
// Connected from ali-prod's cluster: listed under that account, not as a row.
const ack = cloudEntry({ node_count: 4, cluster_uid: "0b9d6c1e-0000-4000-8000-00000000000a" });
// Connected on their own, with kubectl.
const prod = kubeEntry();
const lab = kubeEntry({ id: 2, uuid: "c-2", name: "lab", cluster_uid: "", node_count: 0 });
const everything = { providers: [ali, pods, hosts], clusters: [prod, ack, lab] };

function renderPage(address = "/providers", language: keyof typeof languages = "en") {
  const view = render(<I18nextProvider i18n={languages[language]}><MemoryRouter initialEntries={[address]}><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}

/** The names of the listed platforms, top to bottom. */
const listed = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[1].textContent);
const rowOf = (name: string) => screen.getAllByRole("row").find((row) => within(row).queryAllByRole("cell")[1]?.textContent === name) as HTMLElement;
/** A row's cell under the column with that title. */
const cell = (name: string, title: string) => within(rowOf(name)).getAllByRole("cell")[screen.getAllByRole("columnheader").findIndex((header) => header.textContent === title)];
/** The resource tags in a cell, as they read. */
const tags = (scope: HTMLElement) => Array.from(scope.querySelectorAll<HTMLElement>('[data-slot="badge"]')).map((tag) => tag.textContent);
/** What each tag says when pointed at: the kind of resource its product name stands for; nothing when the tag is the kind itself. */
const tagTitles = (scope: HTMLElement) => Array.from(scope.querySelectorAll<HTMLElement>('[data-slot="badge"]')).map((tag) => tag.getAttribute("title"));
const filterOptions = async (column: string, name = `Filter by ${column}`) => {
  fireEvent.click(screen.getByRole("button", { name }));
  return within(await screen.findByRole("group", { name })).getAllByRole("checkbox");
};
const names = (options: HTMLElement[]) => options.map((option) => option.getAttribute("aria-label"));
const ticked = (options: HTMLElement[]) => options.filter((option) => option.getAttribute("aria-checked") === "true").map((option) => option.getAttribute("aria-label"));

const summaries = {
  en: { count: "External platforms: 5", summary: "External platforms: 5 · Compute nodes: 10" },
  zh: { count: "5 个外部平台", summary: "5 个外部平台 · 10 个计算节点" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} accounts come first, then the clusters connected on their own; a cluster connected from an account is no row, and the summary counts the rows and their nodes`, async () => {
    servePlatforms(api, everything);
    renderPage("/providers", language);
    await screen.findByText("prod-gpu");

    assert.deepEqual(listed(), ["ali-prod", "Pods", "Hosts", "prod-gpu", "lab"]);
    assert.equal(screen.queryAllByText("ack-gpu").length, 0, "the account's own cluster is listed under the account");
    assert.ok(screen.getByText(summaries[language].count));
    // 6 + 0 + 1 through the accounts, 3 + 0 through the clusters; ack-gpu's 4 are in its account's 6.
    assert.ok(screen.getByText(summaries[language].summary));
  });
}

// A tag is the vendor's product name (ECS, ACK, Pod), which is not translated; a resource the vendor has no product name for is
// the console's word for its kind (Host, 物理机). The kind behind a product name is said when the tag is pointed at, and is what the filter offers.
const resourceColumns = {
  en: {
    resources: "Connects", nodes: "Nodes joined", ali: ["ECS", "ACK"], aliKinds: ["VM", "Kubernetes"], pods: ["Pod"], podsKinds: ["Container"], hosts: ["Host"],
    plain: ["VM", "Container"], filter: "Filter by Connects", kinds: ["VM", "Kubernetes", "Container", "Host"],
    resourcesHelp: /^The kinds of resource this external platform can hand over to GPUWorks\./, nodesHelp: /^Machines that joined through this external platform and are compute nodes now/,
  },
  zh: {
    resources: "可接入资源", nodes: "已接入节点", ali: ["ECS", "ACK"], aliKinds: ["虚拟机", "Kubernetes"], pods: ["Pod"], podsKinds: ["容器实例"], hosts: ["物理机"],
    plain: ["虚拟机", "容器实例"], filter: "按「可接入资源」筛选", kinds: ["虚拟机", "Kubernetes", "容器实例", "物理机"],
    resourcesHelp: /^这个外部平台能交给 GPUWorks 的资源种类。/, nodesHelp: /^经这个外部平台接入、已经成为计算节点的机器数量/,
  },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} each row says what its platform can hand over, without amounts: an account its kind's resources by the vendor's product names, or by kind where it has none, a cluster Kubernetes`, async () => {
    const copy = resourceColumns[language];
    // An account whose kind's resources carry no product name, as the server answers for such a kind (AutoDL, CompShare).
    const plain = account(4, "Plain", "plaincloud", { resources: [{ type: "vm" }, { type: "container" }] });
    servePlatforms(api, { providers: [ali, pods, hosts, plain, account(5, "Lab", "newcloud")], clusters: [prod, ack] });
    renderPage("/providers", language);
    await screen.findByText("prod-gpu");

    assert.deepEqual(tags(cell("ali-prod", copy.resources)), copy.ali);
    assert.deepEqual(tagTitles(cell("ali-prod", copy.resources)), copy.aliKinds);
    assert.equal(cell("ali-prod", copy.resources).textContent, copy.ali.join(""), "how many it has is under the row, not here");
    assert.deepEqual(tags(cell("Pods", copy.resources)), copy.pods);
    assert.deepEqual(tagTitles(cell("Pods", copy.resources)), copy.podsKinds);
    assert.deepEqual(tags(cell("Hosts", copy.resources)), copy.hosts);
    assert.deepEqual(tagTitles(cell("Hosts", copy.resources)), [null], "a kind's own name needs no explaining");
    assert.deepEqual(tags(cell("Plain", copy.resources)), copy.plain);
    assert.deepEqual(tagTitles(cell("Plain", copy.resources)), [null, null]);
    assert.deepEqual(tags(cell("prod-gpu", copy.resources)), ["Kubernetes"]);
    assert.deepEqual(tagTitles(cell("prod-gpu", copy.resources)), [null]);
    assert.equal(cell("Lab", copy.resources).textContent, "—", "a kind that declares no resource shows none");
    assert.equal(document.body.textContent?.includes("云主机"), false, "a VM is 虚拟机 in Chinese, not 云主机");

    // The filter is by kind of resource, whatever a vendor calls its product.
    assert.deepEqual(names(await filterOptions(copy.resources, copy.filter)), copy.kinds);
  });

  test(`in ${language} the two columns about what a platform brings explain themselves when their header is pointed at`, async () => {
    const copy = resourceColumns[language];
    servePlatforms(api, everything);
    const { user } = renderPage("/providers", language);
    await screen.findByText("prod-gpu");

    await user.hover(within(screen.getByRole("columnheader", { name: new RegExp(`^${copy.resources}`) })).getByText(copy.resources));
    assert.ok(await screen.findByText(copy.resourcesHelp));
    await user.unhover(screen.getByText(copy.resources));
    await user.hover(within(screen.getByRole("columnheader", { name: new RegExp(`^${copy.nodes}`) })).getByText(copy.nodes));
    assert.ok(await screen.findByText(copy.nodesHelp));
  });
}

test("a cluster connected on its own is no vendor's: — under Vendor, and the platform type the server files it under, Kubernetes", async () => {
  servePlatforms(api, everything);
  renderPage();
  await screen.findByText("prod-gpu");
  assert.deepEqual(["ali-prod", "Pods", "Hosts", "prod-gpu", "lab"].map((name) => cell(name, "Vendor").textContent), ["Alibaba Cloud", "RunPod", "ZStack Cloud", "—", "—"]);
  assert.equal(cell("prod-gpu", "Platform type").textContent, "Kubernetes");
  assert.equal(cell("ali-prod", "Platform type").textContent, "Public cloud");
});

test("the vendor filter offers the accounts' vendors only, and choosing one leaves that vendor's accounts and no cluster connected on its own", async () => {
  servePlatforms(api, { ...everything, providers: [ali, pods, hosts, account(4, "ali-dev", "aliyun")] });
  renderPage();
  await screen.findByText("prod-gpu");
  const options = await filterOptions("Vendor");
  assert.deepEqual(names(options), ["Alibaba Cloud", "RunPod", "ZStack Cloud"], "each vendor once; Kubernetes is no vendor");
  fireEvent.click(options[0]);
  await waitFor(() => assert.deepEqual(listed(), ["ali-prod", "ali-dev"]));
  fireEvent.click(options[0]);
  await waitFor(() => assert.deepEqual(listed(), ["ali-prod", "Pods", "Hosts", "ali-dev", "prod-gpu", "lab"]));
});

test("the update time is offered as a column but not shown by default", async () => {
  servePlatforms(api, everything);
  renderPage();
  await screen.findByText("prod-gpu");
  assert.deepEqual(screen.getAllByRole("columnheader").slice(1).map((header) => header.textContent), ["Name", "Vendor", "Platform type", "Connects", "Nodes joined", "Status", "Actions"]);

  fireEvent.click(screen.getByRole("button", { name: "Customize columns" }));
  const offered = within(await screen.findByRole("dialog")).getByRole("checkbox", { name: "Updated" });
  assert.equal(offered.getAttribute("aria-checked"), "false", "the column is there to be shown");
});

test("a node count opens the node list on the nodes it counted: an account's by its id, a cluster's by its UUID, as that list's source filter; a zero is not a link", async () => {
  servePlatforms(api, everything);
  renderPage();
  await screen.findByText("prod-gpu");

  // /nodes?source=provider:<id> and /nodes?source=cluster:<uuid>, the value encoded.
  assert.equal(within(cell("ali-prod", "Nodes joined")).getByRole("link", { name: "6" }).getAttribute("href"), "/nodes?source=provider%3A3");
  assert.equal(within(cell("Hosts", "Nodes joined")).getByRole("link", { name: "1" }).getAttribute("href"), "/nodes?source=provider%3A2");
  assert.equal(within(cell("prod-gpu", "Nodes joined")).getByRole("link", { name: "3" }).getAttribute("href"), "/nodes?source=cluster%3Ac-1");
  for (const name of ["Pods", "lab"]) {
    assert.equal(cell(name, "Nodes joined").textContent, "0");
    assert.equal(within(cell(name, "Nodes joined")).queryAllByRole("link").length, 0, `${name} brought no node: nothing to open`);
  }
});

test("the resources filter offers the resources in the list and leaves the platforms that connect the chosen one", async () => {
  servePlatforms(api, everything);
  renderPage();
  await screen.findByText("prod-gpu");

  const options = await filterOptions("Connects");
  assert.deepEqual(names(options), ["VM", "Kubernetes", "Container", "Host"]);
  fireEvent.click(options[1]);
  await waitFor(() => assert.deepEqual(listed(), ["ali-prod", "prod-gpu", "lab"]));
  fireEvent.click(options[1]);
  fireEvent.click(options[3]);
  await waitFor(() => assert.deepEqual(listed(), ["Hosts"]));
});

test("?resource=kubernetes opens the list on what connects Kubernetes: accounts with managed clusters and clusters connected on their own; clearing it brings the rest back", async () => {
  servePlatforms(api, everything);
  renderPage("/providers?resource=kubernetes");
  await screen.findByText("prod-gpu");
  assert.deepEqual(listed(), ["ali-prod", "prod-gpu", "lab"]);

  const options = await filterOptions("Connects");
  assert.deepEqual(ticked(options), ["Kubernetes"]);
  fireEvent.click(options[names(options).indexOf("Kubernetes")]);
  await waitFor(() => assert.deepEqual(listed(), ["ali-prod", "Pods", "Hosts", "prod-gpu", "lab"]));
});

test("a ?resource= nothing in the list connects yet still shows as the filter in use, so it can be cleared", async () => {
  servePlatforms(api, { providers: [pods], clusters: [] });
  renderPage("/providers?resource=kubernetes");
  await screen.findByRole("button", { name: "Add RunPod" });
  await waitFor(() => assert.equal(screen.queryAllByText("Pods").length, 0));

  const options = await filterOptions("Connects");
  assert.deepEqual(names(options), ["Container", "Kubernetes"]);
  assert.deepEqual(ticked(options), ["Kubernetes"]);
  fireEvent.click(options[1]);
  await waitFor(() => assert.deepEqual(listed(), ["Pods"]));
});

for (const address of ["/providers?resource=", "/providers?resource=nonsense", "/providers?kind=kubernetes"]) {
  test(`${address} names no resource, so the whole list opens`, async () => {
    servePlatforms(api, everything);
    renderPage(address);
    await screen.findByText("prod-gpu");
    assert.deepEqual(listed(), ["ali-prod", "Pods", "Hosts", "prod-gpu", "lab"]);
    assert.deepEqual(ticked(await filterOptions("Connects")), []);
  });
}

test("?category=kubernetes opens the list on the clusters connected on their own, and the type filter offers Kubernetes beside the clouds", async () => {
  servePlatforms(api, everything);
  renderPage("/providers?category=kubernetes");
  await screen.findByText("prod-gpu");
  assert.deepEqual(listed(), ["prod-gpu", "lab"]);

  const options = await filterOptions("Platform type");
  assert.deepEqual(names(options), ["Public cloud", "Private cloud", "Kubernetes"]);
  assert.deepEqual(ticked(options), ["Kubernetes"]);
});

test("a platform type and a resource in the address narrow the list together", async () => {
  servePlatforms(api, everything);
  renderPage("/providers?category=public_cloud&resource=kubernetes");
  await screen.findByText("ali-prod");
  assert.deepEqual(listed(), ["ali-prod"]);
});

test("the status filter offers the states in the list in the words of the rows' badges, an account's and a cluster's apart", async () => {
  const broken = account(5, "Broken", "runpod", { state: "invalid" });
  const stalled = kubeEntry({ id: 4, uuid: "c-4", name: "doks", cluster_uid: "", node_count: 0, stalled: true });
  const retired = kubeEntry({ id: 3, uuid: "c-3", name: "retired", revoked: true });
  servePlatforms(api, { providers: [ali, broken], clusters: [prod, lab, stalled, retired] });
  renderPage();
  await screen.findByText("prod-gpu");

  const options = await filterOptions("Status");
  assert.deepEqual(names(options), ["Valid", "Error", "Active", "Not connected yet", "Not connected for long", "Revoked"]);
  // Each choice is the word its rows show; a cluster waiting too long has its badge and a way to troubleshoot.
  assert.deepEqual(["ali-prod", "Broken", "prod-gpu", "lab", "retired"].map((name) => cell(name, "Status").textContent), ["Valid", "Error", "Active", "Not connected yet", "Revoked"]);
  assert.ok(within(cell("doks", "Status")).getByRole("button", { name: "Troubleshoot" }));
  fireEvent.click(options[3]);
  await waitFor(() => assert.deepEqual(listed(), ["lab"], "waiting, not yet for long"));
  fireEvent.click(options[3]);
  fireEvent.click(options[4]);
  await waitFor(() => assert.deepEqual(listed(), ["doks"]));
  fireEvent.click(options[4]);
  fireEvent.click(options[5]);
  await waitFor(() => assert.deepEqual(listed(), ["retired"]));
  fireEvent.click(options[5]);
  fireEvent.click(options[2]);
  await waitFor(() => assert.deepEqual(listed(), ["prod-gpu"], "a cluster a node joined from; a valid account is not one"));
  fireEvent.click(options[2]);
  fireEvent.click(options[0]);
  await waitFor(() => assert.deepEqual(listed(), ["ali-prod"]));
});

// The server says where an entry stands (dto.KubeCluster state); the console does not work it out from the entry's other
// fields. These two entries are made to disagree with their own fields, so only the state can explain what the row shows.
test("a cluster's status and the status filter follow the state the server sends, whatever the entry's other fields suggest", async () => {
  const late = kubeEntry({ id: 6, uuid: "c-6", name: "late", cluster_uid: "7f3c2a10-0000-4000-8000-000000000006", node_count: 2, stalled: false, revoked: false, state: "stalled" });
  const early = kubeEntry({ id: 7, uuid: "c-7", name: "early", cluster_uid: "", node_count: 0, state: "active" });
  servePlatforms(api, { providers: [], clusters: [late, early] });
  renderPage();
  await screen.findByText("late");
  const status = within(cell("late", "Status"));
  // The badge reads as the status filter names it.
  assert.ok(status.getByText("Not connected for long"));
  assert.ok(status.getByRole("button", { name: "Troubleshoot" }), "stalled, with the way to find out why");
  assert.equal(cell("early", "Status").textContent, "Active");
  const options = await filterOptions("Status");
  assert.deepEqual(names(options), ["Not connected for long", "Active"]);
  fireEvent.click(options[0]);
  await waitFor(() => assert.deepEqual(listed(), ["late"]));
});

test("a list without a revoked or waiting cluster does not offer those states", async () => {
  servePlatforms(api, { providers: [ali], clusters: [prod] });
  renderPage();
  await screen.findByText("prod-gpu");
  assert.deepEqual(names(await filterOptions("Status")), ["Valid", "Active"]);
});

test("clusters connected on their own are listed when there is no account, and the list is not called empty", async () => {
  servePlatforms(api, { providers: [], clusters: [prod] });
  renderPage();
  await screen.findByText("prod-gpu");
  assert.deepEqual(listed(), ["prod-gpu"]);
  assert.equal(screen.queryAllByText("No external platform yet. Pick one above to connect it.").length, 0);
  assert.ok(screen.getByText("External platforms: 1 · Compute nodes: 3"));
  assert.equal((screen.getByRole("button", { name: "Sync All" }) as HTMLButtonElement).disabled, true, "a cluster has no instances to synchronize");
});

test("the accounts do not wait for the cluster list: they are listed while it is still being read, and the clusters join them when it answers", async () => {
  let answer!: (clusters: unknown) => void;
  servePlatforms(api, { providers: [ali, pods] });
  Object.assign(api, { kubeClusters: () => new Promise((resolve) => { answer = resolve; }) });
  renderPage();
  await screen.findByText("ali-prod");
  assert.deepEqual(listed(), ["ali-prod", "Pods"]);
  assert.equal(screen.queryAllByRole("status").length, 0, "no spinner over a list that has its accounts");
  assert.equal(screen.queryAllByText("Failed to load Kubernetes clusters").length, 0, "still reading is not failing");

  answer({ items: [prod], total: 1 });
  await waitFor(() => assert.deepEqual(listed(), ["ali-prod", "Pods", "prod-gpu"]));
});

const failures = {
  en: { title: "Failed to load Kubernetes clusters", retry: "Retry", summary: "External platforms: 3 · Compute nodes: 7", after: "External platforms: 5 · Compute nodes: 10" },
  zh: { title: "Kubernetes 集群读取失败", retry: "重试", summary: "3 个外部平台 · 7 个计算节点", after: "5 个外部平台 · 10 个计算节点" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} when the cluster list cannot be read the accounts are still listed, a warning gives the server's reason, and retrying brings the clusters`, async () => {
    const copy = failures[language];
    const server = servePlatforms(api, { ...everything, clusters: new Error("kube store unavailable") });
    renderPage("/providers", language);
    await screen.findByText("ali-prod");

    // A notice, not an alert: the list below is still good.
    const warning = (await screen.findByText(copy.title)).closest('[role="status"]') as HTMLElement;
    assert.ok(warning.textContent?.includes("kube store unavailable"));
    assert.deepEqual(listed(), ["ali-prod", "Pods", "Hosts"], "the accounts do not wait for the clusters");
    assert.ok(screen.getByText(copy.summary));

    // Retry reads the cluster list at once (the page's own 15 s poll is not what answers). Still failing: the warning and the accounts stay.
    let before = server.reads.clusters;
    fireEvent.click(within(warning).getByRole("button", { name: copy.retry }));
    assert.equal(server.reads.clusters, before + 1);
    assert.ok(await screen.findByText(copy.title));
    assert.deepEqual(listed(), ["ali-prod", "Pods", "Hosts"]);

    server.clusters = everything.clusters;
    before = server.reads.clusters;
    fireEvent.click(within(screen.getByText(copy.title).closest('[role="status"]') as HTMLElement).getByRole("button", { name: copy.retry }));
    assert.equal(server.reads.clusters, before + 1);
    await waitFor(() => assert.deepEqual(listed(), ["ali-prod", "Pods", "Hosts", "prod-gpu", "lab"]));
    await waitFor(() => assert.equal(screen.queryAllByText(copy.title).length, 0));
    assert.ok(screen.getByText(copy.after));
  });
}
