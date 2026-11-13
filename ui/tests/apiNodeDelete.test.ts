import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, test } from "node:test";
import { createInstance } from "i18next";
import { JSDOM } from "jsdom";
import { APIError, apiErrorText, apiResponseError } from "../src/apiError.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
const { api } = await import("../src/api.ts");
const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

test("node deletion only sends force when the caller explicitly selects it", async () => {
  const requests: { url: string; method?: string }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    requests.push({ url, method: init?.method });
    return Response.json({ status: "deleted", agent_uninstalled: !url.endsWith("?force=true") });
  }) as typeof fetch;
  assert.deepEqual(await api.deleteNode("node/one"), { status: "deleted", agent_uninstalled: true });
  assert.deepEqual(await api.deleteNode("node/one", true), { status: "deleted", agent_uninstalled: false });
  assert.deepEqual(requests, [
    { url: "/api/v1/nodes/node%2Fone", method: "DELETE" },
    { url: "/api/v1/nodes/node%2Fone?force=true", method: "DELETE" },
  ]);
});

for (const language of ["en", "zh"] as const) {
  for (const entry of [
    {
      code: "NODE_DELETE_SSH_REQUIRED",
      wire: '{"code":"NODE_DELETE_SSH_REQUIRED","error":"normal deletion requires saved SSH credentials to uninstall the agent; node retained. Configure SSH and retry, or uninstall the agent manually and use force=true to remove only its management record"}',
      params: undefined,
      en: "Normal deletion requires SSH to uninstall the agent, but this node has no saved SSH credentials. The node record was retained. Configure SSH credentials and retry, or uninstall the agent manually and then select Force removal to delete its management record.",
      zh: "普通删除需要通过 SSH 卸载节点上的 agent，当前节点未保存 SSH 凭据，节点记录已保留。请配置 SSH 凭据后重试，或在节点上手动卸载 agent 后勾选“强制移除”删除管理记录。",
    },
    {
      code: "NODE_UNINSTALL_UNCONFIRMED",
      wire: '{"code":"NODE_UNINSTALL_UNCONFIRMED","error":"agent uninstall over SSH was not confirmed; node and SSH credentials retained. Fix SSH and retry, or uninstall the agent manually and use force=true to remove only its management record: SSH unreachable","params":{"detail":"SSH unreachable"}}',
      params: { detail: "SSH unreachable" },
      en: "Normal deletion could not confirm that the agent was uninstalled over SSH. The node and SSH credentials were retained. Fix the SSH connection or credentials and retry, or uninstall the agent manually and then select Force removal to delete its management record. Details: SSH unreachable",
      zh: "普通删除时，无法确认已通过 SSH 卸载 agent，节点和 SSH 凭据已保留。请修复 SSH 连接或凭据后重试，或在节点上手动卸载 agent 后勾选“强制移除”删除管理记录。详情：SSH unreachable",
    },
  ]) {
    test(`${entry.code} wire explains ordinary deletion and recovery steps in ${language}`, async () => {
      const common = JSON.parse(readFileSync(new URL(`../src/i18n/locales/${language}/common.json`, import.meta.url), "utf8"));
      const i18n = createInstance();
      await i18n.init({ lng: language, resources: { [language]: { common } }, defaultNS: "common", interpolation: { escapeValue: false } });
      const error = await apiResponseError(new Response(entry.wire, { status: 409 }), "fallback");
      assert.ok(error instanceof APIError);
      assert.equal(error.code, entry.code);
      assert.equal(error.status, 409);
      assert.deepEqual(error.params, entry.params);
      assert.equal(apiErrorText((key, options) => i18n.t(key, options), error), entry[language]);
    });
  }
}
