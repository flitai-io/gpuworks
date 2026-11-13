import { schedulingColors } from "../src/pages/scheduling/palette";
import assert from "node:assert/strict";
import { test } from "node:test";
import { arrange, buildModel, compareNames, countStates, freeMiB, NO_FILTERS, type OverviewModel } from "../src/pages/scheduling/overviewModel.ts";
import { awakeFill, fillOf, mix, stateFill } from "../src/pages/scheduling/cellColor.ts";
import { colors } from "../src/theme/tokens.ts";
import { cpuClock } from "./helpers/cpuClock.ts";
import { bigFleet, card, overviewOf, smallFleet, STATES } from "./helpers/overviewFixtures.ts";

// The overview's decoding, filtering, grouping and sorting, with no DOM.

const model = (): OverviewModel => buildModel(smallFleet()).model;
const names = (arranged: ReturnType<typeof arrange>) => arranged.sections.map((s) => s.nodes.map((n) => `${n.node.name}:${n.cards.map((c) => c.index).join("")}`));

test("the compact cards decode into the states the server names", () => {
  const { nodes } = model();
  const states = nodes.map((n) => n.cards.map((c) => c.state));
  assert.deepEqual(states, [["awake", "awake"], ["offline"], ["held_outside", "asleep", "free"]]);
  const [a] = nodes;
  assert.deepEqual(a.cards[1].occupants, [{ deployment: 1, asleep: false }, { deployment: 2, asleep: true }]);
  assert.equal(a.cards[1].switching, true);
  assert.equal(a.cards[0].switching, false);
  assert.equal(a.cards[1].model, "L20");
  assert.equal(nodes[2].cards[0].model, "H100");
  assert.deepEqual(a.labels, { zone: "a" });
  assert.equal(a.contention, 5);
  // The page derives no state: a card is what the server says, whatever its shares or its node.
  const odd = buildModel(overviewOf([
    { uuid: "x", name: "x", online: true, cards: [card(0, 0, 50, 0, { state: "unhealthy" }), card(1, 0, 50, 0, { state: "held_outside" })] },
  ], ["L20"])).model;
  assert.deepEqual(odd.nodes.map((n) => n.cards.map((c) => c.state)), [["unhealthy", "held_outside"]]);
  // The positions are the response's own: a server that lists its states in another order is read by it.
  const reordered = smallFleet();
  const order = [...STATES].reverse();
  for (const node of reordered.nodes) for (const wire of node.cards) wire[5] = order.indexOf(STATES[wire[5]]);
  reordered.states = order;
  assert.deepEqual(buildModel(reordered).model.nodes.map((n) => n.cards.map((c) => c.state)), states);
});

test("the labels of the fleet are listed by key with their values", () => {
  assert.deepEqual(model().labels, [{ key: "zone", values: ["a", "c"] }]);
});

test("a poll that finds a node unchanged keeps its view; a changed node gets a new one", () => {
  const first = buildModel(smallFleet());
  const same = buildModel(smallFleet(), first.seen);
  assert.ok(same.model.nodes.every((node, i) => node === first.model.nodes[i]), "unchanged nodes are the same objects");
  const changed = smallFleet();
  changed.nodes[0].cards[0][3] = 60;
  const next = buildModel(changed, same.seen);
  assert.notEqual(next.model.nodes[0], first.model.nodes[0]);
  assert.equal(next.model.nodes[0].cards[0].awakePct, 60);
  assert.equal(next.model.nodes[1], first.model.nodes[1]);
});

test("filters keep the cards of the chosen models and drop the nodes left with none", () => {
  const arranged = arrange(model(), { ...NO_FILTERS, models: ["H100"] }, "node", "name");
  assert.deepEqual(names(arranged), [["gpu-c:012"]]);
  assert.equal(arranged.nodes, 1);
  assert.equal(arranged.cards, 3);
});

test("the label filter keeps the nodes with that key=value, and a malformed one keeps none", () => {
  assert.deepEqual(names(arrange(model(), { ...NO_FILTERS, label: "zone=a" }, "node", "name")), [["gpu-a:01"]]);
  assert.deepEqual(names(arrange(model(), { ...NO_FILTERS, label: "zone=nowhere" }, "node", "name")), []);
  assert.equal(arrange(model(), { ...NO_FILTERS, label: "zone" }, "node", "name").cards, 0);
});

test("the state filter keeps the nodes with free cards, sleepers, held cards, or the offline ones", () => {
  const only = (state: "free" | "sleepers" | "held" | "offline") => names(arrange(model(), { ...NO_FILTERS, state }, "node", "name"))[0].map((n) => n.split(":")[0]);
  assert.deepEqual(only("free"), ["gpu-c"]);
  assert.deepEqual(only("sleepers"), ["gpu-a", "gpu-c"]);
  assert.deepEqual(only("held"), ["gpu-c"]);
  assert.deepEqual(only("offline"), ["gpu-b"]);
});

test("a search names the deployments, counts the cards they occupy, and matches by part of the name in any case", () => {
  const arranged = arrange(model(), { ...NO_FILTERS, search: "NIGH" }, "node", "name");
  assert.deepEqual([...arranged.matched], [2]);
  assert.equal(arranged.highlighted, 2);
  const none = arrange(model(), { ...NO_FILTERS, search: "nothing" }, "node", "name");
  assert.equal(none.matched.size, 0);
  assert.equal(none.highlighted, 0);
  assert.equal(arrange(model(), NO_FILTERS, "node", "name").matched.size, 0);
});

test("grouping by model puts a node's cards in the section of their model, largest section first", () => {
  const mixed = buildModel(overviewOf(
    [{ uuid: "m", name: "mixed", online: true, cards: [card(0, 0), card(1, 1), card(2, 0)] }, { uuid: "o", name: "other", online: true, cards: [card(0, 1), card(1, 1)] }],
    ["L20", "H100"],
  )).model;
  const arranged = arrange(mixed, NO_FILTERS, "model", "name");
  assert.deepEqual(arranged.sections.map((s) => [s.label, s.cards]), [["H100", 3], ["L20", 2]]);
  assert.deepEqual(names(arranged), [["mixed:1", "other:01"], ["mixed:02"]]);
  assert.equal(arranged.nodes, 2);
  assert.equal(arranged.cards, 5);
});

test("grouping by a label key makes a section per value by name with the unlabelled last", () => {
  const arranged = arrange(model(), NO_FILTERS, "label:zone", "name");
  assert.deepEqual(arranged.sections.map((s) => s.label), ["a", "c", ""]);
  assert.deepEqual(names(arranged), [["gpu-a:01"], ["gpu-c:012"], ["gpu-b:0"]]);
  assert.deepEqual(arranged.sections.map((s) => s.free), [0, 1, 0]);
});

test("nodes sort by name as people count, by free capacity, or by contention", () => {
  const nodes = ["gpu-10", "gpu-2", "gpu-1"].map((name, i) => ({ uuid: name, name, online: true, contention: [1, 9, 3][i], cards: [card(0, 0, [10, 90, 50][i])] }));
  const m = buildModel(overviewOf(nodes, ["L20"])).model;
  const order = (sort: "name" | "free" | "contention") => arrange(m, NO_FILTERS, "node", sort).sections[0].nodes.map((n) => n.node.name);
  assert.deepEqual(order("name"), ["gpu-1", "gpu-2", "gpu-10"]);
  assert.deepEqual(order("free"), ["gpu-10", "gpu-1", "gpu-2"]);
  assert.deepEqual(order("contention"), ["gpu-2", "gpu-1", "gpu-10"]);
  assert.ok(compareNames("gpu-2", "gpu-10") < 0);
});

test("free capacity counts only cards that can take work, by their unclaimed share", () => {
  const m = model();
  assert.ok(Math.abs(freeMiB(m.nodes[0].cards) - 49152 * 0.71) < 1e-6, "a fully claimed card gives nothing, a 25% + 4% one the other 71%");
  assert.equal(freeMiB(m.nodes[1].cards), 0, "an offline node gives nothing");
  assert.ok(Math.abs(freeMiB(m.nodes[2].cards) - (49152 * 0.95 + 49152)) < 1e-6, "a held card gives nothing; a sleeper's card gives what it does not keep");
});

test("the states of an arrangement are counted over what is shown", () => {
  assert.deepEqual(countStates(arrange(model(), NO_FILTERS, "node", "name")), { awake: 2, asleep: 1, free: 1, held_outside: 1, unhealthy: 0, offline: 1 });
  assert.deepEqual(countStates(arrange(model(), { ...NO_FILTERS, models: ["H100"] }, "node", "name")), { awake: 0, asleep: 1, free: 1, held_outside: 1, unhealthy: 0, offline: 0 });
});

test("a large fleet decodes and arranges quickly", () => {
  const overview = bigFleet(1000, 8);
  const took = cpuClock();
  const decoded = buildModel(overview);
  const arranged = arrange(decoded.model, { ...NO_FILTERS, search: "beta" }, "label:zone", "contention");
  const decodeCPU = took();
  assert.equal(arranged.cards, 8000);
  assert.ok(decodeCPU < 1500, `decoding and arranging 8000 cards took ${Math.round(decodeCPU)} ms of CPU`);
  const again = cpuClock();
  const next = buildModel(bigFleet(1000, 8), decoded.seen);
  assert.ok(next.model.nodes.every((n, i) => n === decoded.model.nodes[i]));
  const rebuildCPU = again();
  assert.ok(rebuildCPU < 1500, `rebuilding with the seen identities took ${Math.round(rebuildCPU)} ms of CPU`);
});

test("colours come from the scheduling palette: one per state, and an awake card deepens with its claim", () => {
  assert.equal(stateFill.awake, schedulingColors.serving);
  assert.equal(stateFill.asleep, schedulingColors.sleeping);
  assert.equal(stateFill.held_outside, schedulingColors.external);
  assert.equal(new Set(Object.values(stateFill)).size, 6, "six states, six colours");
  assert.equal(mix("#000000", "#ffffff", 0.5), "#808080");
  assert.equal(mix("#102030", "#102030", 0.9), "#102030");
  assert.equal(awakeFill(100), schedulingColors.serving);
  const lightness = (hex: string) => parseInt(hex.slice(1, 3), 16) + parseInt(hex.slice(3, 5), 16) + parseInt(hex.slice(5, 7), 16);
  const shades = [1, 25, 50, 75, 100].map(awakeFill).map(lightness);
  assert.deepEqual([...shades].sort((a, b) => b - a), shades, "a deeper claim is a darker blue");
  assert.notEqual(awakeFill(1), colors.white, "even a 1% claim shows");
  assert.equal(fillOf({ state: "awake", awakePct: 50 }), awakeFill(50));
  assert.equal(fillOf({ state: "free", awakePct: 0 }), stateFill.free);
});
