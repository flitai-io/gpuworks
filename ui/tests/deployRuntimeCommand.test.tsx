import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent() { return false; } }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const locale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", fallbackLng: "en", resources: { en: { deploy: locale } }, defaultNS: "deploy", interpolation: { escapeValue: false } });
const { cleanup, render, screen } = await import("@testing-library/react");
const { Form } = await import("antd");
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
let currentForm: import("antd").FormInstance;
const userEvent = (await import("@testing-library/user-event")).default;
const DeployRuntimeCommand = (await import("../src/pages/deploy/DeployRuntimeCommand.tsx")).default;
const { shellQuote } = await import("../src/pages/deploy/shellQuote.ts");
const { execFileSync } = await import("node:child_process");
type EvaluateResponse = import("../src/api.ts").EvaluateResponse;
type EvaluatePdUnit = import("../src/api.ts").EvaluatePdUnit;
// Tests install their own clipboard to read what a copy wrote; remove it after
// each one so a later test never writes into an earlier test's capture.
afterEach(() => {
  cleanup();
  delete (navigator as { clipboard?: unknown }).clipboard;
});

const script = 'cat > "$SGLANG_LOGGING_CONFIG_PATH" <<\'CONFIG\'\n{"root":{"level":"ERROR"}}\nCONFIG';
const launch = {
  ok: true, argv: ["--log-level", "error"], entrypoint: ["python", "-m", "sglang.launch_server"],
  env: { SGLANG_LOGGING_CONFIG_PATH: "/tmp/gpuworks-sglang-logging.json" },
  pre_start_script: script, image: "sglang:test", warnings: [], effective_params: {}, template_deviated: false,
} as EvaluateResponse;
function ReviewForm({children}: {children: React.ReactNode}) {
  const [form] = Form.useForm();
  currentForm = form;
  return <Form form={form}>{children}</Form>;
}
function show(result: EvaluateResponse | null, loading = false, error: string | null = null) {
  return render(<I18nextProvider i18n={i18n}><ReviewForm><DeployRuntimeCommand result={result} loading={loading} error={error} /></ReviewForm></I18nextProvider>);
}

after(() => { closeChannels(); dom.window.close(); });

test("resolved logging environment and preparation are visible and copied together", async () => {
  const user = userEvent.setup();
  let copied = "";
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { copied = text; } } });
  const { container } = show(launch);
  assert.ok(screen.getByText(locale.runtimeCommand.dynamicBindings));
  assert.ok(container.textContent?.includes(script));
  assert.equal(container.querySelector("details[open]"), null, "preparation starts collapsed");
  assert.ok(container.textContent?.includes("export SGLANG_LOGGING_CONFIG_PATH=/tmp/gpuworks-sglang-logging.json"));
  const edit = screen.getByRole("button", { name: new RegExp(locale.runtimeCommand.edit) }) as HTMLButtonElement;
  assert.equal(edit.disabled, false, "preparation must not disable editing");
  const copy = screen.getByRole("button", { name: locale.runtimeCommand.copyAria });
  copy.focus();
  await user.keyboard("{Enter}");
  assert.ok(copied.includes(script));
  assert.ok(copied.indexOf(script) < copied.indexOf("python -m"));
  await user.click(edit);
  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.enter, exact: true }));
  const editor = screen.getByRole("textbox") as HTMLTextAreaElement;
  assert.equal(editor.value, copied, "manual edit owns the exact complete startup script");
  assert.equal(buildPayload(currentForm.getFieldsValue(true)).payload.container_overrides?.command_includes_preparation, true);
  await user.clear(editor);
  await user.type(editor, "echo edited");
  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.copyAria }));
  assert.equal(copied, "echo edited");
  await user.click(screen.getByRole("button", { name: new RegExp(locale.runtimeCommand.return) }));
  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.discardAndReturn, exact: true }));
  assert.equal(screen.queryByRole("textbox"), null);
  assert.equal(buildPayload(currentForm.getFieldsValue(true)).payload.container_overrides?.command_includes_preparation, undefined);
  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.copyAria }));
  assert.ok(copied.includes(script));
  assert.ok(!copied.includes("echo edited"));
});

test("launches without preparation retain manual command editing", async () => {
  const user = userEvent.setup();
  show({ ...launch, pre_start_script: undefined, env: {} });
  const edit = screen.getByRole("button", { name: new RegExp(locale.runtimeCommand.edit) }) as HTMLButtonElement;
  assert.equal(edit.disabled, false);
  await user.click(edit);
  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.enter, exact: true }));
  assert.ok(screen.getByRole("textbox"));
});

test("each rank displays and copies only its own command, environment, preparation, and mounts", async () => {
  const user = userEvent.setup();
  const copied: string[] = [];
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (value: string) => { copied.push(value); } } });
  const ranks = [0, 1].map((rank) => ({
    ...launch,
    node_uuid: `n${rank}`,
    node_rank: rank,
    role: rank ? "follower" : "leader",
    headless: Boolean(rank),
    command: ["python", "-m", "worker", "--rank", String(rank), '{"line":"one\\ntwo"}'],
    env: { [`RANK_${rank}`]: `value ${rank}` },
    pre_start_script: `echo 'prepare-${rank}'`,
    mounts: rank === 0
      ? [{ source: "/srv/looks-like-kv", target: "/runtime/r0", read_only: true, purpose: "data" }]
      : [{ volume: "worker-cache", target: "/runtime/r1", read_only: false, purpose: "kv-cache" }],
  }));
  const { container } = show({ ...launch, rank_commands: ranks as never });
  const rankLaunches = Array.from(container.querySelectorAll(".deploy-rank-launch"));
  assert.equal(rankLaunches.length, 2);
  assert.ok(rankLaunches[0].textContent?.includes("RANK_0"));
  assert.ok(rankLaunches[0].textContent?.includes("prepare-0"));
  assert.ok(rankLaunches[0].textContent?.includes("/srv/looks-like-kv"));
  assert.ok(rankLaunches[0].textContent?.includes("/runtime/r0"));
  assert.ok(rankLaunches[0].textContent?.includes("Read-only"));
  assert.ok(rankLaunches[0].textContent?.includes("data"), "purpose comes from the wire even when the path resembles a KV mount");
  assert.equal(rankLaunches[0].textContent?.includes("/runtime/r1"), false);
  assert.ok(rankLaunches[1].textContent?.includes("Volume"));
  assert.ok(rankLaunches[1].textContent?.includes("worker-cache"));
  assert.ok(rankLaunches[1].textContent?.includes("Read-write"));
  assert.ok(rankLaunches[1].textContent?.includes("kv-cache"));

  await user.click(screen.getByRole("button", { name: locale.catalog.copyRank.replace("{{rank}}", "0") }));
  assert.ok(copied[0].includes("RANK_0"));
  assert.ok(copied[0].includes("prepare-0"));
  assert.ok(copied[0].includes('/srv/looks-like-kv'));
  assert.ok(copied[0].includes('"purpose":"data"'));
  assert.equal(copied[0].includes("/runtime/r1"), false);

  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.copyMountsAria.replace("{{rank}}", "1") }));
  assert.ok(copied[1].startsWith("# Mounts (runtime configuration)"));
  assert.ok(copied[1].includes('"volume":"worker-cache"'));
  assert.equal(copied[1].includes("RANK_1"), false, "the mount-only control copies the mount projection only");

  assert.ok(screen.getAllByText(locale.catalog.resolvedRuntime).length >= 2, "raw JSON details remain available for every rank");
});

test("PD runtime commands display and copy each rank of every role unit, followers included", async () => {
  const user = userEvent.setup();
  const copied: string[] = [];
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (value: string) => { copied.push(value); } } });
  // The wire shape evaluatePD answers: argv always present, command the
  // entrypoint and argv joined, env/mounts only when a rank has them
  // (omitempty), gpus only on a rank that fits, and the top-level command
  // fields describing the first decode instance.
  const rank = (node: string, name: string, argv: string[], extra: Partial<EvaluatePdUnit> = {}): EvaluatePdUnit => ({
    node_uuid: node, node_name: name, address: `10.0.0.${name.at(-1)}`, gpus: [0], fits: true,
    argv, command: ["vllm", "serve", ...argv], ...extra,
  });
  const decodeLeader = rank("d1", "Decode Node 1", ["/model", "--role", "decode", "--node-rank", "0"], {
    env: { VLLM_NIXL_SIDE_CHANNEL_PORT: "5601" },
    mounts: [{ source: "/srv/decode-cache", target: "/runtime/d1", read_only: false, purpose: "kv-cache" }],
    followers: [rank("d2", "Decode Node 2", ["/model", "--role", "decode", "--node-rank", "1", "--headless"], { env: { VLLM_HOST_IP: "10.0.0.2" } })],
  });
  const secondDecode = rank("d3", "Decode Node 3", ["/model", "--role", "decode"]);
  const prefill = rank("p1", "Prefill Node 1", ["/model", "--role", "prefill"], {
    gpus: undefined, fits: false, reason: "not enough VRAM on Prefill Node 1",
  });
  const { container } = show({
    ok: false, image: "vllm:test", argv: decodeLeader.argv, command: decodeLeader.command, env: decodeLeader.env, mounts: decodeLeader.mounts,
    warnings: [], template_deviated: false, replicas: 3, plan_scope: "per_instance",
    pd: {
      mode: "vllm-nixl", fallback: "decode", kv_load_failure: "fail",
      roles: [
        { role: "decode", tp: 1, pp: 2, units: [decodeLeader, secondDecode] },
        { role: "prefill", tp: 1, units: [prefill] },
      ],
    },
  } as EvaluateResponse);

  const launches = Array.from(container.querySelectorAll(".deploy-rank-launch"));
  const expected = [
    { label: "Decode #1 · Decode Node 1", unit: decodeLeader },
    { label: "Decode #1 · Decode Node 2", unit: decodeLeader.followers![0] },
    { label: "Decode #2 · Decode Node 3", unit: secondDecode },
    { label: "Prefill #1 · Prefill Node 1", unit: prefill },
  ];
  assert.deepEqual(launches.map((launch) => launch.querySelector("pre")?.textContent), expected.map(({ unit }) => unit.command!.join(" ")), "one launch per rank, a follower after its unit's own, decode before prefill");
  assert.ok(launches[0].textContent?.includes("/runtime/d1"));
  assert.equal(launches[1].textContent?.includes("/runtime/d1"), false, "a follower shows its own mounts, not its leader's");
  assert.ok(launches[1].textContent?.includes("VLLM_HOST_IP"));
  assert.equal(launches[1].textContent?.includes("VLLM_NIXL_SIDE_CHANNEL_PORT"), false);

  for (const { label, unit } of expected) {
    await user.click(screen.getByRole("button", { name: locale.catalog.copyRank.replace("{{rank}}", label) }));
    const environment = Object.entries(unit.env || {}).map(([key, value]) => `export ${key}=${shellQuote(value)}`);
    const script = ["set -eu", ...environment, `exec ${unit.command!.map(shellQuote).join(" ")}`].join("\n");
    if (unit.mounts) {
      assert.ok(copied.at(-1)!.startsWith(`${script}\n# Mounts (runtime configuration)`), `${label} copies its script, then its own mounts`);
      assert.ok(copied.at(-1)!.includes('"/srv/decode-cache"'));
    } else {
      assert.equal(copied.at(-1), script, `${label} copies exactly its own startup script`);
    }
  }

  assert.ok(screen.getByText(locale.catalog.multiRankManualUnavailable));
  const edit = screen.getByRole("button", { name: new RegExp(locale.runtimeCommand.edit) }) as HTMLButtonElement;
  assert.equal(edit.disabled, true, "a PD draft cannot enter manual mode from the first decode instance's command");
  const copyAll = screen.getByRole("button", { name: locale.runtimeCommand.copyAria }) as HTMLButtonElement;
  assert.equal(copyAll.disabled, true, "no single command stands for every PD rank");
});

// Two ranks of a multi-node draft, each with its own resolved command.
function multiRankResult(extra: Partial<EvaluateResponse> = {}): EvaluateResponse {
  const ranks = [0, 1].map((rank) => ({
    ...launch, node_uuid: `n${rank}`, node_rank: rank, role: rank ? "follower" : "leader", headless: Boolean(rank),
    command: ["/bin/sh", "-c", `exec vllm serve /model --node-rank ${rank}`], env: { VLLM_HOST_IP: `10.0.0.${rank + 1}` },
  }));
  return { ...launch, rank_commands: ranks as never, ...extra };
}

test("several launches enter manual mode from the server's shared script", async () => {
  const user = userEvent.setup();
  const draft = 'set -eu\ncase "${GPUWORKS_HEADLESS}" in\n  0)\n    exec vllm serve /model --node-rank ${GPUWORKS_NODE_RANK}\n    ;;\nesac';
  const variables = ["GPUWORKS_HEADLESS", "GPUWORKS_NODE_RANK"];
  const result = multiRankResult({ manual_command_draft: draft, manual_command_variables: variables });
  const { container } = show(result);
  assert.ok(screen.getByText(locale.catalog.multiRankManualDraft));
  const edit = screen.getByRole("button", { name: new RegExp(locale.runtimeCommand.edit) }) as HTMLButtonElement;
  assert.equal(edit.disabled, false, "a server draft makes the command editable");
  await user.click(edit);
  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.enter, exact: true }));
  const editor = screen.getByRole("textbox") as HTMLTextAreaElement;
  assert.equal(editor.value, draft, "the editor starts from the server's script, not one rank's command");
  const overrides = buildPayload(currentForm.getFieldsValue(true)).payload.container_overrides;
  assert.equal(overrides?.command_mode, "manual");
  assert.equal(overrides?.command_includes_preparation, undefined, "the platform keeps preparing every rank (false is the omitted default)");
  assert.equal(currentForm.getFieldValue("command_includes_preparation"), false);

  // Under the editor: every rank's resolved launch as evaluate returned it,
  // and the variables the server listed.
  assert.ok(screen.getByText(locale.catalog.sharedManualCommand));
  assert.ok(screen.getByText(locale.runtimeCommand.variablesHint));
  for (const name of variables) assert.ok(screen.getByText(`\${${name}}`));
  const launches = Array.from(container.querySelectorAll(".deploy-rank-launch"));
  assert.equal(launches.length, 2);
  assert.ok(launches[0].textContent?.includes("--node-rank 0"));
  assert.ok(launches[1].textContent?.includes("--node-rank 1"));
  assert.ok(launches[1].textContent?.includes("VLLM_HOST_IP"));
  assert.equal(screen.queryByText(locale.catalog.multiRankManualDraft), null);
});

test("several launches without a server script keep editing off and say why", () => {
  show(multiRankResult({ warnings: [{ code: "MANUAL_DRAFT_UNAVAILABLE" }] }));
  const edit = screen.getByRole("button", { name: new RegExp(locale.runtimeCommand.edit) }) as HTMLButtonElement;
  assert.equal(edit.disabled, true);
  assert.ok(screen.getByText(locale.warning.MANUAL_DRAFT_UNAVAILABLE));
  assert.equal(screen.queryByText(locale.catalog.multiRankManualUnavailable), null);
});

test("a single launch still enters manual mode with its own preparation", async () => {
  const user = userEvent.setup();
  show({ ...launch, manual_command_draft: "ignored for one launch" });
  await user.click(screen.getByRole("button", { name: new RegExp(locale.runtimeCommand.edit) }));
  await user.click(screen.getByRole("button", { name: locale.runtimeCommand.enter, exact: true }));
  assert.ok((screen.getByRole("textbox") as HTMLTextAreaElement).value.includes(script));
  assert.equal(currentForm.getFieldValue("command_includes_preparation"), true);
  assert.equal(screen.queryByText(locale.catalog.sharedManualCommand), null);
});

test("loading, empty, and error states do not claim a launch command", () => {
  for (const [result, loading, error, text] of [
    [null, true, null, locale.runtimeCommand.generating],
    [null, false, null, locale.runtimeCommand.afterSelection],
    [null, false, "failed", locale.runtimeCommand.afterChecks],
  ] as const) {
    show(result, loading, error);
    assert.ok(screen.getByText(text));
    cleanup();
  }
});

test("shell rendering preserves environment and JSON argument boundaries", () => {
  const value = `space ' quote $literal`;
  const argument = '{"rope_scaling": {"factor": 2}}';
  const command = `export GPUWORKS_TEST_VALUE=${shellQuote(value)}\n` + ["python3", "-c", "import json, os, sys; print(json.dumps([os.environ['GPUWORKS_TEST_VALUE'], sys.argv[1]]))", argument].map(shellQuote).join(" ");
  const got = JSON.parse(execFileSync("/bin/sh", ["-ec", command], { encoding: "utf8" }));
  assert.deepEqual(got, [value, argument]);
});
