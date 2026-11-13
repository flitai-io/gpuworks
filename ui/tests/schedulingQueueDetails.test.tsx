import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

Object.defineProperty(globalThis, "Document", { value: dom.window.Document, configurable: true });
const closeChannels = trackMessageChannels();
const { i18n } = await createI18n(["scheduling", "deploy", "deployments"], "zh");
const { cleanup, fireEvent, render, screen, within } = await import("@testing-library/react");
const { MemoryRouter } = await import("react-router-dom");
const { PendingCard, WaitingWakesCard } = await import("../src/pages/scheduling/BoardTables.tsx");
afterEach(cleanup);
after(closeChannels);

test("four waiting deployments fit in the preview and the fifth can be reached through the total", async () => {
  const rows = Array.from({ length: 5 }, (_, i) => ({
    deployment_id: i + 1, deployment: "wake-" + (i + 1),
    instances: ["instance-" + i], since: new Date().toISOString(),
  }));
  const content = (count: number) => <I18nextProvider i18n={i18n}><MemoryRouter>
    <WaitingWakesCard rows={rows.slice(0, count)} heldRequests={18} busyID={null} onCancel={() => {}} />
  </MemoryRouter></I18nextProvider>;
  const rendered = render(content(4));
  const card = within(screen.getByRole("region"));
  assert.deepEqual(card.getAllByRole("link").map(link => link.textContent), ["wake-1", "wake-2", "wake-3", "wake-4"]);
  rendered.rerender(content(5));
  assert.equal(card.getAllByRole("link").length, 4);
  fireEvent.click(card.getByRole("button", { name: "查看全部 5 条" }));
  const detail = within(await screen.findByRole("dialog"));
  assert.equal(detail.getAllByRole("link").length, 5);
  assert.ok(detail.getByRole("link", { name: "wake-5" }));
});

test("placement preview caps distinct reasons and keeps the remaining deployments accessible", async () => {
  const reasons = ["a", "b", "c", "d", "e", "e"];
  const content = (count: number) => <I18nextProvider i18n={i18n}><MemoryRouter>
    <PendingCard rows={reasons.slice(0, count).map((code, i) => ({
      deployment_id: i + 1, deployment: "deployment-" + i, at: "2026-11-09T08:00:00Z",
      reason: { code: "preview_" + code, message: "模拟原因 " + code },
    }))} />
  </MemoryRouter></I18nextProvider>;
  const rendered = render(content(4));
  const card = within(screen.getByRole("region"));
  assert.equal(card.getAllByRole("button", { name: /模拟原因/ }).length, 4);
  assert.equal(card.queryByRole("button", { name: /查看其余/ }), null);
  rendered.rerender(content(6));
  const visible = card.getAllByRole("button", { name: /模拟原因/ });
  assert.equal(visible.length, 4);
  assert.match(visible[0].textContent ?? "", /模拟原因 e.*2 个部署/);
  fireEvent.click(card.getByRole("button", { name: "查看其余 1 种原因" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /模拟原因 d/ }));
  const details = within(await screen.findByRole("dialog"));
  assert.deepEqual(details.getAllByRole("link").map(link => link.textContent), ["deployment-3"]);
});

test("pending search reaches every page and preserves the reason drilldown; time and name sort independently", async () => {
  const rows = Array.from({ length: 22 }, (_, index) => ({
    deployment_id: 100 + index,
    deployment: index === 21 ? "Zebra" : `app-${index}`,
    at: new Date(Date.UTC(2026, 10, 9, index)).toISOString(),
    reason: { code: index ? "disk" : "gpu", message: index ? "Disk full" : "GPU busy" },
  }));
  render(<I18nextProvider i18n={i18n}><MemoryRouter><PendingCard rows={rows} /></MemoryRouter></I18nextProvider>);
  const card = within(screen.getByRole("region"));
  fireEvent.click(card.getByRole("button", { name: /Disk full/ }));
  const detail = within(await screen.findByRole("dialog"));
  const search = () => detail.getByRole("searchbox");
  const names = () => detail.queryAllByRole("link").map(link => link.textContent);
  const type = (value: string) => fireEvent.change(search(), { target: { value } });
  assert.equal(names()[0], "Zebra");
  fireEvent.click(detail.getByRole("button", { name: "下一页" }));
  assert.deepEqual(names(), ["app-1"]);
  type("  zEbRa ");
  assert.deepEqual(names(), ["Zebra"]);
  type("#100");
  assert.ok(detail.getByText("没有匹配的部署"));
  fireEvent.click(detail.getByRole("button", { name: "查看全部 22 条" }));
  assert.equal((search() as HTMLInputElement).value, "");
  type("#100");
  assert.deepEqual(names(), ["app-0"]);
  type("GPU busy");
  assert.deepEqual(names(), ["app-0"]);
  type("");
  async function sort(column: string, direction: string) {
    fireEvent.click(within(detail.getByRole("columnheader", { name: new RegExp(column) })).getByRole("button"));
    fireEvent.click(await screen.findByRole("menuitemradio", { name: direction }));
  }
  await sort("部署", i18n.t("common:resourceTable.sortAscending"));
  assert.equal(names()[0], "app-0");
  await sort("记录时间", i18n.t("common:resourceTable.sortAscending"));
  assert.equal(names()[0], "app-0");
  await sort("记录时间", i18n.t("common:resourceTable.sortDescending"));
  assert.equal(names()[0], "Zebra");
});
