import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, aliyunKind, capabilities, kubeEntry, runpodKind, servePlatforms, zstackKind, APPLY_COMMAND } from "./helpers/platformFixtures.ts";
import { field, freshPopupFramesPerTest, platformChoice } from "./helpers/platformDialog.ts";

// The quick-connect cards above the External Platforms list
// (ui/src/pages/CloudProvidersPage.tsx): one per vendor the server knows,
// saying what it connects (ui/src/pages/platforms/PlatformResourceBadge.tsx)
// and linking to the vendor's site, and for an administrator one more for a
// Kubernetes cluster connected on its own ("Local Kubernetes cluster").

const en = await createI18n(["providers", "kubernetes"], "en");
const zh = await createI18n(["providers", "kubernetes"], "zh");
const languages = { en: en.i18n, zh: zh.i18n };
const k = en.locale.kubernetes;
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

// A kind the console has no logo, site or resources for, and the server files under no platform type.
const newcloud: ProviderKindInfo = { kind: "newcloud", label: { en: "New Cloud", zh: "新云" }, category: "", capabilities, credential_fields: [], server_dial_supported: false, resources: [] };
// A kind whose resources the vendor has no product names for, and whose site is given with a path and a closing slash.
const plaincloud: ProviderKindInfo = {
  kind: "plaincloud", label: { en: "Plain Cloud", zh: "朴素云" }, category: "public_cloud", homepage: "http://cloud.example.com/console/", capabilities, credential_fields: [],
  server_dial_supported: false, resources: [{ type: "vm" }, { type: "container" }],
};

// The outcome of a sync is said in a toast, which the console's frame hosts.
function renderPage(language: keyof typeof languages = "en") {
  const view = render(
    <I18nextProvider i18n={languages[language]}>
      <FeedbackHost />
      <MemoryRouter><CloudProvidersPage /></MemoryRouter>
    </I18nextProvider>,
  );
  return { ...view, user: userEvent.setup({ document: dom.window.document }) };
}
/** The card a quick-connect button sits on. */
const cardOf = (add: HTMLElement) => add.closest(".provider-kind-card") as HTMLElement;
/** What a card says its platform connects, tag by tag. */
const tags = (card: HTMLElement) => Array.from(card.querySelectorAll('[data-slot="badge"]')).map((tag) => tag.textContent);
/** What each of a card's tags says when pointed at: the kind of resource its product name stands for; nothing when the tag is the kind itself. */
const tagTitles = (card: HTMLElement) => Array.from(card.querySelectorAll('[data-slot="badge"]')).map((tag) => tag.getAttribute("title"));
/** The quick-connect block: the section under its heading. */
const quickConnect = (heading = "Quick Connect") => screen.getByRole("heading", { level: 2, name: heading }).closest("section") as HTMLElement;
/** Lets the session lookup (and whatever follows it) settle. */
const settled = () => act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
/** Following a link is the browser's business (jsdom cannot navigate): the test stops it there and counts it. */
function linksFollowed() {
  const followed: string[] = [];
  const stop = (event: Event) => {
    const link = (event.target as HTMLElement).closest("a");
    if (!link) return;
    followed.push(link.getAttribute("href") ?? "");
    event.preventDefault();
  };
  document.addEventListener("click", stop);
  return { followed, release: () => document.removeEventListener("click", stop) };
}
const choice = platformChoice();
/** The platform the dialog is on: its vendor in the list, and the platform type whose tab is selected. */
const chosenPlatform = (dialog: HTMLElement) => within(dialog).getByRole("combobox", { name: "External platform" }).textContent;
const chosenType = (dialog: HTMLElement) => within(dialog).getByRole("tab", { selected: true }).textContent;

test("a vendor's card shows its name and logo and what it connects by product name; its site is a small link beside the name, named by the address; no capability tags", async () => {
  servePlatforms(api, { kinds: [runpodKind, aliyunKind, newcloud] });
  const { user } = renderPage();

  const add = await screen.findByRole("button", { name: "Add RunPod" });
  assert.equal(add.querySelector("img")?.getAttribute("src"), "/provider-logos/runpod.webp");
  assert.equal(add.textContent, "RunPod", "the card's button reads as the vendor's name");
  assert.deepEqual(tags(cardOf(add)), ["Pod"]);
  assert.deepEqual(tagTitles(cardOf(add)), ["Container"], "what kind of resource a Pod is, on hover");
  // The site is an icon, not a word: its name for assistive technology and its hint on hover say where it leads.
  const homepage = within(cardOf(add)).getByRole("link", { name: "Open www.runpod.io" });
  assert.equal(homepage.textContent, "", "no \"Website\" text any more");
  assert.equal(within(cardOf(add)).queryAllByText("Website").length, 0);
  assert.equal(homepage.getAttribute("href"), "https://www.runpod.io");
  assert.equal(homepage.getAttribute("target"), "_blank");
  assert.equal(homepage.getAttribute("rel"), "noopener noreferrer", "the vendor's page gets no handle on the console");
  assert.equal(add.contains(homepage), false, "the link is beside the card's button, not inside it");
  await user.hover(homepage);
  assert.equal((await screen.findByRole("tooltip")).textContent, "Open www.runpod.io");
  await user.unhover(homepage);

  // The vendor's own product names, in the server's order, without amounts: nothing is connected yet.
  const aliyun = cardOf(screen.getByRole("button", { name: "Add Alibaba Cloud" }));
  assert.deepEqual(tags(aliyun), ["ECS", "ACK"]);
  assert.deepEqual(tagTitles(aliyun), ["VM", "Kubernetes"]);

  // A kind without a bundled mark, a homepage or resources still renders, with its initial.
  const other = screen.getByRole("button", { name: "Add New Cloud" });
  assert.equal(other.querySelectorAll("img").length, 0);
  assert.equal(other.textContent, "NNew Cloud");
  assert.deepEqual(tags(cardOf(other)), []);
  assert.equal(within(cardOf(other)).queryAllByRole("link").length, 0);

  assert.equal(screen.queryAllByText("Can synchronize").length, 0);
  assert.equal(screen.queryAllByText(/Credential fields/).length, 0);

  await user.click(add);
  assert.ok(await screen.findByRole("dialog", { name: "Add External Platform" }));
});

test("following a vendor's site link does not start adding the platform; the card around it still does", async () => {
  servePlatforms(api, { kinds: [runpodKind, aliyunKind] });
  const { user } = renderPage();
  const add = await screen.findByRole("button", { name: "Add Alibaba Cloud" });
  const homepage = within(cardOf(add)).getByRole("link", { name: "Open www.aliyun.com" });
  const links = linksFollowed();
  try {
    await user.click(homepage);
    await settled();
    assert.deepEqual(links.followed, ["https://www.aliyun.com"], "the link is followed");
    assert.equal(screen.queryAllByRole("dialog").length, 0, "and nothing is being added");
  } finally {
    links.release();
  }
  await user.click(add);
  const dialog = await screen.findByRole("dialog", { name: "Add External Platform" });
  await waitFor(() => assert.equal(chosenPlatform(dialog), "Alibaba Cloud"));
  assert.equal(chosenType(dialog), "Public cloud");
});

test("the cards work from the keyboard: Tab goes from a card's button to its site link and on to the next card, and Enter on the button opens the add dialog on that vendor", async () => {
  servePlatforms(api, { kinds: [runpodKind, newcloud, aliyunKind] });
  const { user } = renderPage();
  const runpod = await screen.findByRole("button", { name: "Add RunPod" });
  const lastCard = await screen.findByRole("button", { name: "Add Local Kubernetes cluster" });
  runpod.focus();
  const stops: Array<string | null> = [];
  for (let stop = 0; stop < 5; stop += 1) {
    await user.tab();
    stops.push((document.activeElement as HTMLElement).getAttribute("aria-label"));
  }
  // A card without a site has one stop; the cluster card is the last of the block.
  assert.deepEqual(stops, ["Open www.runpod.io", "Add New Cloud", "Add Alibaba Cloud", "Open www.aliyun.com", "Add Local Kubernetes cluster"]);
  assert.ok(document.activeElement === lastCard);
  assert.equal((document.activeElement as HTMLElement).tagName, "BUTTON");
  assert.equal(screen.getByRole("link", { name: "Open www.aliyun.com" }).tabIndex, 0, "a link is a tab stop of its own");

  await user.tab({ shift: true });
  await user.tab({ shift: true });
  assert.ok(document.activeElement === screen.getByRole("button", { name: "Add Alibaba Cloud" }));
  await user.keyboard("{Enter}");
  const dialog = await screen.findByRole("dialog", { name: "Add External Platform" });
  await waitFor(() => assert.equal(chosenPlatform(dialog), "Alibaba Cloud"));
});

test("a site's link is named by its address without the protocol or a closing slash, and leads to the address as the server gave it", async () => {
  servePlatforms(api, { kinds: [plaincloud] });
  renderPage();
  const add = await screen.findByRole("button", { name: "Add Plain Cloud" });
  const homepage = within(cardOf(add)).getByRole("link");
  assert.equal(homepage.getAttribute("aria-label"), "Open cloud.example.com/console");
  assert.equal(homepage.getAttribute("href"), "http://cloud.example.com/console/");
  // No product names: the tags are the kinds of resource themselves, with nothing more to say on hover.
  assert.deepEqual(tags(cardOf(add)), ["VM", "Container"]);
  assert.deepEqual(tagTitles(cardOf(add)), [null, null]);
});

test("in zh a card's site link is named 打开官网 with the address, its tags are the product names, and a kind of resource reads in Chinese: a VM is 虚拟机", async () => {
  servePlatforms(api, { kinds: [aliyunKind, plaincloud, zstackKind] });
  const { user } = renderPage("zh");
  const add = await screen.findByRole("button", { name: "添加 阿里云" });
  const homepage = within(cardOf(add)).getByRole("link", { name: "打开官网 www.aliyun.com" });
  assert.equal(homepage.textContent, "", "no 官网 text any more");
  assert.equal(screen.queryAllByText("官网").length, 0);
  await user.hover(homepage);
  assert.equal((await screen.findByRole("tooltip")).textContent, "打开官网 www.aliyun.com");
  // A product name is the vendor's and is not translated; what it is, is.
  assert.deepEqual(tags(cardOf(add)), ["ECS", "ACK"]);
  assert.deepEqual(tagTitles(cardOf(add)), ["虚拟机", "Kubernetes"]);
  const plain = cardOf(screen.getByRole("button", { name: "添加 朴素云" }));
  assert.deepEqual(tags(plain), ["虚拟机", "容器实例"]);
  assert.deepEqual(tagTitles(plain), [null, null]);
  assert.equal(within(plain).getByRole("link").getAttribute("aria-label"), "打开官网 cloud.example.com/console");
  assert.deepEqual(tags(cardOf(screen.getByRole("button", { name: "添加 ZStack Cloud" }))), ["物理机"]);
  assert.deepEqual(tags(cardOf(await screen.findByRole("button", { name: "添加 本地 Kubernetes 集群" }))), ["Kubernetes"]);
  assert.equal(document.body.textContent?.includes("云主机"), false, "the old word for a VM is gone");
});

test("the cards sit in one tray under the Quick Connect heading; a viewer with no kind to offer gets the notice and no empty tray, an administrator still gets the cluster card", async () => {
  servePlatforms(api, { kinds: [runpodKind, zstackKind] });
  const first = renderPage();
  await screen.findByRole("button", { name: "Add Local Kubernetes cluster" });
  const block = quickConnect();
  const tray = block.querySelector('[data-slot="container"]') as HTMLElement;
  assert.deepEqual(within(tray).getAllByRole("button").map((button) => button.getAttribute("aria-label")), ["Add RunPod", "Add ZStack Cloud", "Add Local Kubernetes cluster"], "every card is in the tray");
  assert.equal(block.querySelectorAll('[data-slot="container"]').length, 1);
  first.unmount();

  // The kind schema could not be read. A viewer has nothing to pick: no tray is drawn around nothing.
  servePlatforms(api, { kinds: new Error("schema unavailable"), admin: false });
  const second = renderPage();
  assert.ok(await screen.findByText("Provider schema was not loaded"));
  await settled();
  assert.equal(quickConnect().querySelectorAll('[data-slot="container"]').length, 0);
  assert.equal(within(quickConnect()).queryAllByRole("button").length, 0);
  second.unmount();

  // An administrator can still connect a cluster on its own: the tray holds that one card.
  servePlatforms(api, { kinds: new Error("schema unavailable") });
  renderPage();
  const add = await screen.findByRole("button", { name: "Add Local Kubernetes cluster" });
  assert.ok(screen.getByText("Provider schema was not loaded"));
  assert.ok(quickConnect().querySelector('[data-slot="container"]')?.contains(add));
  assert.deepEqual(within(quickConnect()).getAllByRole("button").map((button) => button.getAttribute("aria-label")), ["Add Local Kubernetes cluster"]);
});

test("public and private clouds share one list, each with its platform type, and sync together", async () => {
  const synced: number[] = [];
  servePlatforms(api, {
    providers: [account(1, "Pods", "runpod", { instance_count: 3, node_count: 2 }), account(2, "Hosts", "zstack", { instance_count: 2, node_count: 1 })],
    kinds: [runpodKind, zstackKind],
  });
  Object.assign(api, { syncProvider: async (id: number) => { synced.push(id); return { count: 0, instances: [] }; } });
  const { user } = renderPage();

  await screen.findByText("Pods");
  assert.ok(screen.getByText("Hosts"));
  assert.ok(screen.getByRole("columnheader", { name: "Platform type" }));
  assert.ok(screen.getByText("Public cloud"));
  assert.ok(screen.getByText("Private cloud"));
  assert.ok(screen.getByText("External platforms: 2 · Compute nodes: 3"));
  assert.equal(screen.queryAllByText(/Invalid: /).length, 0);
  assert.equal(screen.queryAllByRole("alert").length, 0);

  await user.click(screen.getByRole("button", { name: "Sync All" }));
  assert.ok(await screen.findByText("Synchronized 2 external platforms that support synchronization"));
  assert.deepEqual(synced, [1, 2]);
});

test("an invalid account shows in the summary and raises the warning above the list", async () => {
  servePlatforms(api, { providers: [account(2, "Hosts", "zstack", { instance_count: 2, state: "invalid" })], kinds: [zstackKind] });
  renderPage();

  await screen.findByText("Hosts");
  assert.ok(screen.getByText("Invalid: 1"));
  // A notice, not an alert: the list below is still good.
  const warning = screen.getByText("Some external platforms have errors").closest('[role="status"]') as HTMLElement;
  assert.ok(warning.textContent?.includes("Review the external platform's status or synchronize again"));
  const add = screen.getByRole("button", { name: "Add ZStack Cloud" });
  assert.equal(add.querySelector("img")?.getAttribute("src"), "/provider-logos/zstack.svg");
});

test("an administrator gets a last card for a local Kubernetes cluster; it opens the add dialog on Kubernetes, where the cluster is connected and joins the list behind it", async () => {
  const server = servePlatforms(api, { providers: [account(1, "Pods", "runpod")], kinds: [runpodKind] });
  const created: unknown[] = [];
  Object.assign(api, {
    createKubeCluster: async (body: { name: string }) => {
      created.push(body);
      const cluster = kubeEntry({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0 });
      server.clusters = [cluster];
      return cluster;
    },
  });
  const { user } = renderPage();

  const add = await screen.findByRole("button", { name: "Add Local Kubernetes cluster" });
  const cards = screen.getAllByRole("button", { name: /^Add (?!External Platform)/ });
  assert.ok(cards.at(-1) === add, "after the vendors");
  assert.equal(add.textContent, "Local Kubernetes cluster", "its mark is an icon, not an initial");
  assert.deepEqual(tags(cardOf(add)), ["Kubernetes"]);
  assert.equal(within(cardOf(add)).queryAllByRole("link").length, 0, "no vendor, no site");
  assert.equal(screen.queryAllByRole("dialog").length, 0);

  await user.click(add);
  // The same dialog that adds an account, opened on Kubernetes: the cluster's form, not a vendor's credentials.
  const dialog = await screen.findByRole("dialog", { name: "Add External Platform" });
  assert.equal(chosenType(dialog), "Local Kubernetes cluster");
  assert.equal(within(dialog).queryAllByRole("combobox", { name: "External platform" }).length, 0, "a cluster has no vendor to choose");
  const form = within(dialog);
  await user.type(await form.findByLabelText(field(k.drawer.name)), "edge");
  const before = server.reads.clusters;
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.deepEqual(created, [{ name: "edge", server_url: "http://localhost" }]));
  assert.ok(await form.findByText(k.created.note));
  assert.equal(form.getByLabelText(k.created.command).textContent, APPLY_COMMAND);
  await waitFor(() => assert.equal(server.reads.clusters, before + 1, "the cluster list is read again"));
  assert.ok(await screen.findByText("edge", { selector: "td" }, { container: document.body }), "the cluster is a row of the list behind the dialog");

  // The dialog stays on the command until it is closed.
  // The dialog's corner button is named Close too; the footer's comes last.
  await user.click(form.getAllByRole("button", { name: k.drawer.close }).at(-1) as HTMLElement);
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));
  assert.equal(server.reads.clusters, before + 1, "closing reads nothing more");
});

test("a card of a vendor opens the add dialog on that vendor, not on Kubernetes", async () => {
  servePlatforms(api, { kinds: [runpodKind, zstackKind] });
  const { user } = renderPage();
  await user.click(await screen.findByRole("button", { name: "Add ZStack Cloud" }));
  const dialog = await screen.findByRole("dialog", { name: "Add External Platform" });
  await waitFor(() => assert.equal(chosenPlatform(dialog), "ZStack Cloud"));
  assert.equal(chosenType(dialog), "Private cloud", "on the tab of that vendor's type");
  assert.equal(within(dialog).queryAllByRole("button", { name: k.drawer.submit }).length, 0, "a vendor's form is added, not connected");
  // A kind that asks for no credentials has no Credentials heading over nothing; the dialog's only heading is its title.
  assert.deepEqual(within(dialog).getAllByRole("heading").map((heading) => heading.textContent), ["Add External Platform"]);
  assert.ok(within(dialog).getByRole("button", { name: "Add" }));
});

test("a viewer has no card for a local Kubernetes cluster and is not offered Kubernetes in the add dialog: connecting one makes an enrollment token", async () => {
  servePlatforms(api, { kinds: [runpodKind], admin: false });
  const { user } = renderPage();
  await screen.findByRole("button", { name: "Add RunPod" });
  await settled();
  assert.equal(screen.queryAllByRole("button", { name: "Add Local Kubernetes cluster" }).length, 0);
  assert.equal(screen.queryAllByText("Local Kubernetes cluster").length, 0);

  await user.click(screen.getByRole("button", { name: "Add External Platform" }));
  await screen.findByRole("dialog", { name: "Add External Platform" });
  assert.deepEqual(await choice.types(), ["Public cloud"], "no Local Kubernetes cluster tab");
  assert.deepEqual(await choice.choices(user), ["RunPod"]);
});

test("the add dialog offers an administrator Kubernetes after the vendors", async () => {
  servePlatforms(api, { kinds: [runpodKind] });
  const { user } = renderPage();
  await screen.findByRole("button", { name: "Add Local Kubernetes cluster" });
  await user.click(screen.getByRole("button", { name: "Add External Platform" }));
  const dialog = await screen.findByRole("dialog", { name: "Add External Platform" });
  await waitFor(() => assert.equal(chosenPlatform(dialog), "RunPod", "opened from the header it starts on the first vendor"));
  assert.deepEqual(await choice.types(), ["Public cloud", "Local Kubernetes cluster"]);
  assert.equal(chosenType(dialog), "Public cloud");
  // That tab is the cluster form, the same the last card opens.
  await choice.pickType(user, "Local Kubernetes cluster");
  assert.ok(await within(dialog).findByRole("button", { name: k.drawer.submit }));
  assert.equal(within(dialog).queryAllByRole("combobox", { name: "External platform" }).length, 0);
});
