import { useEffect, useState } from "react";
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
import { api, type AIConfig, type AIConfigTestResult, type AIPreset } from "../../api";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";

/** PRESETS are the endpoints the platform knows a base address and model for. */
export const PRESETS: { value: AIPreset; base_url: string; model: string }[] = [
  { value: "deepseek", base_url: "https://api.deepseek.com", model: "deepseek-v4-flash" },
  {
    value: "bailian",
    base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1",
    model: "qwen-plus",
  },
  { value: "siliconflow", base_url: "https://api.siliconflow.cn/v1", model: "" },
  { value: "openrouter", base_url: "https://openrouter.ai/api/v1", model: "" },
  { value: "poe", base_url: "https://api.poe.com/v1", model: "GPT-5" },
  { value: "custom", base_url: "", model: "" },
];

export const presetLabel = (t: TFunction, preset: string) =>
  PRESETS.some((item) => item.value === preset) ? t(`ai.presetName.${preset}`) : preset;

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
}

/**
 * AIConfigModal adds an AI service, or edits one in place. An edit is
 * prefilled from the stored service and leaves the key blank, because the API
 * never sends a key back: a blank field keeps the stored one, and typing a new
 * key replaces it.
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
  const outboundAccess = useOutboundHostAccess([baseURL ?? ""], open);
  const [submitting, setSubmitting] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<AIConfigTestResult | null>(null);

  // A result belongs to the address it was measured against; a new dialog
  // starts without one.
  useEffect(() => {
    if (!open) setTestResult(null);
  }, [open]);

  const onPreset = (value: AIPreset) => {
    const p = PRESETS.find((x) => x.value === value);
    if (p) form.setFieldsValue({ base_url: p.base_url, model: p.model });
  };

  const initialValues: FormValues = editing
    ? {
        preset: editing.preset as AIPreset,
        name: editing.name,
        base_url: editing.base_url,
        model: editing.model,
        api_key: "",
        is_default: editing.is_default,
      }
    : {
        preset: "deepseek",
        name: "",
        base_url: "https://api.deepseek.com",
        model: "deepseek-v4-flash",
        api_key: "",
        is_default: true,
      };

  // test asks the endpoint about the values in the form, before they are
  // saved: only the address and the model are required, so an operator can try
  // an address without having filled in a name yet. The other values are read
  // unvalidated - a key typed here is what the test must use.
  const test = async () => {
    const valid = await form.validateFields(["base_url", "model"]).catch(() => null);
    if (!valid) return;
    const values = form.getFieldsValue();
    setTesting(true);
    setTestResult(null);
    try {
      await outboundAccess.authorize();
      setTestResult(
        await api.testAIConfig({
          id: editing?.id,
          base_url: values.base_url,
          // An empty key on an edit means "keep the stored one"; the server
          // uses it only for the service's own address.
          api_key: values.api_key || undefined,
          model: values.model,
        }),
      );
    } catch (e) {
      // The test request itself failed (an invalid address, an expired
      // session) rather than the endpoint answering badly.
      message.error(t("error.save", { detail: (e as Error).message }));
    } finally {
      setTesting(false);
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

  return (
    <Modal
      title={editing ? t("ai.editService") : t("ai.addService")}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={submitting}
      okText={editing ? t("common.save") : t("common.add")}
      cancelText={t("common.cancel")}
      destroyOnHidden
    >
      <Form
        key={editing ? `edit-${editing.id}` : "add"}
        form={form}
        layout="vertical"
        initialValues={initialValues}
      >
        <Form.Item label={t("ai.preset")} name="preset" rules={[{ required: true }]}>
          <Select options={PRESETS.map((preset) => ({ value: preset.value, label: presetLabel(t, preset.value) }))} onChange={onPreset} />
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
          <Input placeholder="deepseek-v4-flash" />
        </Form.Item>
        <Form.Item
          label={t("ai.upstreamKey")}
          name="api_key"
          rules={editing ? [] : [{ required: true, whitespace: true, message: t("ai.keyRequired") }]}
        >
          <Input.Password placeholder={editing ? t("ai.keyKeep") : "sk-…"} autoComplete="new-password" />
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
        <Button onClick={test} loading={testing}>
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
      </Space>
    </Modal>
  );
}
