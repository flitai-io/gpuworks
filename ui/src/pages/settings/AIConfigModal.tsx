import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Button,
  Form,
  Input,
  Modal,
  Select,
  Space,
  Switch,
  message,
} from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import {
  api,
  type AIConfig,
  type AIConfigTestResult,
  type AIPreset,
  type AIPresetDictionary,
  type AIProxyMode,
} from "../../api";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import { Spinner } from "../../ui-kit/Spinner";

/**
 * presetLabel words a preset's stable name; a name this console has no label
 * for - a preset a newer server added - is shown as the server spells it.
 */
export const presetLabel = (t: TFunction, preset: string) =>
  preset ? t(`ai.presetName.${preset}`, { defaultValue: preset }) : preset;

/**
 * usePresetDictionary loads the server's preset table while the dialog is
 * open: the addresses, starting models and image switches a preset fills in
 * are the server's, the same table a create fills from.
 */
function usePresetDictionary(open: boolean) {
  const [dictionary, setDictionary] = useState<AIPresetDictionary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(() => {
    setError(null);
    api
      .aiPresets()
      .then(setDictionary)
      .catch((e) => setError((e as Error).message));
  }, []);
  useEffect(() => {
    if (open && !dictionary) load();
  }, [open, dictionary, load]);
  return { dictionary, error, retry: load };
}

/** IMAGE_RESULTS word each answer of the image check. */
const IMAGE_RESULTS: Record<
  NonNullable<AIConfigTestResult["image"]>,
  { type: "success" | "warning" | "info"; text: string }
> = {
  accepted: { type: "success", text: "ai.imageAccepted" },
  ignored: { type: "warning", text: "ai.imageIgnored" },
  rejected: { type: "warning", text: "ai.imageRejected" },
  unknown: { type: "info", text: "ai.imageUnknown" },
};

/**
 * TEST_RESULTS word each answer the server's connection test distinguishes.
 * The distinctions are the ones an operator acts on differently: a wrong
 * address, a wrong key, a model the endpoint does not serve, a host the
 * platform refuses to call, and a service that does not answer at all.
 */
const TEST_RESULTS: Record<
  AIConfigTestResult["outcome"],
  { type: "success" | "warning" | "error"; text: string }
> = {
  ok: { type: "success", text: "ai.testOk" },
  connection: { type: "error", text: "ai.testConnection" },
  blocked: { type: "error", text: "ai.testBlocked" },
  auth: { type: "error", text: "ai.testAuth" },
  path: { type: "error", text: "ai.testPath" },
  model: { type: "error", text: "ai.testModel" },
  http: { type: "warning", text: "ai.testHttp" },
};

interface FormValues {
  preset: AIPreset;
  name: string;
  base_url: string;
  model: string;
  api_key?: string;
  is_default: boolean;
  proxy_mode: AIProxyMode;
  proxy_url?: string;
  image_input: boolean;
}

/**
 * AIConfigModal adds an AI service, or edits one in place. An edit is
 * prefilled from the stored service and leaves the key blank, because the API
 * never sends a key back: a blank field keeps the stored one, and typing a new
 * key replaces it. A custom proxy is the same: the stored one is only ever
 * shown masked, so a blank field keeps it.
 */
export default function AIConfigModal({
  open,
  editing,
  onClose,
  onDone,
}: {
  open: boolean;
  editing?: AIConfig;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useTranslation("settings");
  const [form] = Form.useForm<FormValues>();
  const baseURL = Form.useWatch("base_url", form) as string | undefined;
  const proxyMode = Form.useWatch("proxy_mode", form) as AIProxyMode | undefined;
  const outboundAccess = useOutboundHostAccess([baseURL ?? ""], open);
  const { dictionary, error: dictionaryError, retry } = usePresetDictionary(open);
  const [submitting, setSubmitting] = useState(false);
  const [testing, setTesting] = useState<"text" | "image" | null>(null);
  const [testResult, setTestResult] = useState<AIConfigTestResult | null>(null);

  // A result belongs to the address it was measured against; a new dialog
  // starts without one.
  useEffect(() => {
    if (!open) setTestResult(null);
  }, [open]);

  const presets = dictionary?.presets ?? [];
  // The stored custom proxy, kept while the field is left blank.
  const storedProxy = editing?.proxy_mode === "custom" ? editing.proxy_url : undefined;

  const onPreset = (value: AIPreset) => {
    const p = presets.find((x) => x.name === value);
    if (p) form.setFieldsValue({ base_url: p.base_url, model: p.model, image_input: p.image_input });
  };

  const first = presets[0];
  const initialValues: FormValues = editing
    ? {
        preset: editing.preset,
        name: editing.name,
        base_url: editing.base_url,
        model: editing.model,
        api_key: "",
        is_default: editing.is_default,
        proxy_mode: editing.proxy_mode || "management",
        proxy_url: "",
        image_input: editing.image_input,
      }
    : {
        preset: first?.name ?? "",
        name: "",
        base_url: first?.base_url ?? "",
        model: first?.model ?? "",
        api_key: "",
        is_default: true,
        proxy_mode: "management",
        proxy_url: "",
        image_input: first?.image_input ?? false,
      };

  // proxyFields is the proxy choice as a request carries it: the URL only for
  // custom and only when typed, so a blank field keeps the stored proxy.
  const proxyFields = (values: FormValues) => ({
    proxy_mode: values.proxy_mode,
    ...(values.proxy_mode === "custom" && values.proxy_url ? { proxy_url: values.proxy_url } : {}),
  });

  // test asks the endpoint about the values in the form, before they are
  // saved: only the address and the model are required, so an operator can try
  // an address without having filled in a name yet. The other values are read
  // unvalidated - a key typed here is what the test must use. checkImage also
  // sends a small image and sets the image switch from the answer.
  const test = async (checkImage: boolean) => {
    const fields: (keyof FormValues)[] = ["base_url", "model"];
    if (form.getFieldValue("proxy_mode") === "custom") fields.push("proxy_url");
    const valid = await form.validateFields(fields).catch(() => null);
    if (!valid) return;
    const values = form.getFieldsValue();
    setTesting(checkImage ? "image" : "text");
    setTestResult(null);
    try {
      await outboundAccess.authorize();
      const result = await api.testAIConfig({
        id: editing?.id,
        base_url: values.base_url,
        // An empty key on an edit means "keep the stored one"; the server
        // uses it only for the service's own address.
        api_key: values.api_key || undefined,
        model: values.model,
        ...proxyFields(values),
        ...(checkImage ? { check_image: true } : {}),
      });
      setTestResult(result);
      // Only a definite answer moves the switch; an unknown one leaves the
      // operator's choice alone.
      if (result.image === "accepted") form.setFieldsValue({ image_input: true });
      // An endpoint that answered without seeing the image is as good as
      // one that refused it: a pasted screenshot would be dropped silently.
      if (result.image === "rejected" || result.image === "ignored") form.setFieldsValue({ image_input: false });
    } catch (e) {
      // The test request itself failed (an invalid address, an expired
      // session) rather than the endpoint answering badly.
      message.error(t("error.save", { detail: (e as Error).message }));
    } finally {
      setTesting(null);
    }
  };

  const submit = async () => {
    // A rejected validation leaves the dialog open on the fields it marked;
    // it is not a failed save, so it shows no message.
    const v = await form.validateFields().catch(() => null);
    if (!v) return;
    setSubmitting(true);
    try {
      await outboundAccess.authorize();
      if (editing) {
        await api.updateAIConfig(editing.id, {
          name: v.name,
          preset: v.preset,
          base_url: v.base_url,
          model: v.model,
          is_default: !!v.is_default,
          image_input: !!v.image_input,
          ...proxyFields(v),
          // Left out unless typed, so the stored key survives the edit.
          ...(v.api_key ? { api_key: v.api_key } : {}),
        });
        message.success(t("ai.updated"));
      } else {
        await api.createAIConfig({
          name: v.name,
          preset: v.preset,
          base_url: v.base_url,
          api_key: v.api_key ?? "",
          model: v.model,
          is_default: !!v.is_default,
          image_input: !!v.image_input,
          ...proxyFields(v),
        });
        message.success(t("ai.added"));
      }
      form.resetFields();
      setTestResult(null);
      onDone();
    } catch (e) {
      message.error(t(editing ? "error.update" : "error.add", { detail: (e as Error).message }));
    } finally {
      setSubmitting(false);
    }
  };

  const result = testResult ? TEST_RESULTS[testResult.outcome] ?? TEST_RESULTS.http : null;
  const imageResult = testResult?.image ? IMAGE_RESULTS[testResult.image] : null;

  return (
    <Modal
      title={editing ? t("ai.editService") : t("ai.addService")}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={submitting}
      okButtonProps={{ disabled: !dictionary }}
      okText={editing ? t("common.save") : t("common.add")}
      cancelText={t("common.cancel")}
      destroyOnHidden
    >
      {!dictionary && dictionaryError && (
        <Alert
          type="error"
          showIcon
          message={t("ai.presetsFailed", { detail: dictionaryError })}
          action={
            <Button size="small" onClick={retry}>
              {t("ai.retry")}
            </Button>
          }
        />
      )}
      {!dictionary && !dictionaryError && <Spinner label={t("ai.presetsLoading")} />}
      {dictionary && (
        <>
          <Form
            key={editing ? `edit-${editing.id}` : "add"}
            form={form}
            layout="vertical"
            initialValues={initialValues}
          >
            <Form.Item label={t("ai.preset")} name="preset" rules={[{ required: true }]}>
              <Select
                options={presets.map((preset) => ({ value: preset.name, label: presetLabel(t, preset.name) }))}
                onChange={onPreset}
              />
            </Form.Item>
            <Form.Item label={t("ai.name")} name="name" rules={[{ required: true, whitespace: true, message: t("ai.nameRequired") }]}>
              <Input placeholder={t("ai.namePlaceholder")} />
            </Form.Item>
            <Form.Item
              label={t("ai.baseUrl")}
              name="base_url"
              extra={t("ai.baseUrlHint")}
              rules={[{ required: true, whitespace: true, message: t("ai.baseUrlRequired") }]}
            >
              <Input placeholder="https://api.deepseek.com" />
            </Form.Item>
            <OutboundHostAccess access={outboundAccess} />
            <Form.Item label={t("ai.model")} name="model" rules={[{ required: true, whitespace: true, message: t("ai.modelRequired") }]}>
              <Input placeholder={first?.model || undefined} />
            </Form.Item>
            <Form.Item
              label={t("ai.upstreamKey")}
              name="api_key"
              rules={editing ? [] : [{ required: true, whitespace: true, message: t("ai.keyRequired") }]}
            >
              <Input.Password placeholder={editing ? t("ai.keyKeep") : "sk-…"} autoComplete="new-password" />
            </Form.Item>
            <Form.Item label={t("ai.proxy")} name="proxy_mode" extra={proxyMode ? t(`ai.proxyModeHint.${proxyMode}`) : undefined}>
              <Select options={dictionary.proxy_modes.map((mode) => ({ value: mode, label: t(`ai.proxyMode.${mode}`) }))} />
            </Form.Item>
            {proxyMode === "custom" && (
              <Form.Item
                label={t("ai.proxyUrl")}
                name="proxy_url"
                extra={storedProxy ? t("ai.proxyUrlStored", { url: storedProxy }) : t("ai.proxyUrlHint")}
                rules={storedProxy ? [] : [{ required: true, whitespace: true, message: t("ai.proxyUrlRequired") }]}
              >
                <Input
                  placeholder={storedProxy ? t("ai.keyKeep") : "http://user:password@proxy.example.com:3128"}
                  autoComplete="off"
                />
              </Form.Item>
            )}
            <Form.Item label={t("ai.imageInput")} extra={t("ai.imageInputHint")}>
              <Space>
                <Form.Item name="image_input" valuePropName="checked" noStyle>
                  <Switch aria-label={t("ai.imageInput")} />
                </Form.Item>
                <Button size="small" onClick={() => test(true)} loading={testing === "image"} disabled={testing === "text"}>
                  {t("ai.imageCheck")}
                </Button>
              </Space>
            </Form.Item>
            <Form.Item
              label={t("ai.setDefault")}
              name="is_default"
              valuePropName="checked"
              // While any service is stored exactly one of them is the one the
              // assistant uses, and the server refuses to clear the mark on that one
              // rather than leaving every stored service unused. The switch cannot
              // therefore offer to clear it: turning it off here would only produce a
              // rejection the operator cannot act on from this dialog.
              extra={editing?.is_default ? t("ai.defaultLocked") : undefined}
            >
              <Switch disabled={editing?.is_default} />
            </Form.Item>
          </Form>
          <Space direction="vertical" size={8} style={{ width: "100%" }}>
            <Button onClick={() => test(false)} loading={testing === "text"} disabled={testing === "image"}>
              {t("ai.test")}
            </Button>
            {testResult && result && (
              <Alert
                type={result.type}
                showIcon
                message={t(result.text)}
                description={
                  <span className="text-fg-muted">
                    {[
                      testResult.detail,
                      t("ai.testAddress", { url: testResult.base_url }),
                      t("ai.testLatency", { ms: testResult.latency_ms }),
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </span>
                }
              />
            )}
            {imageResult && (
              <Alert
                type={imageResult.type}
                showIcon
                message={t(imageResult.text)}
                description={testResult?.image_detail ? <span className="text-fg-muted">{testResult.image_detail}</span> : undefined}
              />
            )}
          </Space>
        </>
      )}
    </Modal>
  );
}
