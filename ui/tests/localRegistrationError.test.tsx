import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { createI18n } from "./helpers/dom.ts";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { installFleet } from "./helpers/fakeFleet.ts";
import { list } from "./helpers/listPage.ts";
const { cleanup, render, screen, fireEvent, waitFor } = await import("@testing-library/react");
const { message } = await import("antd");
const { APIError, apiResponseError } = await import("../src/apiError.ts");
const { localRegistrationErrorText } = await import("../src/pages/models/localRegistrationError.ts");
const { RegisterModal } = await import("../src/pages/models/RegisterModelModal.tsx");
const { api } = await import("../src/api.ts");
const original = { nodes: api.nodes, nodesPage: api.nodesPage, storages: api.storages, imageVersions: api.imageVersions, previewLocalModel: api.previewLocalModel, registerLocalModel: api.registerLocalModel };
const originalError = message.error;
afterEach(() => { cleanup(); Object.assign(api, original); message.error = originalError; });

const header = "validate local model: directory must identify exactly one model entry (found 0):";
const missingWeights = "Transformers configuration was read, but no supported checkpoint weights or index were found in this directory";
const invalidJSON = "unexpected end of JSON input";

for (const lng of ["zh", "en"] as const) {
  test(`${lng} translates the existing plain-text responses without changing file paths`, async () => {
    const { i18n } = await createI18n(["models"], lng);
    for (const [source, expected] of [
      [missingWeights, "在此目录中未找到支持的模型权重或权重索引"],
      [invalidJSON, "JSON 内容不完整，解析意外结束"],
    ]) {
      const path = "model: 中文/unexpected end of JSON input/config.json";
      const raw = `${header}\n- ${path}: ${source}`;
      const error = await apiResponseError(new Response(JSON.stringify({error:raw}),{status:400}), "failed");
      assert.ok(!(error instanceof APIError), "the existing server response has no product error code");
      const rendered = localRegistrationErrorText(i18n.getFixedT(lng, "models"), error);
      if (lng === "en") assert.equal(rendered,raw);
      else {
        assert.ok(rendered.includes(expected),rendered);
        assert.ok(rendered.includes("当前识别到 0 个"));
      }
      assert.ok(rendered.includes(path), "file names must not be translated");
    }
  });
  test(`${lng} keeps the existing error message for both preview and registration failure`, async () => {
    const { i18n, locale } = await createI18n(["models"], lng);
    const text = locale.models;
    installFleet(api as never, [{uuid:"n1",hostname:"diagnostic-node",gpu_count:0,state:"online"}] as never);
    api.storages = (async () => list([])) as never;
    api.imageVersions = (async () => list([])) as never;
    const rejection = (reason: string) => new Error(`${header}\n- config.json: ${reason === "missing_weights" ? missingWeights : invalidJSON}`);
    api.previewLocalModel = (async (_node: string, path: string) => {
      if (path === "/models/broken") throw rejection("missing_weights");
      return {node_uuid:"n1",path,source_id:"local/fixed",name:"fixed",weight_bytes:1024};
    }) as never;
    api.registerLocalModel = (async () => { throw rejection("invalid_json"); }) as never;
    const messages: string[] = [];
    message.error = ((value: unknown) => { messages.push(String(value)); }) as never;
    render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
    fireEvent.mouseDown(screen.getByRole("combobox",{name:text.column.source}));
    fireEvent.click(await screen.findByText(text.register.localPath));
    fireEvent.mouseDown(await screen.findByRole("combobox",{name:text.detail.node}));
    fireEvent.click(await screen.findByText("diagnostic-node"));
    const path = screen.getByPlaceholderText("/models/Qwen/Qwen3-8B");
    fireEvent.change(path,{target:{value:"/models/broken"}});
    fireEvent.click(screen.getByRole("button",{name:text.register.parsePreview}));
    await waitFor(() => assert.equal(messages.length,1));
    assert.ok(messages[0].includes(lng === "zh" ? "未找到支持的模型权重" : "no supported checkpoint weights"));
    assert.equal(screen.queryByRole("button",{name:text.register.confirm}),null);
    fireEvent.change(path,{target:{value:"/models/fixed"}});
    fireEvent.click(screen.getByRole("button",{name:text.register.parsePreview}));
    fireEvent.click(await screen.findByRole("button",{name:text.register.confirm}));
    await waitFor(() => assert.equal(messages.length,2));
    assert.ok(messages[1].includes(lng === "zh" ? "JSON 内容不完整" : "unexpected end of JSON input"));
  });
}

test("unknown messages and unrecognized clauses remain verbatim", async () => {
  const { i18n } = await createI18n(["models"], "zh");
  const t = i18n.getFixedT("zh", "models");
  for (const error of [new Error("node offline"),new APIError("SOME_OTHER_ERROR",{},"other failure",400)]) {
    assert.equal(localRegistrationErrorText(t,error), error.message);
  }
  const detail = "- config.json: unexpected end of JSON input from an unknown parser";
  const rendered = localRegistrationErrorText(t,new Error(`${header}\n${detail}`));
  assert.ok(rendered.endsWith(detail), "partial message matches must not invent a translation");
});

test("missing-file paths and empty-report explanations keep their existing meaning", async () => {
  const { i18n } = await createI18n(["models"], "zh");
  const t = i18n.getFixedT("zh", "models");
  const missing = localRegistrationErrorText(t,new Error("model entry has missing or unresolved files:\n- missing or unresolved file: model/file is empty.safetensors"));
  assert.equal(missing,"模型条目中存在缺失或无法确认的文件:\n- 缺失或无法确认的文件: model/file is empty.safetensors");
  const empty = localRegistrationErrorText(t,new Error("directory must identify exactly one model entry (found 0): the recognizer returned no detailed failure reason"));
  assert.ok(empty.includes("识别器未返回详细失败原因"));
});
