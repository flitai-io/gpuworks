import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { batch, model } from "./helpers/batchFixtures.ts";

// The batch building blocks mounted on their own: the status tag, the progress
// bar and its words, the read-only facts, the file's models and the model field.

const { i18n, locale } = await createI18n(["batches"]);
const { i18n: zh, locale: zhLocale } = await createI18n(["batches"], "zh");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { BatchStatusTag, BatchProgress, batchProgress } = await import("../src/pages/batches/batchDisplay.tsx");
const { BatchFacts } = await import("../src/pages/batches/BatchFacts.tsx");
const { default: BatchFileModels } = await import("../src/pages/batches/BatchFileModels.tsx");
const { default: BatchModelField } = await import("../src/pages/batches/BatchModelField.tsx");
afterEach(cleanup);
const b = locale.batches;
const mount = (children: React.ReactNode, instance = i18n) => render(<I18nextProvider i18n={instance}>{children}</I18nextProvider>);

const STATUS_BADGES: Record<string, string> = {
  validating: "neutral", in_progress: "info", finalizing: "info", cancelling: "warning",
  completed: "success", failed: "danger", expired: "warning", cancelled: "neutral",
};

test("each batch status is worded from the locale and coloured by how it ended or that it moves", () => {
  for (const [instance, words] of [[i18n, b.status], [zh, zhLocale.batches.status]] as const) {
    const view = mount(<>{Object.keys(STATUS_BADGES).map((status) => <BatchStatusTag key={status} status={status} />)}</>, instance);
    const shown = Object.keys(STATUS_BADGES).map((status) => {
      const tag = screen.getByText(words[status]).closest("[data-slot='badge']") as HTMLElement;
      return [status, tag.dataset.status];
    });
    assert.deepEqual(Object.fromEntries(shown), STATUS_BADGES);
    view.unmount();
  }
});

test("a status the console does not know is shown as the server sent it, in neutral", () => {
  mount(<BatchStatusTag status="paused_by_quota" />);
  assert.equal((screen.getByText("paused_by_quota").closest("[data-slot='badge']") as HTMLElement).dataset.status, "neutral");
});

test("progress counts finished requests, names the failed ones and splits the bar into succeeded and failed", () => {
  mount(<BatchProgress batch={batch({ request_counts: { total: 10, completed: 4, failed: 1 } })} />);
  const bar = screen.getByRole("progressbar", { name: "5 of 10 requests finished, 1 failed" });
  assert.equal(bar.getAttribute("aria-valuenow"), "50");
  assert.deepEqual([...bar.children].map((part) => (part as HTMLElement).style.width), ["40%", "10%"]);
  assert.ok(screen.getByText("5 / 10 done"));
  assert.ok(screen.getByText("1 failed"));
});

test("progress without failures says nothing about failures", () => {
  mount(<BatchProgress batch={batch({ request_counts: { total: 4, completed: 4, failed: 0 } })} />);
  assert.equal(screen.getByRole("progressbar").getAttribute("aria-valuenow"), "100");
  assert.ok(screen.getByText("4 / 4 done"));
  assert.equal(screen.queryByText(/failed$/), null);
});

test("a batch with no requests yet reads 0 / 0 at an empty bar, not NaN", () => {
  mount(<BatchProgress batch={batch({ status: "validating", request_counts: { total: 0, completed: 0, failed: 0 } })} />);
  const bar = screen.getByRole("progressbar", { name: "0 of 0 requests finished, 0 failed" });
  assert.equal(bar.getAttribute("aria-valuenow"), "0");
  assert.deepEqual([...bar.children].map((part) => (part as HTMLElement).style.width), ["0%", "0%"]);
  assert.ok(screen.getByText("0 / 0 done"));
});

test("counts above the total (a late recount) cap the bar at 100%", () => {
  assert.deepEqual(batchProgress({ request_counts: { total: 2, completed: 3, failed: 1 } }), { total: 2, completed: 3, failed: 1, done: 4, percent: 100, succeeded: 100 });
  mount(<BatchProgress batch={batch({ request_counts: { total: 2, completed: 3, failed: 1 } })} />);
  const bar = screen.getByRole("progressbar");
  assert.equal(bar.getAttribute("aria-valuenow"), "100");
  assert.deepEqual([...bar.children].map((part) => (part as HTMLElement).style.width), ["100%", "0%"]);
});

test("batch facts list each label beside its value, under an optional heading and action", () => {
  const view = mount(<BatchFacts title="Requests" action={<button>Copy</button>} items={[{ key: "id", label: "ID", value: "batch_1" }, { key: "endpoint", label: "Endpoint", value: "/v1/embeddings" }]} />);
  assert.ok(screen.getByRole("heading", { name: "Requests" }));
  assert.ok(screen.getByRole("button", { name: "Copy" }));
  const pairs = [...document.querySelectorAll("dt")].map((term) => [term.textContent, term.nextElementSibling?.textContent]);
  assert.deepEqual(pairs, [["ID", "batch_1"], ["Endpoint", "/v1/embeddings"]]);
  view.unmount();
  mount(<BatchFacts items={[{ key: "id", label: "ID", value: "batch_2" }]} />);
  assert.equal(screen.queryByRole("heading"), null, "no heading row without a title or action");
});

const fileModels = (props: Partial<React.ComponentProps<typeof BatchFileModels>>) =>
  mount(<BatchFileModels fileModels={null} skipped={false} known={null} override={undefined} {...props} />);

test("the file's models: unread says nothing, an oversized file says it was not checked, an empty one says no model", () => {
  const unread = fileModels({});
  assert.equal(unread.container.textContent, "");
  unread.unmount();
  const skipped = fileModels({ skipped: true, fileModels: ["a"] });
  assert.equal(skipped.container.textContent, b.create.fileModels.skipped);
  skipped.unmount();
  fileModels({ fileModels: [] });
  assert.ok(screen.getByText(b.create.fileModels.none));
  assert.equal(screen.queryByRole("status"), null);
});

test("without a chosen model the file's unserved models are a warning, but only once the server's list is known", () => {
  const before = fileModels({ fileModels: ["qwen", "ghost"] });
  assert.ok(screen.getByText("Models in the file (2): qwen, ghost"));
  assert.equal(screen.queryByRole("status"), null, "no warning before the dictionary is read");
  before.unmount();
  fileModels({ fileModels: ["qwen", "ghost"], known: new Set(["qwen"]) });
  assert.match(screen.getByRole("status").textContent ?? "", /have no available deployment on the platform, so the batch cannot be created: ghost\./);
});

test("with a chosen model only the other models are said to be replaced, as information", () => {
  const replaced = fileModels({ fileModels: ["qwen", "ghost"], known: new Set(), override: "qwen" });
  assert.equal(screen.getByRole("status").textContent, "The models in the file (ghost) will all be replaced by qwen.");
  replaced.unmount();
  fileModels({ fileModels: ["qwen"], known: new Set(), override: "qwen" });
  assert.equal(screen.queryByRole("status"), null, "nothing is replaced when the file already names the chosen model");
});

function Field({ options, initial, disabled = false, changes, toggles }: { options: ReturnType<typeof model>[]; initial?: string; disabled?: boolean; changes: (string | undefined)[]; toggles: boolean[] }) {
  const [value, setValue] = useState(initial);
  const [showWaiting, setShowWaiting] = useState(false);
  return <BatchModelField options={options} value={value} disabled={disabled} showWaiting={showWaiting}
    onChange={(next) => { changes.push(next); setValue(next); }} onShowWaitingChange={(next) => { toggles.push(next); setShowWaiting(next); }} />;
}

test("the model field offers each model with its state, warns about a waiting one and clears back to the file's own", async () => {
  const changes: (string | undefined)[] = [];
  const toggles: boolean[] = [];
  mount(<Field options={[model("qwen", "running"), model("parked", "stopped"), model("odd", "migrating")]} initial="parked" changes={changes} toggles={toggles} />);
  assert.ok(screen.getByText(b.create.model.warning.stopped.replace("{{model}}", "parked")));
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name: b.create.model.label }));
  const listbox = await screen.findByRole("listbox");
  assert.deepEqual(within(listbox).getAllByRole("option").map((option) => option.textContent),
    ["qwen · running", "parked · stopped", "odd · migrating"], "a state without words is shown as the server sent it");
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("listbox"), null));
  await user.click(screen.getByRole("button", { name: "Remove" }));
  assert.deepEqual(changes, [undefined], "clearing sends no model, not an empty name");
  assert.equal(screen.queryByRole("button", { name: "Remove" }), null);
  assert.equal(screen.queryByRole("status"), null, "the warning leaves with the choice");
  const toggle = screen.getByRole("checkbox", { name: b.create.model.showWaiting });
  toggle.focus();
  await user.keyboard(" ");
  await user.click(toggle);
  assert.deepEqual(toggles, [true, false]);
});

test("a running model raises no warning and a disabled field locks the select, remove and toggle", () => {
  mount(<Field options={[model("qwen", "running")]} initial="qwen" disabled changes={[]} toggles={[]} />);
  assert.equal(screen.queryByRole("status"), null);
  assert.equal((screen.getByRole("button", { name: "Remove" }) as HTMLButtonElement).disabled, true);
  const combobox = screen.getByRole("combobox", { name: b.create.model.label });
  assert.ok(combobox.hasAttribute("disabled") || combobox.getAttribute("aria-disabled") === "true" || combobox.hasAttribute("data-disabled"), "the select is disabled");
  const toggle = screen.getByRole("checkbox", { name: b.create.model.showWaiting });
  assert.ok(toggle.hasAttribute("disabled") || toggle.getAttribute("aria-disabled") === "true", "the toggle is disabled");
});
