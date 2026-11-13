import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import "./helpers/dom.ts";
import { describedBy, field } from "./helpers/platformDialog.ts";
import { kindCategories } from "./helpers/platformFixtures.ts";

// A ZStack account in the Add / Edit External Platform dialog
// (ui/src/components/ProviderConfigModal.tsx), through the real client: the
// address template that must be replaced before anything is sent, and the
// server's refusals said in the viewer's language at the foot of the form.

const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { default: i18n } = await import("../src/i18n/index.ts");
type KindInfo = import("../src/api.ts").ProviderKindInfo;

for (const language of ["en", "zh"]) {
  for (const namespace of ["common", "providers"]) {
    i18n.addResourceBundle(language, namespace, JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/${namespace}.json`, import.meta.url), "utf8")));
  }
}
const originalFetch = globalThis.fetch;
afterEach(async () => { await act(async () => { cleanup(); }); globalThis.fetch = originalFetch; });

const endpointTemplate = "http://MANAGEMENT_NODE_IP:8080/zstack";
const kindInfos: KindInfo[] = [{
  kind: "zstack", label: { en: "ZStack" }, category: "private_cloud", resources: [{ type: "host" }], server_dial_supported: false,
  capabilities: { can_list_instances: true, can_power_control: false, can_install_agent: true, can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: false },
  credential_fields: [
    { name: "endpoint", label: { en: "API Endpoint", zh: "API 接口地址" }, secret: false, required: true, default_template: endpointTemplate },
    { name: "public_key", label: { en: "AccessKey ID" }, secret: false, required: true },
    { name: "private_key", label: { en: "AccessKey Secret" }, secret: true, required: true },
  ],
}];
// The fields by the labels the kind's schema gives them: the address has one per language, the keys only an English one.
const LABELS = { name: { en: "Name", zh: "名称" }, endpoint: { en: "API Endpoint", zh: "API 接口地址" }, public_key: { en: "AccessKey ID", zh: "AccessKey ID" }, private_key: { en: "AccessKey Secret", zh: "AccessKey Secret" } };
const input = (name: keyof typeof LABELS) => screen.queryByLabelText(field(LABELS[name][i18n.language as "en" | "zh"])) as HTMLInputElement;
/** The refusals the dialog shows, top to bottom. */
const alerts = () => within(screen.getByRole("dialog")).queryAllByRole("alert").map((alert) => alert.textContent);
const addButton = () => screen.getByRole("button", { name: (name) => name.replace(/\s/g, "") === i18n.t("config.add", { ns: "providers" }).replace(/\s/g, "") });

for (const language of ["en", "zh"] as const) {
  test(`ZStack template must be replaced before a request is sent (${language})`, async () => {
    await i18n.changeLanguage(language);
    const requests: Record<string, unknown>[] = [];
    let saved = 0;
    globalThis.fetch = async (url, init) => {
      assert.equal(url, "/api/v1/providers");
      assert.equal(init?.method, "POST");
      requests.push(JSON.parse(String(init?.body)));
      return new Response('{"id":1,"kind":"zstack","name":"cloud"}', { status: 201 });
    };
    render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind="zstack" kindInfos={kindInfos} categories={kindCategories(kindInfos)} providerLabel="ZStack" onClose={() => {}} onSaved={() => { saved++; }} /></I18nextProvider>);
    await waitFor(() => assert.equal(input("endpoint")?.value, endpointTemplate));
    // What the vendor's version must be is a notice at the head of the form, not a refusal.
    assert.deepEqual(within(screen.getByRole("dialog")).getAllByRole("status").map((notice) => notice.textContent), [language === "zh" ? "需要 ZStack Cloud 5.4.2 或更高版本。" : "Requires ZStack Cloud 5.4.2 or later."]);
    assert.deepEqual(alerts(), []);
    for (const [name, value] of Object.entries({ name: "cloud", public_key: "entered-ak", private_key: "entered-sk" })) {
      fireEvent.change(input(name as keyof typeof LABELS), { target: { value } });
    }
    const error = language === "zh"
      ? "请将API 接口地址中的占位内容替换为实际值"
      : "Replace the placeholder in API Endpoint with an actual value";
    for (const value of [endpointTemplate, ` ${endpointTemplate} `]) {
      fireEvent.change(input("endpoint"), { target: { value } });
      await act(async () => { fireEvent.click(addButton()); });
      await waitFor(() => assert.deepEqual(alerts(), [error], "said under the address, and of nothing else"));
      assert.equal(input("endpoint").getAttribute("aria-invalid"), "true");
      assert.equal(describedBy(input("endpoint")), error);
      assert.ok(document.activeElement === input("endpoint"), "the refused field takes the focus");
      assert.equal(requests.length, 0, "invalid templates must never reach the server");
      assert.equal(saved, 0);
      assert.equal(input("private_key").value, "entered-sk");
    }

    fireEvent.change(input("endpoint"), { target: { value: "" } });
    fireEvent.click(addButton());
    await waitFor(() => assert.deepEqual(alerts(), [language === "zh" ? "请输入 API 接口地址" : "Enter API Endpoint"]));
    assert.equal(requests.length, 0, "clearing the required endpoint cannot bypass validation");

    const endpoint = "https://192.0.2.10:8443/zstack";
    fireEvent.change(input("endpoint"), { target: { value: endpoint } });
    fireEvent.click(addButton());
    await waitFor(() => assert.equal(saved, 1));
    assert.equal(requests.length, 1);
    assert.equal(requests[0].endpoint, endpoint);
    assert.equal(requests[0].public_key, "entered-ak");
    assert.equal(requests[0].private_key, "entered-sk");
    assert.equal(screen.queryAllByText(error).length, 0);
  });

  test(`ZStack add shows localized failures, keeps entered credentials and permits retry (${language})`, async () => {
    await i18n.changeLanguage(language);
    let saved = 0;
    let code = "ZSTACK_ACCESS_KEY_INVALID";
    globalThis.fetch = async (url, init) => {
      assert.equal(url, "/api/v1/providers");
      assert.equal(init?.method, "POST");
      const body = JSON.parse(String(init?.body));
      assert.equal(body.public_key, "entered-ak");
      assert.equal(body.private_key, "entered-sk");
      return code
        ? new Response(JSON.stringify({ code, error: "English server fallback" }), { status: 400 })
        : new Response('{"id":1,"kind":"zstack","name":"cloud"}', { status: 201 });
    };
    render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind="zstack" kindInfos={kindInfos} categories={kindCategories(kindInfos)} providerLabel="ZStack" onClose={() => {}} onSaved={() => { saved++; }} /></I18nextProvider>);
    await waitFor(() => assert.ok(input("endpoint")));
    for (const [name, value] of Object.entries({ name: "cloud", endpoint: "http://cloud.example.test:8080/zstack", public_key: "entered-ak", private_key: "entered-sk" })) {
      fireEvent.change(input(name as keyof typeof LABELS), { target: { value } });
    }
    for (const [nextCode, zh, en] of [
      ["ZSTACK_ACCESS_KEY_INVALID", "AK 不存在或已禁用", "AK does not exist or is disabled"],
      ["ZSTACK_PORT_REFUSED", "端口连接被拒绝", "port refused the connection"],
      ["ZSTACK_IP_UNREACHABLE", "无法到达 ZStack IP", "cannot reach the ZStack IP"],
      ["ZSTACK_NETWORK_UNCONFIRMED", "目前无法确定具体原因", "the cause is not yet determined"],
      ["ZSTACK_NETWORK_DIAGNOSTIC_UNAVAILABLE", "IP 探测无法执行", "IP probe could not run"],
      ["ZSTACK_LICENSE_REQUIRED", "有效的企业版许可证", "valid Enterprise license"],
      ["ZSTACK_RESPONSE_TIMEOUT", "API 响应超时", "API response timed out"],
      ["ZSTACK_REQUEST_TIMEOUT", "API 请求超时", "API request timed out"],
    ]) {
      code = nextCode;
      fireEvent.click(addButton());
      // One notice at the foot of the form, replaced by the next refusal; it names the operation in the viewer's language.
      await waitFor(() => {
        assert.equal(alerts().length, 1);
        assert.match(alerts()[0] ?? "", new RegExp(language === "zh" ? `^添加失败：.*${zh}` : `^Add failed: .*${en}`));
      });
      assert.equal(saved, 0, "failed validation must leave the dialog open");
      assert.equal(input("private_key").value, "entered-sk");
      assert.equal(input("endpoint").value, "http://cloud.example.test:8080/zstack");
      assert.equal(screen.queryAllByText(/English server fallback/).length, 0);
      await waitFor(() => assert.equal((addButton() as HTMLButtonElement).disabled, false, "the form can be sent again"));
    }
    code = "";
    fireEvent.click(addButton());
    await waitFor(() => assert.equal(saved, 1));
    assert.deepEqual(alerts(), [], "the last refusal does not outlive a successful add");
    assert.equal(input("private_key")?.value ?? "", "");
  });
}

test("language changes keep the English template and preserve a replaced address", async () => {
  await i18n.changeLanguage("en");
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind="zstack" kindInfos={kindInfos} categories={kindCategories(kindInfos)} providerLabel="ZStack" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  await waitFor(() => assert.equal(input("endpoint")?.value, endpointTemplate));
  await act(async () => { await i18n.changeLanguage("zh"); });
  assert.equal(input("endpoint").value, endpointTemplate);
  const endpoint = "https://cloud.example.test:8443/zstack";
  fireEvent.change(input("endpoint"), { target: { value: endpoint } });
  await act(async () => { await i18n.changeLanguage("en"); });
  assert.equal(input("endpoint").value, endpoint);
});

test("ZStack editing loads and submits the saved endpoint without applying the creation template", async () => {
  await i18n.changeLanguage("en");
  const endpoint = "http://192.0.2.10:8080/zstack";
  let saved = 0;
  const requests: Record<string, unknown>[] = [];
  globalThis.fetch = async (url, init) => {
    assert.equal(url, "/api/v1/providers/1");
    if (!init?.method || init.method === "GET") {
      return new Response(JSON.stringify({ id: 1, kind: "zstack", name: "cloud", endpoint, public_key: "saved-ak", extra: {} }));
    }
    assert.equal(init.method, "PUT");
    requests.push(JSON.parse(String(init.body)));
    return new Response('{"id":1,"kind":"zstack","name":"cloud"}');
  };
  const provider = { id: 1, kind: "zstack", name: "cloud", category: "private_cloud" as const, state: "valid", instance_count: 0, resources: [{ type: "host" as const }], node_count: 0, created_at: "", updated_at: "" };
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open provider={provider} kindInfos={kindInfos} categories={kindCategories(kindInfos)} providerLabel="ZStack" onClose={() => {}} onSaved={() => { saved++; }} /></I18nextProvider>);
  await waitFor(() => assert.equal(input("endpoint")?.value, endpoint));
  assert.equal(input("private_key").value, "");
  assert.ok(within(screen.getByRole("dialog")).getByText("Requires ZStack Cloud 5.4.2 or later."), "an edit states the requirement of the account's kind too");
  const saveButton = () => screen.getByRole("button", { name: /Save/ }) as HTMLButtonElement;
  await waitFor(() => assert.equal(saveButton().disabled, false));
  fireEvent.click(saveButton());
  await waitFor(() => assert.equal(saved, 1));
  assert.equal(requests.length, 1);
  assert.equal(requests[0].endpoint, endpoint);
  assert.equal(requests[0].private_key, undefined);
});
