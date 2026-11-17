import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// The playground's request priority: the class chosen in the parameters, the
// header the reply asks with, and the class the gateway says it ran at.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["playground", "common"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ParametersPanel, initialParameterState } = await import("../src/pages/playground/ParametersPanel.tsx");
const { default: MessageBubble } = await import("../src/pages/playground/MessageBubble.tsx");
const { streamChat, PRIORITY_CLASS_HEADER } = await import("../src/pages/playground/api.ts");
type PriorityClasses = import("../src/api.ts").PriorityClasses;
type ParameterState = import("../src/pages/playground/types.ts").ParameterState;
type PriorityChoice = import("../src/pages/playground/ParametersPanel.tsx").PriorityChoice;

const p = locale.playground;
const originalFetch = globalThis.fetch;
afterEach(() => { cleanup(); globalThis.fetch = originalFetch; });
after(() => closeChannels());

const classes: PriorityClasses = {
  min_value: -100, max_value: 100, policies: [], default_policy: "", ceiling_policies: [],
  items: [
    { name: "critical", display_name: "", description: "", value: 40, builtin: true, default: false, deletable: false, created_at: "" },
    { name: "normal", display_name: "", description: "", value: 0, builtin: true, default: true, deletable: false, created_at: "" },
    { name: "gold", display_name: "Gold", description: "", value: 30, builtin: false, default: false, deletable: true, created_at: "" },
  ],
};

function Panel({ priority, seen }: { priority?: PriorityChoice; seen: ParameterState[] }) {
  const [value, setValue] = useState(initialParameterState([]));
  return <ParametersPanel definitions={[]} presets={[]} value={value} priority={priority} onChange={(next) => { seen.push(next); setValue(next); }} />;
}
const wrap = (node: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);

test("the parameters offer the server's classes, the default first, and keep the choice", async () => {
  const seen: ParameterState[] = [];
  wrap(<Panel priority={{ classes, scheduling: true }} seen={seen} />);
  const user = userEvent.setup({ document: dom.window.document });
  const trigger = screen.getByRole("combobox", { name: p.parameters.priority });
  assert.equal(trigger.textContent, p.parameters.priorityDefault.replace("{{name}}", "normal"));
  assert.equal(screen.queryByText(p.parameters.priorityNotScheduled), null);
  trigger.focus();
  await user.keyboard("{Enter}");
  const offered = (await screen.findAllByRole("option")).map((node) => node.textContent);
  assert.deepEqual(offered, [p.parameters.priorityDefault.replace("{{name}}", "normal"), "critical (40)", "normal (0)", "Gold (30)"]);
  await user.click(screen.getByRole("option", { name: "Gold (30)" }));
  await waitFor(() => assert.equal(seen.at(-1)?.priorityClass, "gold"));
  // Back to the default asks for no class at all.
  await waitFor(() => assert.ok(!screen.queryByRole("listbox")));
  const reopened = screen.getByRole("combobox", { name: p.parameters.priority });
  reopened.focus();
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(reopened.getAttribute("aria-expanded"), "true"));
  await user.click(await screen.findByRole("option", { name: p.parameters.priorityDefault.replace("{{name}}", "normal") }));
  await waitFor(() => assert.equal(seen.at(-1)?.priorityClass, ""));
});

test("a deployment without priority scheduling says the class is only recorded; a model taking none shows no choice", () => {
  wrap(<Panel priority={{ classes, scheduling: false }} seen={[]} />);
  screen.getByText(p.parameters.priorityNotScheduled);
  cleanup();
  wrap(<Panel seen={[]} />);
  assert.equal(screen.queryByRole("combobox", { name: p.parameters.priority }), null);
});

test("while the classes load the choice waits", () => {
  wrap(<Panel priority={{ classes: null, scheduling: true }} seen={[]} />);
  assert.equal((screen.getByRole("combobox", { name: p.parameters.priority }) as HTMLButtonElement).disabled, true);
});

test("a reply asks for the chosen class and reports the class the gateway answers, refused or not", async () => {
  const asked: Array<string | null> = [];
  let status = 200;
  globalThis.fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    asked.push(new Headers(init?.headers).get(PRIORITY_CLASS_HEADER));
    const headers = { "Content-Type": "text/event-stream", [PRIORITY_CLASS_HEADER]: "gold" };
    return status === 200
      ? new Response('data: {"choices":[{"delta":{"content":"hi"}}]}\n\ndata: [DONE]\n\n', { status, headers })
      : new Response('{"error":{"message":"asleep"}}', { status, headers });
  }) as typeof fetch;
  const ranAt: string[] = [];
  const callbacks = { onContent() {}, onReasoning() {}, onUsage() {}, onDelta() {}, onPriorityClass: (name: string) => ranAt.push(name) };
  const parameters = { ...initialParameterState([]), priorityClass: "gold" };
  await streamChat("m", [{ role: "user", content: "x" }], parameters, [], callbacks, new AbortController().signal);
  await streamChat("m", [{ role: "user", content: "x" }], initialParameterState([]), [], callbacks, new AbortController().signal);
  status = 503;
  await assert.rejects(streamChat("m", [{ role: "user", content: "x" }], parameters, [], callbacks, new AbortController().signal));
  assert.deepEqual(asked, ["gold", null, "gold"]);
  assert.deepEqual(ranAt, ["gold", "gold", "gold"]);
});

test("a reply names the class it ran at beside its metrics", () => {
  wrap(<MessageBubble message={{ id: "a", role: "assistant", content: "hi", model: "m", priorityClass: "gold", metrics: { durationMS: 10, ttftMS: 5 } }} />);
  screen.getByText(p.metrics.priorityLabel);
  screen.getByText("gold");
  cleanup();
  wrap(<MessageBubble message={{ id: "b", role: "assistant", content: "hi", model: "m", metrics: { durationMS: 10 } }} />);
  assert.equal(screen.queryByText(p.metrics.priorityLabel), null);
});

test("a saved class that no longer exists asks for the default; one the page could not check is kept", async () => {
  const { knownClass } = await import("../src/pages/playground/priorityClasses.ts");
  assert.equal(knownClass("gold", { classes, failed: false }), "gold");
  assert.equal(knownClass("platinum", { classes, failed: false }), "");
  assert.equal(knownClass("platinum", { classes: null, failed: false }), "platinum");
  assert.equal(knownClass("platinum", { classes: { ...classes, items: [] }, failed: true }), "platinum");
  assert.equal(knownClass(undefined, { classes, failed: false }), undefined);
});

test("classes that cannot be read still leave the default to choose", async () => {
  const { api } = await import("../src/api.ts");
  const { usePlaygroundPriorityClasses } = await import("../src/pages/playground/priorityClasses.ts");
  const original = api.priorityClasses;
  api.priorityClasses = async () => { throw new Error("server down"); };
  try {
    function Probe() {
      const loaded = usePlaygroundPriorityClasses();
      return <ParametersPanel definitions={[]} presets={[]} value={initialParameterState([])} onChange={() => {}} priority={{ classes: loaded.classes, scheduling: true }} />;
    }
    wrap(<Probe />);
    const trigger = screen.getByRole("combobox", { name: p.parameters.priority });
    await waitFor(() => assert.equal((trigger as HTMLButtonElement).disabled, false));
    assert.equal(trigger.textContent, p.parameters.priorityDefaultUnnamed);
  } finally {
    api.priorityClasses = original;
  }
});
