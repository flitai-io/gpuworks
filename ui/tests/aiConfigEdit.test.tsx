import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["settings", "common"], "en");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const AICard = (await import("../src/pages/settings/AIConfigCard.tsx")).default;

const ai = locale.settings.ai as Record<string, any>;
const common = locale.settings.common as Record<string, any>;

/** stored is one service as the API lists it, with no key. */
function stored(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    name: "primary",
    preset: "deepseek",
    base_url: "https://api.deepseek.com/v1",
    model: "deepseek-v4-flash",
    is_default: true,
    proxy_mode: "management" as const,
    image_input: false,
    ...overrides,
  };
}

/** DICTIONARY is GET /api/v1/ai/presets as the server answers it. */
const DICTIONARY = {
  presets: [
    { name: "deepseek", base_url: "https://api.deepseek.com", model: "deepseek-flash", image_input: true },
    { name: "bailian", base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1", model: "qwen-plus", image_input: false },
    { name: "siliconflow", base_url: "https://api.siliconflow.cn/v1", model: "deepseek-ai/DeepSeek-V3", image_input: false },
    { name: "openrouter", base_url: "https://openrouter.ai/api/v1", model: "deepseek/deepseek-chat", image_input: false },
    { name: "poe", base_url: "https://api.poe.com/v1", model: "GPT-5", image_input: true },
    { name: "custom", base_url: "", model: "", image_input: false },
  ],
  proxy_modes: ["management", "direct", "custom"] as ("management" | "direct" | "custom")[],
};

/** stubAPI replaces the AI calls and records what the card sent. */
function stubAPI(rows: ReturnType<typeof stored>[]) {
  const original = {
    aiConfigs: api.aiConfigs,
    updateAIConfig: api.updateAIConfig,
    createAIConfig: api.createAIConfig,
    deleteAIConfig: api.deleteAIConfig,
    testAIConfig: api.testAIConfig,
    aiPresets: api.aiPresets,
  };
  const calls: {
    updated: { id: number; body: Record<string, unknown> }[];
    created: Record<string, unknown>[];
    tested: Record<string, unknown>[];
    deleted: number[];
  } = { updated: [], created: [], tested: [], deleted: [] };
  let current = rows;
  api.aiConfigs = async () => list(current);
  api.aiPresets = async () => DICTIONARY;
  api.updateAIConfig = async (id, body) => {
    calls.updated.push({ id, body: body as Record<string, unknown> });
    current = current.map((row) => (row.id === id ? ({ ...row, ...body } as ReturnType<typeof stored>) : row));
    return current.find((row) => row.id === id)!;
  };
  api.createAIConfig = async (body) => {
    calls.created.push(body as unknown as Record<string, unknown>);
    return stored(body as unknown as Record<string, unknown>);
  };
  api.deleteAIConfig = async (id) => {
    calls.deleted.push(id);
    // The server promotes the newest survivor when the deleted service was the
    // one in use, and names it in the answer; the stub does the same so the card
    // is exercised against the reply it really gets.
    const wasDefault = current.some((row) => row.id === id && row.is_default);
    current = current.filter((row) => row.id !== id);
    if (!wasDefault || current.length === 0) return { status: "deleted" };
    const promoted = [...current].sort((a, b) => b.id - a.id)[0];
    current = current.map((row) => (row.id === promoted.id ? { ...row, is_default: true } : row));
    return { status: "deleted", promoted_id: promoted.id };
  };
  api.testAIConfig = async (body) => {
    calls.tested.push(body as unknown as Record<string, unknown>);
    return { outcome: "ok", base_url: (body as { base_url: string }).base_url, latency_ms: 12 };
  };
  return {
    calls,
    restore: () => Object.assign(api, original),
  };
}

const fakes: { restore(): void }[] = [];
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
});

function renderCard() {
  return render(
    <I18nextProvider i18n={i18n}>
      <AICard />
    </I18nextProvider>,
  );
}

test("the list shows each service's address beside its name, preset and model", async () => {
  fakes.push(stubAPI([stored()]));
  renderCard();
  // The address the assistant will call is part of the row, so an operator can
  // tell two services apart without opening them.
  assert.ok(await screen.findByText("https://api.deepseek.com/v1"));
  assert.ok(screen.getByText("primary"));
  assert.ok(screen.getByText(ai.model));
  assert.ok(screen.getByText(ai.presetName.deepseek));
});

test("editing a service prefills it and changes only what was edited", async () => {
  const fake = stubAPI([stored()]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getByRole("button", { name: common.edit }));
  const dialog = await screen.findByRole("dialog");
  // Prefilled from the stored service: the dialog opens on its values, so an
  // operator changes one field instead of retyping the service.
  const name = within(dialog).getByLabelText(ai.name) as HTMLInputElement;
  const address = within(dialog).getByLabelText(ai.baseUrl) as HTMLInputElement;
  const model = within(dialog).getByLabelText(ai.model) as HTMLInputElement;
  const key = within(dialog).getByLabelText(ai.upstreamKey) as HTMLInputElement;
  assert.equal(name.value, "primary");
  assert.equal(address.value, "https://api.deepseek.com/v1");
  assert.equal(model.value, "deepseek-v4-flash");
  // The key is never sent to the console, so the field starts blank and says
  // what blank means.
  assert.equal(key.value, "");
  assert.equal(key.getAttribute("placeholder"), ai.keyKeep);
  await user.clear(name);
  await user.type(name, "renamed");
  await user.click(within(dialog).getByRole("button", { name: common.save }));
  await waitFor(() => assert.equal(fake.calls.updated.length, 1));
  const { id, body } = fake.calls.updated[0];
  assert.equal(id, 1);
  assert.equal(body.name, "renamed");
  // A blank key is left out entirely, so the server keeps the stored key.
  assert.ok(!("api_key" in body), `an edit sent a blank key: ${JSON.stringify(body)}`);
  assert.equal(body.base_url, "https://api.deepseek.com/v1");
});

test("a new key typed on an edit is sent, and the row follows it", async () => {
  const fake = stubAPI([stored()]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getByRole("button", { name: common.edit }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(ai.upstreamKey), "sk-replaced");
  await user.click(within(dialog).getByRole("button", { name: common.save }));
  await waitFor(() => assert.equal(fake.calls.updated.length, 1));
  assert.equal(fake.calls.updated[0].body.api_key, "sk-replaced");
});

test("a new service still requires a key", async () => {
  const fake = stubAPI([]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(ai.empty);
  await user.click(screen.getByRole("button", { name: common.add }));
  const dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByLabelText(ai.name), "fresh");
  await user.click(within(dialog).getByRole("button", { name: common.add }));
  assert.ok(await within(dialog).findByText(ai.keyRequired));
  assert.equal(fake.calls.created.length, 0);
  await user.type(within(dialog).getByLabelText(ai.upstreamKey), "sk-fresh");
  await user.click(within(dialog).getByRole("button", { name: common.add }));
  await waitFor(() => assert.equal(fake.calls.created.length, 1));
  // A new service starts from the server's first preset, image switch
  // included, and from the management node's proxy.
  assert.deepEqual(fake.calls.created[0], {
    name: "fresh",
    preset: "deepseek",
    base_url: "https://api.deepseek.com",
    api_key: "sk-fresh",
    model: "deepseek-flash",
    is_default: true,
    image_input: true,
    proxy_mode: "management",
  });
});

// The five answers an operator acts on differently must read differently: a
// wrong address, a wrong key, a model the endpoint does not serve, an address
// the platform refuses to call, and a service that answers.
const OUTCOMES: { outcome: string; message: string }[] = [
  { outcome: "ok", message: "testOk" },
  { outcome: "connection", message: "testConnection" },
  { outcome: "blocked", message: "testBlocked" },
  { outcome: "auth", message: "testAuth" },
  { outcome: "path", message: "testPath" },
  { outcome: "model", message: "testModel" },
  { outcome: "http", message: "testHttp" },
];

test("a connection test words each answer it can get", async () => {
  const fake = stubAPI([stored()]);
  fakes.push(fake);
  const outcomes = OUTCOMES.map((entry) => entry.outcome as never);
  api.testAIConfig = async (body) => {
    fake.calls.tested.push(body as unknown as Record<string, unknown>);
    return {
      outcome: outcomes[fake.calls.tested.length - 1],
      status: 200,
      detail: "the endpoint's own words",
      base_url: (body as { base_url: string }).base_url,
      latency_ms: 7,
    };
  };
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getByRole("button", { name: common.edit }));
  const dialog = await screen.findByRole("dialog");
  const button = within(dialog).getByRole("button", { name: ai.test });
  for (const entry of OUTCOMES) {
    await user.click(button);
    assert.ok(await within(dialog).findByText(ai[entry.message]), `${entry.outcome} showed no result`);
    // One answer is shown at a time: the previous test's result is replaced,
    // not stacked, so an operator never reads a stale verdict.
    for (const other of OUTCOMES.filter((item) => item.outcome !== entry.outcome)) {
      assert.equal(
        within(dialog).queryByText(ai[other.message]),
        null,
        `${entry.outcome} also showed ${other.outcome}`,
      );
    }
  }
  // The endpoint's own words and the address tested are shown next to the
  // code, so an operator can act on the answer without reading server logs.
  assert.ok(within(dialog).getByText(/the endpoint's own words/));
  assert.ok(within(dialog).getByText(new RegExp(ai.testAddress.replace("{{url}}", "https://api.deepseek.com/v1"))));
  // The key never went to the endpoint from here: the edit sent only the id,
  // so the server uses the stored key for its own address.
  assert.deepEqual(fake.calls.tested[0], {
    id: 1,
    base_url: "https://api.deepseek.com/v1",
    api_key: undefined,
    model: "deepseek-v4-flash",
    proxy_mode: "management",
  });
});

test("the add dialog tests the values in the form without saving them", async () => {
  const fake = stubAPI([]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(ai.empty);
  await user.click(screen.getByRole("button", { name: common.add }));
  const dialog = await screen.findByRole("dialog");
  const address = within(dialog).getByLabelText(ai.baseUrl);
  await user.clear(address);
  // A pasted call path is what the bug reported; the test asks about it as
  // typed, and the server normalizes it before storing or calling it.
  await user.type(address, "https://api.deepseek.com/v1/chat/completions");
  await user.type(within(dialog).getByLabelText(ai.upstreamKey), "sk-typed");
  await user.click(within(dialog).getByRole("button", { name: ai.test }));
  await waitFor(() => assert.equal(fake.calls.tested.length, 1));
  // Only the address and the model are needed, so a test does not demand a
  // name first; the key that was typed is the one the test calls with.
  assert.equal(fake.calls.tested[0].base_url, "https://api.deepseek.com/v1/chat/completions");
  assert.equal(fake.calls.tested[0].model, "deepseek-flash");
  assert.equal(fake.calls.tested[0].api_key, "sk-typed");
  assert.equal(fake.calls.created.length, 0);
  assert.ok(await within(dialog).findByText(ai.testOk));
});

test("the form says where the address ends", async () => {
  fakes.push(stubAPI([]));
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(ai.empty);
  await user.click(screen.getByRole("button", { name: common.add }));
  const dialog = await screen.findByRole("dialog");
  // A wrong address is the failure the bug reported; the hint is next to the
  // field, not only in a message after the assistant fails.
  assert.ok(within(dialog).getByText(ai.baseUrlHint));
});

// While any service is stored exactly one of them is the one the assistant uses,
// and the server refuses to clear the mark on that one. The dialog therefore does
// not offer to clear it: a switch that can only produce a rejection is worse than
// no switch, because the operator reads it as a setting they own.
test("the default service's own dialog does not offer to unset it", async () => {
  const fake = stubAPI([stored({ id: 1, name: "primary", is_default: true }), stored({ id: 2, name: "spare", is_default: false })]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  // primary is the service in use, and the table lists it first.
  await user.click(screen.getAllByRole("button", { name: common.edit })[0]);
  const dialog = await screen.findByRole("dialog");
  const toggle = within(dialog).getByRole("switch", { name: ai.setDefault }) as HTMLButtonElement;
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  assert.equal(toggle.disabled, true);
  assert.ok(within(dialog).getByText(ai.defaultLocked));
});

// Moving the assistant to another service stays available: that is the way the
// operator changes which service it uses, and the switch on that row works.
test("a service that is not in use can still be made the default", async () => {
  const fake = stubAPI([stored({ id: 1, name: "primary", is_default: true }), stored({ id: 2, name: "spare", is_default: false })]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("spare");
  await user.click(screen.getAllByRole("button", { name: common.edit })[1]);
  const dialog = await screen.findByRole("dialog");
  const toggle = within(dialog).getByRole("switch", { name: ai.setDefault }) as HTMLButtonElement;
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  assert.equal(toggle.disabled, false);
  assert.equal(within(dialog).queryByText(ai.defaultLocked), null);
  // And the save carries it, so the move happens as the switch says it will.
  await user.click(toggle);
  await user.click(within(dialog).getByRole("button", { name: common.save }));
  await waitFor(() => assert.equal(fake.calls.updated.length, 1));
  assert.equal(fake.calls.updated[0].body.is_default, true);
});

// Deleting the service in use hands the mark to another one; the console says
// which, so the data-egress notice above the table does not silently start
// naming a different host.
test("deleting the service in use says which one the assistant moved to", async () => {
  const fake = stubAPI([stored({ id: 1, name: "primary", is_default: true }), stored({ id: 2, name: "spare", is_default: false })]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getAllByRole("button", { name: common.delete })[0]);
  const buttons = await screen.findAllByRole("button", { name: common.delete });
  await user.click(buttons[buttons.length - 1]);
  await waitFor(() => assert.deepEqual(fake.calls.deleted, [1]));
  // The replacement is named: the row that took over is where the assistant's
  // reads now go, and the operator is told so instead of inferring it.
  assert.ok(await screen.findByText(ai.deletedPromoted.replace("{{name}}", "spare")));
  await waitFor(() => assert.equal(screen.queryByText("primary"), null));
  assert.ok(screen.getByText("spare"));
});

// Deleting a service that is not in use changes nothing about which one the
// assistant uses, so there is nothing to announce beyond the delete.
test("deleting a spare service says only that it was deleted", async () => {
  const fake = stubAPI([stored({ id: 1, name: "primary", is_default: true }), stored({ id: 2, name: "spare", is_default: false })]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("spare");
  await user.click(screen.getAllByRole("button", { name: common.delete })[1]);
  const buttons = await screen.findAllByRole("button", { name: common.delete });
  await user.click(buttons[buttons.length - 1]);
  await waitFor(() => assert.deepEqual(fake.calls.deleted, [2]));
  assert.ok(await screen.findByText(ai.deleted));
  await waitFor(() => assert.equal(screen.queryByText("spare"), null));
  // The service in use is untouched.
  assert.ok(screen.getByText("primary"));
});

/** chooseProxy picks a proxy mode from the dialog's proxy select. */
async function chooseProxy(user: ReturnType<typeof userEvent.setup>, dialog: HTMLElement, label: string) {
  await user.click(within(dialog).getByLabelText(ai.proxy));
  const options = await screen.findAllByText(label);
  await user.click(options[options.length - 1]);
}

async function openAdd(fake: ReturnType<typeof stubAPI>) {
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(ai.empty);
  await user.click(screen.getByRole("button", { name: common.add }));
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByLabelText(ai.name);
  return { user, dialog };
}

// The preset table is the server's: choosing a preset fills its address,
// model and image switch from the dictionary, so the two cannot disagree.
test("choosing a preset fills the server's address, model and image switch", async () => {
  const { user, dialog } = await openAdd(stubAPI([]));
  const image = within(dialog).getByRole("switch", { name: ai.imageInput });
  assert.equal(image.getAttribute("aria-checked"), "true");
  await user.click(within(dialog).getByLabelText(ai.preset));
  await user.click(await screen.findByText(ai.presetName.bailian));
  await waitFor(() =>
    assert.equal((within(dialog).getByLabelText(ai.baseUrl) as HTMLInputElement).value, "https://dashscope.aliyuncs.com/compatible-mode/v1"),
  );
  assert.equal((within(dialog).getByLabelText(ai.model) as HTMLInputElement).value, "qwen-plus");
  assert.equal(image.getAttribute("aria-checked"), "false");
});

test("the dialog waits for the preset dictionary and offers a retry when it fails", async () => {
  const fake = stubAPI([]);
  let attempts = 0;
  api.aiPresets = async () => {
    attempts++;
    if (attempts === 1) throw new Error("gateway timeout");
    return DICTIONARY;
  };
  const { user, dialog } = await (async () => {
    fakes.push(fake);
    renderCard();
    const user = userEvent.setup({ document: dom.window.document });
    await screen.findByText(ai.empty);
    await user.click(screen.getByRole("button", { name: common.add }));
    return { user, dialog: await screen.findByRole("dialog") };
  })();
  assert.ok(await within(dialog).findByText(ai.presetsFailed.replace("{{detail}}", "gateway timeout")));
  assert.equal(screen.queryByRole("status", { name: ai.presetsLoading }), null);
  assert.equal(within(dialog).queryByLabelText(ai.name), null);
  assert.equal((within(dialog).getByRole("button", { name: common.add }) as HTMLButtonElement).disabled, true);
  await user.click(within(dialog).getByRole("button", { name: ai.retry }));
  assert.ok(await within(dialog).findByLabelText(ai.name));
  assert.equal(attempts, 2);
});

test("a custom proxy is asked for, required, and sent with the test and the save", async () => {
  const fake = stubAPI([]);
  const { user, dialog } = await openAdd(fake);
  // Management is the default and says where its proxy comes from.
  assert.ok(within(dialog).getByText(ai.proxyModeHint.management));
  assert.ok(within(dialog).getByText(ai.proxyMode.management));
  assert.equal(within(dialog).queryByLabelText(ai.proxyUrl), null);
  await chooseProxy(user, dialog, ai.proxyMode.custom);
  const proxy = await within(dialog).findByLabelText(ai.proxyUrl);
  await user.type(within(dialog).getByLabelText(ai.name), "via-proxy");
  await user.type(within(dialog).getByLabelText(ai.upstreamKey), "sk");
  await user.click(within(dialog).getByRole("button", { name: common.add }));
  assert.ok(await within(dialog).findByText(ai.proxyUrlRequired));
  assert.equal(fake.calls.created.length, 0);
  await user.type(proxy, "socks5h://u:p@proxy.example:1080");
  await user.click(within(dialog).getByRole("button", { name: ai.test }));
  await waitFor(() => assert.equal(fake.calls.tested.length, 1));
  assert.equal(fake.calls.tested[0].proxy_mode, "custom");
  assert.equal(fake.calls.tested[0].proxy_url, "socks5h://u:p@proxy.example:1080");
  assert.ok(!("check_image" in fake.calls.tested[0]));
  await user.click(within(dialog).getByRole("button", { name: common.add }));
  await waitFor(() => assert.equal(fake.calls.created.length, 1));
  assert.equal(fake.calls.created[0].proxy_mode, "custom");
  assert.equal(fake.calls.created[0].proxy_url, "socks5h://u:p@proxy.example:1080");
});

test("direct sends no proxy URL", async () => {
  const fake = stubAPI([]);
  const { user, dialog } = await openAdd(fake);
  await chooseProxy(user, dialog, ai.proxyMode.direct);
  assert.ok(within(dialog).getByText(ai.proxyModeHint.direct));
  await user.type(within(dialog).getByLabelText(ai.name), "direct");
  await user.type(within(dialog).getByLabelText(ai.upstreamKey), "sk");
  await user.click(within(dialog).getByRole("button", { name: common.add }));
  await waitFor(() => assert.equal(fake.calls.created.length, 1));
  assert.equal(fake.calls.created[0].proxy_mode, "direct");
  assert.ok(!("proxy_url" in fake.calls.created[0]));
});

// The stored proxy is shown masked, so a blank field must keep it: the edit
// leaves proxy_url out, as it leaves out a blank key.
test("editing a service with a custom proxy keeps the stored proxy unless one is typed", async () => {
  const fake = stubAPI([stored({ proxy_mode: "custom", proxy_url: "http://xxxxx:xxxxx@proxy.example:3128" })]);
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getByRole("button", { name: common.edit }));
  const dialog = await screen.findByRole("dialog");
  const proxy = (await within(dialog).findByLabelText(ai.proxyUrl)) as HTMLInputElement;
  assert.equal(proxy.value, "");
  assert.ok(within(dialog).getByText(ai.proxyUrlStored.replace("{{url}}", "http://xxxxx:xxxxx@proxy.example:3128")));
  await user.click(within(dialog).getByRole("button", { name: common.save }));
  await waitFor(() => assert.equal(fake.calls.updated.length, 1));
  assert.equal(fake.calls.updated[0].body.proxy_mode, "custom");
  assert.ok(!("proxy_url" in fake.calls.updated[0].body), JSON.stringify(fake.calls.updated[0].body));
});

// The image check sets the switch on a definite answer and leaves it alone on
// an unknown one; the answer is worded, with the endpoint's own words.
test("the image check sets the switch from what the model answered", async () => {
  const fake = stubAPI([stored({ image_input: false })]);
  const answers: ("accepted" | "ignored" | "rejected" | "unknown")[] = ["accepted", "unknown", "rejected", "accepted", "ignored"];
  api.testAIConfig = async (body) => {
    fake.calls.tested.push(body as unknown as Record<string, unknown>);
    const image = answers[fake.calls.tested.length - 1];
    return {
      outcome: "ok",
      base_url: (body as { base_url: string }).base_url,
      latency_ms: 5,
      image,
      ...(image === "rejected" ? { image_status: 400, image_detail: "m is not a multimodal model" } : {}),
    };
  };
  fakes.push(fake);
  renderCard();
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("primary");
  await user.click(screen.getByRole("button", { name: common.edit }));
  const dialog = await screen.findByRole("dialog");
  const image = await within(dialog).findByRole("switch", { name: ai.imageInput });
  const check = within(dialog).getByRole("button", { name: ai.imageCheck });
  assert.equal(image.getAttribute("aria-checked"), "false");

  await user.click(check);
  assert.ok(await within(dialog).findByText(ai.imageAccepted));
  assert.equal(image.getAttribute("aria-checked"), "true");
  assert.equal(fake.calls.tested[0].check_image, true);

  await user.click(check);
  assert.ok(await within(dialog).findByText(ai.imageUnknown));
  assert.equal(image.getAttribute("aria-checked"), "true");

  await user.click(check);
  assert.ok(await within(dialog).findByText(ai.imageRejected));
  assert.ok(within(dialog).getByText("m is not a multimodal model"));
  assert.equal(image.getAttribute("aria-checked"), "false");

  // A model that answered without seeing the image turns the switch off too.
  await user.click(check);
  await waitFor(() => assert.equal(image.getAttribute("aria-checked"), "true"));
  await user.click(check);
  assert.ok(await within(dialog).findByText(ai.imageIgnored));
  assert.equal(image.getAttribute("aria-checked"), "false");

  // The operator still has the last word, and the save carries it.
  await user.click(image);
  await user.click(within(dialog).getByRole("button", { name: common.save }));
  await waitFor(() => assert.equal(fake.calls.updated.length, 1));
  assert.equal(fake.calls.updated[0].body.image_input, true);
});
