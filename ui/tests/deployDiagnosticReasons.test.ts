import assert from "node:assert/strict";
import { after, test } from "node:test";
import { createI18n, dom } from "./helpers/dom.ts";
import { diagnosticMessageText } from "../src/pages/deploy/deployDiagnosticText.ts";

after(() => dom.window.close());

for (const language of ["en", "zh"] as const) {
  test(`metadata failures retain distinct localized DNS, TLS, connect and authentication causes (${language})`, async () => {
    const { i18n } = await createI18n(["deploy"], language);
    const translate = (namespace: string) => (key: string, options: Record<string, unknown>) => i18n.t(key, { ...options, ns: namespace });
    const actual = ["source_dns_failed", "source_tls_failed", "source_connect_failed", "source_authentication"].map((reason) => diagnosticMessageText(translate("deploy"), translate("common"), {
      code: "MODEL_SOURCE_UNAVAILABLE", params: { reason }, message: "The management node could not read the model metadata",
    }));
    assert.equal(new Set(actual).size, 4, "four actual backend causes must not collapse into one generic fallback");
    for (const message of actual) assert.doesNotMatch(message, /The management node could not read the model metadata|diagnostics\.reason\./);
    assert.match(actual[0], /DNS/);
    assert.match(actual[1], /TLS/);
    assert.match(actual[2], language === "zh" ? /连接/ : /connect/i);
    assert.match(actual[3], language === "zh" ? /凭据|认证/ : /credential|authenticat/i);
  });

  test(`agent storage causes reuse their existing localized reason vocabulary (${language})`, async () => {
    const { i18n, locale } = await createI18n(["deploy"], language);
    const translate = (namespace: string) => (key: string, options: Record<string, unknown>) => i18n.t(key, { ...options, ns: namespace });
    for (const reason of ["missing_client", "unreachable", "source_mismatch", "not_root", "path_missing"]) {
      const actual = diagnosticMessageText(translate("deploy"), translate("common"), {
        code: "STORAGE_UNAVAILABLE_ON_NODE", params: { reason }, message: "Storage is unavailable",
      });
      assert.equal(actual, locale.common.storageReason[reason], `${reason} must retain the agent's specific cause`);
    }
  });

  test(`storage progress and availability causes keep distinct localized explanations (${language})`, async () => {
    const { i18n } = await createI18n(["deploy"], language);
    const translate = (namespace: string) => (key: string, options: Record<string, unknown>) => i18n.t(key, { ...options, ns: namespace });
    const actual = ["storage_mounting", "storage_unresponsive", "storage_unsupported", "storage_not_writable"].map((reason) => diagnosticMessageText(translate("deploy"), translate("common"), {
      code: "STORAGE_UNAVAILABLE_ON_NODE", params: { reason }, message: "Storage is unavailable",
    }));
    assert.equal(new Set(actual).size, 4);
    for (const message of actual) assert.doesNotMatch(message, /Storage is unavailable|diagnostics\.reason\./);
    assert.match(actual[0], language === "zh" ? /正在挂载/ : /mounting/);
  });

  test(`a denied peer path names the source and target and explains the storage-sync constraint (${language})`, async () => {
    const { i18n } = await createI18n(["deploy"], language);
    const translate = (namespace: string) => (key: string, options: Record<string, unknown>) => i18n.t(key, { ...options, ns: namespace });
    const message = diagnosticMessageText(translate("deploy"), translate("common"), { code: "NO_STORAGE_PATH", params: { reason: "no_storage_path", source: "peer-a", target: "gpu-b" }, message: "no storage path" });
    assert.match(message, /peer-a/);
    assert.match(message, /gpu-b/);
    assert.match(message, language === "zh" ? /存储同步/ : /storage sync/);
    assert.doesNotMatch(message, /Download from the source instead|请改为从源下载/);
    const blocked = diagnosticMessageText(translate("deploy"), translate("common"), { code: "MODEL_SOURCE_UNAVAILABLE", params: { reason: "peer_transport_required" }, message: "peer path required" });
    assert.doesNotMatch(blocked, /peer path required|diagnostics\.reason\./);
  });
}
