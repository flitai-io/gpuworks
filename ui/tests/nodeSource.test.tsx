// Where a compute node came from (ui/src/pages/nodes/NodeSourceCell.tsx) and
// the node list filtered by source (ui/src/pages/NodesPage.tsx): the filter on
// the Source column header, whose choice lives in the address as ?source=, the
// address an external platform's node count links to (ui/src/nodeSources.ts).
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

Object.defineProperty(globalThis, "Document", { value: dom.window.Document, configurable: true });
// The node event stream never delivers here: the list shows what it loaded.
class SilentEventSource { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: SilentEventSource, configurable: true });

const languages = { en: await createI18n(["nodes", "providers"], "en"), zh: await createI18n(["nodes", "providers"], "zh") };
const { RouterProvider, createMemoryRouter } = await import("react-router-dom");
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { message } = await import("antd");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: NodesPage } = await import("../src/pages/NodesPage.tsx");
const { kubeClusterNodesPath, nodeSourcesOf, platformNodesPath, platformPath } = await import("../src/nodeSources.ts");
import type { ListQuery, Node, NodePage, NodeSource, NodeSourceOption } from "../src/api";
import { list } from "./helpers/listPage.ts";

const CLUSTER_OF_ACCOUNT = "7c1b6f0e-0000-4000-8000-000000000007";
const CLUSTER_ON_ITS_OWN = "9d2e4a11-0000-4000-8000-000000000009";

function node(name: string, source?: NodeSource): Node {
  return {
    uuid: `uuid-${name}`, state: "online", hostname: `${name}.internal`, name,
    os: "linux", kernel: "5.15.0", arch: "amd64", cpu_model: "EPYC 7543",
    cpu_cores: 64, mem_total_gib: 512, disk_total_gib: 1024, gpu_count: 0,
    agent_version: "0.9.7", has_ssh_credential: false, gpus: [], created_at: "2026-11-01T08:00:00Z",
    runtime_choices: ["auto", "docker", "nerdctl", "udocker"],
    ...(source ? { source } : {}),
  };
}
// The four ways a node is in the fleet, as dto.NodeSource states them (absent fields are omitted on the wire).
const byHand = node("by-hand");
const instance = node("ecs-a10", { resource: "vm", provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun" });
const accountClusterNode = node("ack-worker", { resource: "kubernetes", provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_uuid: CLUSTER_OF_ACCOUNT, cluster_name: "prod-gpu" });
const ownClusterNode = node("k3s-worker", { resource: "kubernetes", cluster_uuid: CLUSTER_ON_ITS_OWN, cluster_name: "edge-k3s" });
const otherAccount = node("pod-1", { resource: "container", provider_id: 5, provider_name: "pods", provider_kind: "runpod" });
const fleet = [byHand, instance, accountClusterNode, ownClusterNode, otherAccount];

const originals = { ...api };
let queries: ListQuery[] = [];
let failing: Error | null = null;

// Every source the fleet has, as the server lists them (dto.NodeSourceOption):
// accounts by name, then clusters by name, then the nodes added by hand. An
// account's count takes in the nodes of its clusters, as its filter does.
function sourceOptions(nodes: Node[]): NodeSourceOption[] {
  const providers = new Map<number, NodeSourceOption>();
  const clusters = new Map<string, NodeSourceOption>();
  let manual = 0;
  for (const { source } of nodes) {
    if (!source) { manual += 1; continue; }
    const named = { ...(source.provider_name ? { provider_name: source.provider_name } : {}), ...(source.provider_kind ? { provider_kind: source.provider_kind } : {}) };
    if (source.provider_id) {
      const option = providers.get(source.provider_id) ?? { value: `provider:${source.provider_id}`, ...named, nodes: 0 };
      providers.set(source.provider_id, { ...option, nodes: option.nodes + 1 });
    }
    if (source.cluster_uuid) {
      const option = clusters.get(source.cluster_uuid) ?? { value: `cluster:${source.cluster_uuid}`, ...named, ...(source.cluster_name ? { cluster_name: source.cluster_name } : {}), nodes: 0 };
      clusters.set(source.cluster_uuid, { ...option, nodes: option.nodes + 1 });
    }
  }
  const by = (name: (option: NodeSourceOption) => string) => (x: NodeSourceOption, y: NodeSourceOption) => name(x).localeCompare(name(y)) || x.value.localeCompare(y.value);
  return [
    ...[...providers.values()].sort(by((option) => option.provider_name ?? "")),
    ...[...clusters.values()].sort(by((option) => option.cluster_name ?? "")),
    ...(manual > 0 ? [{ value: "manual", nodes: manual }] : []),
  ];
}

// GET /nodes?page=…&source=a,b as the server answers it: a node from any of
// the sources matches (provider:<id> the account's instances and the nodes of
// its clusters, cluster:<uuid> that cluster entry's nodes, manual the nodes
// added by hand); a value of no known form is refused with 400, not ignored;
// "all" and "sources" describe the whole fleet, whatever the filter.
function nodesServer(nodes: Node[]) {
  return async (query: ListQuery): Promise<NodePage> => {
    queries.push(query);
    if (failing) throw failing;
    let matched = nodes;
    const asked = query.extra?.source;
    if (asked?.length) {
      let manual = false;
      const providers = new Set<number>();
      const clusters = new Set<string>();
      for (const value of asked.join(",").split(",").map((each) => each.trim())) {
        if (value === "manual") manual = true;
        else if (/^provider:\d+$/.test(value) && Number(value.slice(9)) > 0) providers.add(Number(value.slice(9)));
        else if (value.startsWith("cluster:") && value.length > 8) clusters.add(value.slice(8));
        else throw new APIError("LIST_FILTER_INVALID", { name: "source", value }, `unknown source filter "${value}"`, 400);
      }
      matched = nodes.filter(({ source }) => !source ? manual : (!!source.provider_id && providers.has(source.provider_id)) || (!!source.cluster_uuid && clusters.has(source.cluster_uuid)));
    }
    if (query.state?.length) matched = matched.filter((item) => query.state!.includes(item.state));
    const start = (query.page - 1) * query.page_size;
    return {
      items: matched.slice(start, start + query.page_size), page: query.page, page_size: query.page_size, total: matched.length, all: nodes.length,
      labels: [], sources: sourceOptions(nodes),
    };
  };
}

function stubApi(nodes: Node[] = fleet) {
  queries = [];
  failing = null;
  Object.assign(api, {
    me: async () => ({ role: "admin" }),
    agentUpgradeRuns: async () => ({ items: [], total: 0, page: 1, page_size: 20 }),
    nodes: async () => { throw new Error("the node page fetched every node"); },
    nodesPage: nodesServer(nodes),
    health: async () => ({ version: "0.9.7" }),
    agentUpgradeErrors: async () => ({}),
    engines: async () => list([]),
  });
}

afterEach(async () => {
  await act(async () => { cleanup(); message.destroy(); });
  Object.assign(api, originals);
});

// A page to come from, so going back tells a replaced address from a pushed one.
function renderAt(address: string, language: keyof typeof languages = "en") {
  const router = createMemoryRouter(
    [{ path: "/nodes", element: <NodesPage /> }, { path: "/providers", element: <p>the external platforms list</p> }],
    { initialEntries: ["/providers", address], initialIndex: 1 },
  );
  render(<I18nextProvider i18n={languages[language].i18n}><RouterProvider router={router} /></I18nextProvider>);
  return { router, address: () => router.state.location.pathname + router.state.location.search };
}

/** The Source cell of the row that lists the named node. */
async function sourceCell(name: string, title = "Source") {
  const row = (await screen.findByRole("button", { name })).closest("tr") as HTMLElement;
  const column = screen.getAllByRole("columnheader").findIndex((header) => header.textContent === title);
  assert.ok(column > 0, `a "${title}" column`);
  return within(within(row).getAllByRole("cell")[column]);
}
/** The names of the listed nodes, top to bottom. */
const listed = () => screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("button")[0].textContent);
const everyNode = fleet.map((item) => item.name);
/** Opens the funnel on the Source header and returns its choices. */
async function sourceFilter(title = "Filter by Source") {
  fireEvent.click(screen.getByRole("button", { name: title }));
  return within(await screen.findByRole("group", { name: title }));
}
const options = (filter: ReturnType<typeof within>) => filter.getAllByRole("checkbox").map((option: HTMLElement) => option.getAttribute("aria-label"));
const ticked = (filter: ReturnType<typeof within>) => filter.getAllByRole("checkbox").filter((option: HTMLElement) => option.getAttribute("aria-checked") === "true").map((option: HTMLElement) => option.getAttribute("aria-label"));
/** The filters in effect, as the row above the table states them; each is the button that clears it. */
const chips = (group = "Filtered", clear = /^Clear filter: /) => screen.queryAllByRole("group", { name: group }).flatMap((row) => within(row).queryAllByRole("button", { name: clear }).map((chip) => chip.textContent));
/** The source filter of every request so far. */
const asked = () => queries.map((query) => query.extra?.source);

test("a node added by hand says so; one that joined through an external platform links to that platform's row and says what it is there", async () => {
  stubApi();
  renderAt("/nodes");

  const manual = await sourceCell("by-hand");
  assert.ok(manual.getByText("Added by hand"));
  assert.equal(manual.queryAllByRole("link").length, 0, "nothing to open for a node added by hand");

  // An instance of an account: the account's row.
  const onInstance = await sourceCell("ecs-a10");
  assert.equal(onInstance.getByRole("link", { name: "ali-prod" }).getAttribute("href"), "/providers?open=p3");
  assert.ok(onInstance.getByText("Alibaba Cloud · VM"));

  // A node of a cluster connected from an account: the account's row, with that cluster's group brought into view.
  const inAccountCluster = await sourceCell("ack-worker");
  assert.equal(inAccountCluster.getByRole("link", { name: "ali-prod" }).getAttribute("href"), `/providers?open=p3&cluster=${CLUSTER_OF_ACCOUNT}`);
  assert.ok(inAccountCluster.getByText("Alibaba Cloud · Cluster prod-gpu"));

  // A node of a cluster connected on its own: the cluster is the platform.
  const inOwnCluster = await sourceCell("k3s-worker");
  assert.equal(inOwnCluster.getByRole("link", { name: "edge-k3s" }).getAttribute("href"), `/providers?open=k${CLUSTER_ON_ITS_OWN}`);
  assert.ok(inOwnCluster.getByText("Kubernetes cluster"));
  assert.equal(inOwnCluster.queryAllByText(/·/).length, 0, "no vendor to name before it");

  const container = await sourceCell("pod-1");
  assert.equal(container.getByRole("link", { name: "pods" }).getAttribute("href"), "/providers?open=p5");
  assert.ok(container.getByText("RunPod · Container"));
});

// The server could not read where this node came from (dto.Node source_unknown): it has no source, yet was not added by hand.
const unread: Node = { ...node("lost-1"), source_unknown: true };

for (const [language, copy] of [["en", { unread: "Source could not be read", manual: "Added by hand", title: "Source" }], ["zh", { unread: "来源读取失败", manual: "手动添加", title: "来源" }]] as const) {
  test(`in ${language} a node whose source the server could not read says so, not that it was added by hand, and links nowhere`, async () => {
    stubApi([byHand, unread, instance]);
    renderAt("/nodes", language);
    const cell = await sourceCell("lost-1", copy.title);
    assert.ok(cell.getByText(copy.unread));
    assert.equal(cell.queryAllByText(copy.manual).length, 0);
    assert.equal(cell.queryAllByRole("link").length, 0, "no platform to open");
    // A node added by hand, which the server sends without the flag, still says so.
    assert.ok((await sourceCell("by-hand", copy.title)).getByText(copy.manual));
  });
}

test("a source that lacks a name, a resource or a known kind still shows what is known", async () => {
  stubApi([
    // The account is no longer listed, so it has no name: the vendor stands in for it.
    node("unnamed-account", { resource: "vm", provider_id: 7, provider_kind: "aliyun" }),
    // The kind does not say what its instances are.
    node("no-resource", { resource: "", provider_id: 8, provider_name: "lab", provider_kind: "runpod" }),
    // A kind the console has no name for shows as the server sent it.
    node("new-kind", { resource: "host", provider_id: 9, provider_name: "racks", provider_kind: "newcloud" }),
    node("unnamed-cluster", { resource: "kubernetes", cluster_uuid: CLUSTER_ON_ITS_OWN }),
  ]);
  renderAt("/nodes");

  const unnamed = await sourceCell("unnamed-account");
  assert.equal(unnamed.getByRole("link", { name: "Alibaba Cloud" }).getAttribute("href"), "/providers?open=p7");
  assert.ok(unnamed.getByText("Alibaba Cloud · VM"));

  const bare = await sourceCell("no-resource");
  assert.ok(bare.getByRole("link", { name: "lab" }));
  assert.ok(bare.getByText("RunPod"), "the vendor alone");
  assert.equal(bare.queryAllByText(/·/).length, 0);

  const unknown = await sourceCell("new-kind");
  assert.ok(unknown.getByRole("link", { name: "racks" }));
  assert.ok(unknown.getByText("newcloud · Host"));

  const cluster = await sourceCell("unnamed-cluster");
  assert.equal(cluster.getByRole("link", { name: "Kubernetes cluster" }).getAttribute("href"), `/providers?open=k${CLUSTER_ON_ITS_OWN}`);
});

test("the Source header's filter offers every source the fleet has, each by its name, and its search box narrows them", async () => {
  stubApi();
  renderAt("/nodes");
  await screen.findByRole("button", { name: "by-hand" });
  assert.deepEqual(queries.map((query) => query.extra?.source !== undefined), queries.map(() => false), "an unfiltered list names no filter of its own in the request");

  const filter = await sourceFilter();
  // Accounts by their name, a cluster by its name and the account it was connected from, then the nodes added by hand.
  assert.deepEqual(options(filter), ["ali-prod", "pods", "Cluster edge-k3s", "Cluster prod-gpu (ali-prod)", "Added by hand"]);
  assert.deepEqual(ticked(filter), []);
  assert.deepEqual(chips(), []);

  const search = screen.getByRole("textbox", { name: "Search options" });
  fireEvent.change(search, { target: { value: "ALI" } });
  await waitFor(() => assert.deepEqual(options(filter), ["ali-prod", "Cluster prod-gpu (ali-prod)"]));
  fireEvent.change(search, { target: { value: "hand" } });
  await waitFor(() => assert.deepEqual(options(filter), ["Added by hand"]));
});

test("ticking sources asks the server for the nodes of any of them and keeps the choice in the address, replacing it; the chip above the table clears it", async () => {
  stubApi();
  const { router, address } = renderAt("/nodes?view=compact");
  await screen.findByRole("button", { name: "by-hand" });

  const filter = await sourceFilter();
  fireEvent.click(filter.getByRole("checkbox", { name: "ali-prod" }));
  await waitFor(() => assert.deepEqual(listed(), ["ecs-a10", "ack-worker"], "the account's instances and the nodes of its clusters"));
  assert.deepEqual(queries.at(-1)?.extra?.source, ["provider:3"]);
  assert.deepEqual(queries.at(-1)?.extra?.agent_upgrade, ["all"], "beside the agent version filter, which every request names");
  assert.equal(address(), "/nodes?view=compact&source=provider%3A3", "the rest of the address stays");
  assert.ok(screen.getByText("2/5 nodes"));

  fireEvent.click(filter.getByRole("checkbox", { name: "Added by hand" }));
  await waitFor(() => assert.deepEqual(listed(), ["by-hand", "ecs-a10", "ack-worker"]));
  assert.deepEqual(queries.at(-1)?.extra?.source, ["provider:3", "manual"]);
  assert.equal(address(), "/nodes?view=compact&source=provider%3A3%2Cmanual");
  assert.deepEqual(ticked(filter), ["ali-prod", "Added by hand"]);
  assert.deepEqual(chips(), ["Source: ali-prod, Added by hand"]);

  fireEvent.click(screen.getByRole("button", { name: "Clear filter: Source ali-prod, Added by hand" }));
  await waitFor(() => assert.deepEqual(listed(), everyNode));
  assert.equal(address(), "/nodes?view=compact");
  assert.equal((queries.at(-1) as ListQuery).extra?.source, undefined, "the request names no source again");
  assert.deepEqual(chips(), []);

  // Three choices later the page before the list is still one step back.
  await act(() => router.navigate(-1));
  assert.equal(address(), "/providers");
});

test("a status filter set on a list opened on a source keeps the source: both go to the server and the address is left as it was", async () => {
  const offline = { ...node("ecs-down", { resource: "vm", provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun" }), state: "offline" as const };
  stubApi([...fleet, offline]);
  const { address } = renderAt(platformNodesPath(3));
  await screen.findByRole("button", { name: "ecs-down" });
  assert.deepEqual(listed(), ["ecs-a10", "ack-worker", "ecs-down"]);

  fireEvent.click(screen.getByRole("button", { name: "Filter by Status" }));
  fireEvent.click(within(await screen.findByRole("group", { name: "Filter by Status" })).getByRole("checkbox", { name: "Offline" }));
  await waitFor(() => assert.deepEqual(listed(), ["ecs-down"]));
  assert.deepEqual([queries.at(-1)?.state, queries.at(-1)?.extra?.source], [["offline"], ["provider:3"]]);
  assert.equal(address(), platformNodesPath(3));
  assert.deepEqual(chips(), ["Status: Offline", "Source: ali-prod"]);
});

// What an external platform's node count links to (tests/providerPlatformRows.test.tsx asserts the links).
const landings: Array<[string, string, string, string[]]> = [
  ["an account", platformNodesPath(3), "ali-prod", ["ecs-a10", "ack-worker"]],
  ["a cluster connected from an account", kubeClusterNodesPath(CLUSTER_OF_ACCOUNT), "Cluster prod-gpu (ali-prod)", ["ack-worker"]],
  ["a cluster connected on its own", kubeClusterNodesPath(CLUSTER_ON_ITS_OWN), "Cluster edge-k3s", ["k3s-worker"]],
];
for (const [what, path, name, nodes] of landings) {
  test(`the address of the nodes of ${what} opens the list already filtered to them, with the source ticked and stated above the table`, async () => {
    stubApi();
    renderAt(path);
    await screen.findByRole("button", { name: nodes[0] });
    const value = decodeURIComponent(path.slice(path.indexOf("=") + 1));
    assert.deepEqual(asked(), queries.map(() => [value]), "from the very first request");
    assert.deepEqual(listed(), nodes);
    assert.deepEqual(chips(), [`Source: ${name}`]);
    assert.deepEqual(ticked(await sourceFilter()), [name]);
  });
}

test("source values the console does not know are sent as written, beside the well-formed ones, and the server's refusal names the one it refused; nothing is listed as if unfiltered", async () => {
  stubApi();
  renderAt("/nodes?source=provider:abc,%20manual%20,manual,,nonsense");
  // The fake server refuses a malformed value with 400 as the real one does: the console does not judge the values itself.
  assert.ok((await screen.findByRole("alert")).textContent?.includes('unknown source filter "provider:abc"'));
  assert.deepEqual(asked(), queries.map(() => ["provider:abc", "manual", "nonsense"]), "each once, without its spaces, the empty one left out");
  assert.equal(screen.queryAllByRole("button", { name: "by-hand" }).length, 0, "the whole fleet is not shown in place of the refused filter");
  assert.ok(screen.getByRole("button", { name: "Retry" }));
});

test("a source the address names but no node came from is still offered, as the chosen source, says the list is filtered, and unticking it brings the fleet back", async () => {
  stubApi();
  const { address } = renderAt(platformNodesPath(42));

  assert.ok(await screen.findByText("No nodes match the search or the selected labels"));
  assert.equal(screen.queryAllByText("No nodes yet. Click Add node to onboard one.").length, 0, "the fleet is not empty, the filter is");
  assert.deepEqual(asked(), queries.map(() => ["provider:42"]));
  assert.deepEqual(chips(), ["Source: The chosen source"]);

  const filter = await sourceFilter();
  assert.deepEqual(options(filter), ["ali-prod", "pods", "Cluster edge-k3s", "Cluster prod-gpu (ali-prod)", "Added by hand", "The chosen source"]);
  assert.deepEqual(ticked(filter), ["The chosen source"]);
  fireEvent.click(filter.getByRole("checkbox", { name: "The chosen source" }));
  await waitFor(() => assert.deepEqual(listed(), everyNode));
  assert.equal(address(), "/nodes");
  assert.deepEqual(chips(), []);
  await waitFor(() => assert.deepEqual(options(filter), ["ali-prod", "pods", "Cluster edge-k3s", "Cluster prod-gpu (ali-prod)", "Added by hand"], "nothing names it any more"));
});

test("a filtered list that fails to load keeps its filter, and its retry asks for the same source", async () => {
  stubApi();
  failing = new Error("nodes unavailable");
  const { address } = renderAt(kubeClusterNodesPath(CLUSTER_ON_ITS_OWN));

  assert.ok(await screen.findByText("nodes unavailable"));
  assert.deepEqual(chips(), ["Source: The chosen source"], "no answer yet to read the cluster's name from");

  failing = null;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("button", { name: "k3s-worker" });
  assert.deepEqual(queries.at(-1)?.extra?.source, [`cluster:${CLUSTER_ON_ITS_OWN}`]);
  assert.deepEqual(listed(), ["k3s-worker"]);
  assert.deepEqual(chips(), ["Source: Cluster edge-k3s"]);
  assert.equal(address(), kubeClusterNodesPath(CLUSTER_ON_ITS_OWN));
});

test("in Chinese the column, what a node is on its platform, the filter's choices and the filter in effect are all translated", async () => {
  stubApi();
  renderAt("/nodes", "zh");
  assert.ok((await sourceCell("by-hand", "来源")).getByText("手动添加"));
  // A VM is 虚拟机 in Chinese, the same word the External Platforms page uses for it.
  assert.ok((await sourceCell("ecs-a10", "来源")).getByText("阿里云 · 虚拟机"));
  assert.ok((await sourceCell("ack-worker", "来源")).getByText("阿里云 · 集群 prod-gpu"));
  assert.ok((await sourceCell("k3s-worker", "来源")).getByText("Kubernetes 集群"));
  assert.ok((await sourceCell("pod-1", "来源")).getByText("RunPod · 容器实例"));
  assert.deepEqual(options(await sourceFilter("按「来源」筛选")), ["ali-prod", "pods", "集群 edge-k3s", "集群 prod-gpu（ali-prod）", "手动添加"]);
  await act(async () => { cleanup(); });

  stubApi();
  renderAt(`/nodes?source=provider%3A42,cluster%3A${CLUSTER_OF_ACCOUNT}`, "zh");
  await screen.findByRole("button", { name: "ack-worker" });
  assert.deepEqual(chips("已筛选", /^清除筛选：/), ["来源：所选来源、集群 prod-gpu（ali-prod）"]);
  assert.ok(!document.body.textContent?.includes("{{"), "no sentence is left with an unfilled placeholder");
});

// The contract between the two pages: what an external platform's node count
// links to is exactly what the node list reads back as its filter.
test("the node-list address of an account or a cluster names it as one source value, and reads back as that source whatever the UUID holds", () => {
  const read = (path: string) => nodeSourcesOf(new URLSearchParams(path.slice(path.indexOf("?"))));
  assert.equal(platformNodesPath(3), "/nodes?source=provider%3A3");
  assert.equal(kubeClusterNodesPath(CLUSTER_OF_ACCOUNT), `/nodes?source=cluster%3A${CLUSTER_OF_ACCOUNT}`);
  assert.deepEqual(read(platformNodesPath(3)), ["provider:3"]);
  assert.deepEqual(read(kubeClusterNodesPath(CLUSTER_OF_ACCOUNT)), [`cluster:${CLUSTER_OF_ACCOUNT}`]);
  assert.equal(kubeClusterNodesPath("u-1&source=manual"), "/nodes?source=cluster%3Au-1%26source%3Dmanual", "the UUID cannot smuggle a second filter in");
  assert.deepEqual(read(kubeClusterNodesPath("u-1&source=manual")), ["cluster:u-1&source=manual"]);
});

test("the address's sources are read as written, each once, without the spaces around it and without empty ones; which are valid is the server's to say", () => {
  const read = (search: string) => nodeSourcesOf(new URLSearchParams(search));
  assert.deepEqual(read(""), []);
  assert.deepEqual(read("source="), []);
  assert.deepEqual(read("source=%20,,"), [], "spaces and empty values are no source");
  assert.deepEqual(read("source=cluster:u-1,manual,provider:12"), ["cluster:u-1", "manual", "provider:12"], "in the order asked");
  assert.deepEqual(read("source=manual,provider:3,manual,provider:3"), ["manual", "provider:3"]);
  assert.deepEqual(read("source=%20manual%20,%20provider:7"), ["manual", "provider:7"]);
  // Values of no form the console knows go to the server, which refuses them by name, rather than being dropped unseen.
  assert.deepEqual(read("source=provider:0,provider:-1,provider:abc,cluster:,platform:3,Manual,3"), ["provider:0", "provider:-1", "provider:abc", "cluster:", "platform:3", "Manual", "3"]);
});

test("a source that names neither an account nor a cluster opens the external platforms list itself", () => {
  assert.equal(platformPath({}), "/providers");
});

test("a platform's address opens its row: an account's by id, with one of its clusters brought into view by the entry's UUID, encoded; a cluster on its own is the row itself", () => {
  assert.equal(platformPath({ provider_id: 3 }), "/providers?open=p3");
  assert.equal(platformPath({ provider_id: 3, cluster_uuid: "e-1" }), "/providers?open=p3&cluster=e-1");
  assert.equal(platformPath({ provider_id: 3, cluster_uuid: "u-1&open=k2" }), "/providers?open=p3&cluster=u-1%26open%3Dk2", "the UUID cannot open a second row");
  assert.equal(platformPath({ cluster_uuid: "c-7" }), "/providers?open=kc-7");
  const read = new URLSearchParams(platformPath({ provider_id: 3, cluster_uuid: "u-1&open=k2" }).split("?")[1]);
  assert.deepEqual([read.getAll("open"), read.get("cluster"), read.has("tab")], [["p3"], "u-1&open=k2", false]);
});
