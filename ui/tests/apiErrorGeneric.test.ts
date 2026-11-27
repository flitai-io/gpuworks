import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";

import { APIError, apiErrorText, apiResponseError, GENERIC_API_ERROR_CODES, isGenericAPIErrorCode } from "../src/apiError.ts";
import { playgroundErrorText } from "../src/pages/playground/api.ts";

// Every server error now has a code; one the server did not name gets its
// status's generic code, which says only the category. The English text holds
// what actually went wrong, so the console shows both.

async function translator(lng: "en" | "zh") {
  const i18n = createInstance();
  const common = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lng}/common.json`, import.meta.url), "utf8"));
  await i18n.init({ lng, resources: { [lng]: { common } }, defaultNS: "common", interpolation: { escapeValue: false } });
  return (key: string, options: Record<string, unknown>) => i18n.t(key, options);
}

async function errorFrom(body: unknown, status: number) {
  return apiResponseError(new Response(JSON.stringify(body), { status, headers: { "X-Request-ID": "r9" } }), "request failed");
}

test("a generic code reads as the localized category followed by the server's detail", async () => {
  const error = await errorFrom({ error: "deployment 7 not found", code: "NOT_FOUND" }, 404);
  assert.ok(error instanceof APIError);
  assert.equal(error.requestId, "r9");
  assert.equal(apiErrorText(await translator("zh"), error), "未找到：deployment 7 not found");
  assert.equal(apiErrorText(await translator("en"), error), "Not found: deployment 7 not found");
  const internal = await errorFrom({ error: "database is locked", code: "INTERNAL_ERROR" }, 500);
  assert.equal(apiErrorText(await translator("zh"), internal), "服务端出错：database is locked");
  const invalid = await errorFrom({ error: "invalid id", code: "BAD_REQUEST" }, 400);
  assert.equal(apiErrorText(await translator("zh"), invalid), "请求无效：invalid id");
});

test("a generic code without detail is the category alone", async () => {
  const error = new APIError("SERVICE_UNAVAILABLE", undefined, "", 503);
  assert.equal(apiErrorText(await translator("zh"), error), "服务暂不可用");
});

test("a specific code still reads from its own text, not as a category", async () => {
  const error = await errorFrom({ error: "two mounts claim /data", code: "MOUNT_TARGET_DUPLICATE", params: { target: "/data" } }, 400);
  assert.ok(!isGenericAPIErrorCode("MOUNT_TARGET_DUPLICATE"));
  const zh = apiErrorText(await translator("zh"), error);
  assert.ok(!zh.includes("two mounts claim"), zh);
  assert.ok(!zh.startsWith("请求无效"), zh);
});

test("a disconnected ZStack host shows its status and recovery step before SSH installation", async () => {
  const error = await errorFrom({
    error: "install through SSH connection: host is disconnected",
    code: "ZSTACK_AGENT_HOST_NOT_CONNECTED",
    params: { host: "compute-1", address: "192.0.2.11", status: "Disconnected" },
  }, 424);
  assert.equal(apiErrorText(await translator("zh"), error),
    "无法安装 Agent：ZStack 物理机 compute-1（192.0.2.11）尚未连接（当前状态：Disconnected）。请在 ZStack 中重连物理机，待状态变为 Connected 后重试。");
  assert.match(apiErrorText(await translator("en"), error), /compute-1 .*192\.0\.2\.11.*Disconnected.*Reconnect.*Connected/);
});

test("every generic code has en and zh category text", async () => {
  for (const lng of ["en", "zh"] as const) {
    const t = await translator(lng);
    for (const code of GENERIC_API_ERROR_CODES) {
      const text = apiErrorText(t, new APIError(code, undefined, "detail", 400));
      assert.ok(!text.startsWith(code), `${lng} ${code}: ${text}`);
      assert.ok(text.endsWith("detail"), `${lng} ${code}: ${text}`);
    }
  }
});

test("gateway errors render from their code in the Playground", () => {
  const asleep = JSON.stringify({ error: { message: "model is asleep; wake it first (console, or POST /api/v1/deployments/7/wake; a schedule or a waiting wake may also wake it)", type: "server_error", code: "MODEL_ASLEEP", param: null, params: { deployment_id: 7 } } });
  assert.equal(playgroundErrorText(503, asleep), "The model is asleep. Wake deployment 7 first, or wait for its schedule or a waiting wake.");
  const tooLarge = JSON.stringify({ error: { message: "request body exceeds 64 bytes", type: "invalid_request_error", code: "REQUEST_TOO_LARGE", param: null, params: { max_bytes: 64 } } });
  assert.equal(playgroundErrorText(413, tooLarge), "Request too large: request body exceeds 64 bytes");
  const previous = globalThis.document;
  Object.defineProperty(globalThis, "document", { configurable: true, value: { documentElement: { lang: "zh-CN" } } });
  try {
    assert.equal(playgroundErrorText(503, asleep), "模型正在睡眠。请先唤醒部署 7，或等待定时唤醒或排队中的唤醒。");
    assert.equal(playgroundErrorText(413, tooLarge), "请求过大：request body exceeds 64 bytes");
    const modality = JSON.stringify({ error: { message: "This model does not support image_url input (supported: text).", type: "invalid_request_error", code: "MODEL_INPUT_MODALITY_UNSUPPORTED", param: "messages", params: { modality: "image_url", supported: "text" } } });
    assert.equal(playgroundErrorText(400, modality), "该模型不支持 image_url 输入（支持：text）。请移除附件或选择兼容的模型。");
  } finally {
    if (previous === undefined) Reflect.deleteProperty(globalThis, "document");
    else Object.defineProperty(globalThis, "document", { configurable: true, value: previous });
  }
  // A code the console has no text for, or one missing a param its text
  // names, keeps the server's message.
  assert.equal(playgroundErrorText(429, JSON.stringify({ error: { message: "Rate limit reached", code: "rate_limit" } })), "Rate limit reached");
  assert.equal(playgroundErrorText(404, JSON.stringify({ error: { message: "no deployment serves model x", code: "MODEL_NOT_FOUND" } })), "no deployment serves model x");
});
