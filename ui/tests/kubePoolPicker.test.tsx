// The GPU node pool choice of an entry made from a cloud cluster
// (ui/src/pages/kubernetes/KubePoolPicker.tsx): the pools chosen as a list,
// each with its "In the entry" tag, taken out with its own button while more
// than one is chosen; the cluster's other pools added from a combobox; and,
// when a pool the entry holds now is taken out, a warning that its nodes
// leave. Where it is used: the connect drawer (tests/kubeClusterForm.test.tsx,
// tests/kubeCloudClusters.test.tsx) and the pools drawer
// (tests/kubeCloudClusters.test.tsx).
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { freshPopupFramesPerTest } from "./helpers/platformDialog.ts";
import { l20Pool } from "./helpers/platformFixtures.ts";
import { poolPicker } from "./helpers/poolPicker.ts";
import type { ProviderClusterPool } from "../src/api";

const languages = { en: await createI18n(["kubernetes", "providers"], "en"), zh: await createI18n(["kubernetes", "providers"], "zh") };
const k = languages.en.locale.kubernetes;
const p = languages.en.locale.providers;
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: KubePoolPicker } = await import("../src/pages/kubernetes/KubePoolPicker.tsx");

await freshPopupFramesPerTest();
afterEach(() => cleanup());

const POOLS: ProviderClusterPool[] = [
  { ...l20Pool, id: "np-a", name: "a10", gpu_model: "NVIDIA A10", nodes: 2, in_entry: true },
  { ...l20Pool, id: "np-l", name: "l20", nodes: 1 },
  { ...l20Pool, id: "np-p", name: "ppu", gpu_model: "PPU-ZW810E", gpu_count: 8, nodes: 1, vendor: "ppu" },
  { ...l20Pool, id: "np-m", name: "mixed", gpu_model: "", gpu_count: 2, nodes: 3 },
];
const A10 = "a10 · NVIDIA A10 ×1 · 2 nodes";
const L20 = "l20 · NVIDIA L20 ×1 · 1 nodes";
const PPU = "ppu · PPU-ZW810E ×8 · 1 nodes";
const MIXED = "mixed · mixed models ×2 · 3 nodes";
const LABEL = k.drawer.pools;
const remove = (name: string) => k.pools.removeOne.replace("{{name}}", name);

/** The picker as a form holds it: the choice is state the picker reports into; every change is recorded. */
function mount({ selected = [] as string[], held = [] as string[], disabled = false, pools = POOLS, language = "en" as keyof typeof languages } = {}) {
  const changes: string[][] = [];
  function Harness() {
    const [ids, setIds] = React.useState(selected);
    return <KubePoolPicker pools={pools} selected={ids} onChange={(next) => { changes.push(next); setIds(next); }} disabled={disabled} label={LABEL} held={held} />;
  }
  render(<I18nextProvider i18n={languages[language].i18n}><Harness /></I18nextProvider>);
  return { changes, user: userEvent.setup({ document: dom.window.document }) };
}
const picker = poolPicker(() => document.body, { label: LABEL, addLabel: k.pools.add, removeName: remove, inEntry: p.clusters.inEntry });
const warnings = () => screen.queryAllByText(/^Pools taken out:/);

test("the chosen pools are listed in the cluster's order, each as name, cards and nodes; a pool the entry holds is tagged; a pool of mixed models says so", async () => {
  mount({ selected: ["np-m", "np-a", "np-p"] });
  assert.deepEqual(await picker.chosen(), [A10, PPU, MIXED], "the cluster's order, not the order chosen");
  assert.deepEqual(await picker.tagged(), [A10], "only the pool already in the entry carries the tag");
  assert.ok(within(await picker.list()).getByText(p.clusters.inEntry));
  assert.deepEqual(await picker.removeButtons(), [remove("a10"), remove("ppu"), remove("mixed")], "each chosen pool can be taken out, named for it");
});

test("taking a pool out drops it from the list and from what the form holds; the last pool has no remove button", async () => {
  const { changes, user } = mount({ selected: ["np-a", "np-l"] });
  await picker.remove(user, "a10");
  assert.deepEqual(changes, [["np-l"]]);
  assert.deepEqual(await picker.chosen(), [L20]);
  assert.deepEqual(await picker.removeButtons(), [], "an entry holds at least one pool: the last one stays");
  assert.equal(screen.queryAllByRole("button", { name: remove("l20") }).length, 0);
});

test("the add combobox offers only the pools not chosen, and picking one adds it to the list; once every pool is chosen it is gone", async () => {
  const { changes, user } = mount({ selected: ["np-l"] });
  const adder = await picker.adder();
  assert.ok(adder, "the add combobox is there while pools are left");
  assert.equal(adder.textContent, k.pools.add, "it shows what it does, not a value");
  assert.deepEqual(await picker.offered(user), [A10, PPU, MIXED], "the chosen pool is not offered again");
  await picker.add(user, PPU);
  assert.deepEqual(changes, [["np-l", "np-p"]]);
  assert.deepEqual(await picker.chosen(), [L20, PPU]);
  assert.equal((await picker.adder())?.textContent, k.pools.add, "the combobox holds no value after adding");
  assert.deepEqual(await picker.offered(user), [A10, MIXED]);
  await picker.add(user, A10);
  await picker.add(user, MIXED);
  assert.deepEqual(await picker.chosen(), [A10, L20, PPU, MIXED]);
  assert.equal(await picker.adder(), null, "nothing is left to add");
});

test("from the keyboard: Tab reaches a remove button and Enter takes the pool out; the add combobox opens with the arrow key, the arrows move, Enter adds and Escape closes without adding", async () => {
  const { changes, user } = mount({ selected: ["np-a", "np-l"] });
  // The remove buttons come first, in the list's order, then the combobox.
  await user.tab();
  assert.ok(document.activeElement === screen.getByRole("button", { name: remove("a10") }));
  await user.tab();
  assert.ok(document.activeElement === screen.getByRole("button", { name: remove("l20") }));
  await user.keyboard("{Enter}");
  assert.deepEqual(changes, [["np-a"]]);
  assert.deepEqual(await picker.chosen(), [A10]);

  // The combobox opens from the keyboard on the first pool left to add (a10 is chosen).
  const adder = (await picker.adder()) as HTMLElement;
  adder.focus();
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.equal(adder.getAttribute("aria-expanded"), "true"));
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: L20 })));
  await user.keyboard("{ArrowDown}");
  await waitFor(() => assert.ok(document.activeElement === screen.getByRole("option", { name: PPU })));
  await user.keyboard("{Enter}");
  await picker.closed();
  assert.deepEqual(changes.at(-1), ["np-a", "np-p"]);
  assert.deepEqual(await picker.chosen(), [A10, PPU]);

  // Escape closes the list without adding.
  await picker.open(user);
  await user.keyboard("{Escape}");
  await picker.closed();
  assert.deepEqual(await picker.chosen(), [A10, PPU]);
  assert.equal(changes.length, 2);
});

test("taking out a pool the entry holds says which and what follows; adding it back takes the warning away; a pool it did not hold warns of nothing", async () => {
  const { user } = mount({ selected: ["np-a", "np-l", "np-p"], held: ["np-a", "np-l"] });
  assert.equal(warnings().length, 0, "nothing taken out yet");
  await picker.remove(user, "ppu");
  assert.equal(warnings().length, 0, "the entry did not hold ppu: nothing leaves");
  await picker.remove(user, "a10");
  const notice = await screen.findByText(k.pools.removing.replace("{{names}}", "a10"));
  assert.ok(screen.getByText(k.pools.removingHelp));
  assert.ok(notice);
  await picker.add(user, PPU);
  await picker.remove(user, "l20");
  assert.ok(await screen.findByText(k.pools.removing.replace("{{names}}", `a10${k.vendors.separator}l20`)), "every held pool taken out, in the cluster's order");
  await picker.add(user, A10);
  assert.ok(await screen.findByText(k.pools.removing.replace("{{names}}", "l20")));
  await picker.add(user, L20);
  await waitFor(() => assert.equal(warnings().length, 0, "every held pool is back: no warning"));
  assert.equal(screen.queryAllByText(k.pools.removingHelp).length, 0);
});

test("without held pools (a cluster not yet connected) taking pools out warns of nothing", async () => {
  const { user } = mount({ selected: ["np-a", "np-l"] });
  await picker.remove(user, "a10");
  assert.equal(screen.queryAllByText(k.pools.removingHelp).length, 0);
});

test("an entry that holds none of the pools yet starts with an empty list and every pool to add", async () => {
  const { user } = mount({ selected: [] });
  assert.deepEqual(await picker.chosen(), []);
  assert.deepEqual(await picker.offered(user), [A10, L20, PPU, MIXED]);
});

test("while the form is saving nothing can be taken out or added", async () => {
  const { changes } = mount({ selected: ["np-a", "np-l"], disabled: true });
  for (const button of within(await picker.list()).getAllByRole("button")) assert.equal((button as HTMLButtonElement).disabled, true);
  const adder = (await picker.adder()) as HTMLElement;
  assert.ok(adder.hasAttribute("disabled") || adder.getAttribute("aria-disabled") === "true" || adder.hasAttribute("data-disabled"), "the combobox is disabled");
  assert.deepEqual(changes, []);
});

test("in Chinese the list, its buttons, the combobox and the warning read in Chinese", async () => {
  const zk = languages.zh.locale.kubernetes;
  const zp = languages.zh.locale.providers;
  const zhPicker = poolPicker(() => document.body, { label: LABEL, addLabel: zk.pools.add, removeName: (name) => zk.pools.removeOne.replace("{{name}}", name), inEntry: zp.clusters.inEntry });
  const { user } = mount({ selected: ["np-a", "np-l"], held: ["np-a"], language: "zh" });
  assert.deepEqual(await zhPicker.chosen(), ["a10 · NVIDIA A10 ×1 · 2 个节点", "l20 · NVIDIA L20 ×1 · 1 个节点"]);
  assert.deepEqual(await zhPicker.tagged(), ["a10 · NVIDIA A10 ×1 · 2 个节点"]);
  assert.deepEqual(await zhPicker.removeButtons(), ["移除 a10", "移除 l20"]);
  assert.ok(await zhPicker.adder());
  await zhPicker.remove(user, "a10");
  assert.ok(await screen.findByText("去掉的节点池：a10"));
  assert.ok(screen.getByText(zk.pools.removingHelp));
  assert.equal(document.body.textContent?.includes("{{"), false);
});
