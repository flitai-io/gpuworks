import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import type { BoardGPU, BoardOccupant } from "../src/schedulingApi.ts";

const { i18n } = await createI18n(["scheduling"]);
const { render, cleanup, within } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { default: GpuResidencyTable } = await import("../src/pages/scheduling/GpuResidencyTable.tsx");
afterEach(cleanup);

const gib = 1024 ** 3;
function card(overrides: Partial<BoardGPU> = {}): BoardGPU {
  return { index: 0, name: "L20", memory_bytes: 48 * gib, awake_bytes: 0, sleeping_bytes: 0,
    free_bytes: 48 * gib, state: "free", held_outside: false, switching: false, occupants: [], ...overrides };
}
function mount(gpus: BoardGPU[], online = true) {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter>
    <GpuResidencyTable node={{ name: "gpu-host", uuid: "one", gpus, online }}/>
  </MemoryRouter></I18nextProvider>);
}

test("every GPU keeps its meter while only reconciled readings show the remainder as free", () => {
  const { container } = mount([
    card({ switching: true }),
    card({ index: 1, held_outside: true, switching: true }),
    card({ index: 2, state: "offline" }),
    card({ index: 3, free_bytes: undefined }),
    card({ index: 4, free_bytes: -1 }),
    card({ index: 5, memory_bytes: 0, free_bytes: 0 }),
    card({ index: 6, awake_bytes: 48 * gib, free_bytes: 0 }),
    card({ index: 7, awake_bytes: 12 * gib, free_bytes: 10 * gib }),
    card({ index: 8, awake_bytes: 49 * gib, free_bytes: -1 }),
  ]);
  const rows = [...container.querySelectorAll("tbody tr")];
  assert.equal(container.querySelectorAll(".residency-meter").length, rows.length);
  assert.deepEqual(rows.flatMap((row, index) => row.querySelector('[data-balanced="true"]') ? [index] : []), [0, 6]);
  assert.deepEqual(rows.flatMap((row, index) => row.querySelector('.residency-meter[data-switching="true"]') ? [index] : []), [0, 1]);
  for (const index of [1, 2, 3, 4, 5, 8]) {
    assert.match(rows[index].querySelector(".free-tag")!.textContent!, /unknown/i);
    assert.equal(rows[index].querySelector(".residency-meter i"), null);
  }
  assert.match(rows[6].querySelector(".free-tag")!.textContent!, /0 B/);
});

test("recorded allocations survive node disconnect without suggesting that live capacity is known", () => {
  const { container } = mount([card({ awake_bytes: 12 * gib, free_bytes: 36 * gib, switching: true })], false);
  assert.match(container.querySelector("tbody")!.textContent!, /12.0 GiB/);
  assert.match(container.querySelector(".free-tag")!.textContent!, /unknown/i);
  const meter = container.querySelector('.residency-meter[data-switching="true"][data-balanced="false"]');
  assert.equal(meter?.querySelector("i")?.style.width, "25%");
  assert.match(container.querySelector("tbody .sr-only")!.textContent!, /switching.*recorded/i);
});

test("two instances of a deployment share one allocation tag while distinct sleep levels remain visible", () => {
  const resident: BoardOccupant = { deployment_id: 12, deployment: "asr", yield_policy: "idle", instance: "one",
    state: "awake", claim_bytes: 10 * gib };
  const { container } = mount([card({ awake_bytes: 20 * gib, sleeping_bytes: 2 * gib, free_bytes: 26 * gib,
    occupants: [resident, { ...resident, instance: "two" },
      { ...resident, instance: "three", state: "asleep", sleep_level: 1, residue_bytes: 2 * gib },
      { ...resident, instance: "four", state: "asleep", sleep_level: 2 }] })]);
  const tags = [...container.querySelectorAll<HTMLElement>(".resident-tag")];
  assert.equal(tags.length, 3);
  for (const tag of tags) assert.equal(within(tag).getByRole("link", { name: "asr" }).getAttribute("href"), "/deployments?deployment=12");
  assert.match(tags[0].textContent!, /20.0 GiB/);
  assert.match(tags[1].textContent!, /L1.*2.0 GiB/);
  assert.match(tags[2].textContent!, /L2.*0 B/);
  assert.match(container.querySelector(".free-tag")!.textContent!, /26.0 GiB/);
});
