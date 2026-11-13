import { useEffect, useState } from "react";
import { Button, Form, Input, Modal, Space, Typography, message } from "antd";
import { MinusCircleOutlined, PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, type Node } from "../api";

type LabelRow = { key: string; value: string };
export default function NodeLabelsModal({ node, onClose, onSaved }: { node: Node | null; onClose: () => void; onSaved: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const [form] = Form.useForm<{ labels: LabelRow[] }>();
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    if (node) form.setFieldsValue({ labels: Object.entries(node.labels ?? {}).map(([key, value]) => ({ key, value })) });
    else form.resetFields();
  }, [form, node]);
  async function save() {
    if (!node) return;
    let rows: LabelRow[];
    try { rows = (await form.validateFields()).labels ?? []; } catch { return; }
    const keys = rows.map((row) => row.key);
    if (new Set(keys).size !== keys.length) { message.error(t("labels.duplicate")); return; }
    setSaving(true);
    try {
      const updated = await api.updateNodeLabels(node.uuid, Object.fromEntries(rows.map((row) => [row.key, row.value ?? ""])));
      message.success(t("labels.saved")); onSaved(updated);
    } catch (error) { message.error(t("labels.failed", { detail: (error as Error).message })); }
    finally { setSaving(false); }
  }
  return <Modal title={t("labels.title")} open={!!node} onCancel={onClose} onOk={save} confirmLoading={saving} okText={t("action.save")} cancelText={t("action.cancel")} forceRender>
    <Typography.Paragraph>{t("labels.help")}</Typography.Paragraph>
    <Form form={form} layout="vertical"><Form.List name="labels">{(fields, { add, remove }) => <>
      {fields.map((field) => <Space key={field.key} align="baseline">
        <Form.Item name={[field.name, "key"]} rules={[{ required: true, whitespace: true, message: t("labels.keyRequired") }]}><Input aria-label={t("labels.key")} placeholder={t("labels.key")} maxLength={128} /></Form.Item>
        <Form.Item name={[field.name, "value"]}><Input aria-label={t("labels.value")} placeholder={t("labels.value")} maxLength={256} /></Form.Item>
        <Button type="text" icon={<MinusCircleOutlined />} aria-label={t("labels.remove")} onClick={() => remove(field.name)} />
      </Space>)}
      <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ key: "", value: "" })}>{t("labels.add")}</Button>
    </>}</Form.List></Form>
  </Modal>;
}
