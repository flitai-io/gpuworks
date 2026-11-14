import { Alert, Button, Descriptions, Form, InputNumber, Modal, Skeleton, Typography, message } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { api, usePolling, type DGPUQuotaResult } from "../api";
import { apiErrorText } from "../apiError";
import { HelpTip } from "../ui-kit/HelpTip";

const GIB = 1024 ** 3;

type Values = { vram_limit_gib: number };

function fmtGiB(bytes: number) { return `${(bytes / GIB).toFixed(1)} GiB`; }

// DGPUQuotaModal changes the dGPU memory limit of the deployment a slice
// belongs to. The limit is in effect at once: the server holds each running
// slice to it in place where that is safe and says which slices keep their
// limit until a restart. An engine that sizes its pool at start (vLLM,
// SGLang) uses a new limit only once its instance is restarted, which the
// operator does from the deployment's page; the dialog ends by saying so.
export default function DGPUQuotaModal({ deploymentID, onClose, onChanged }: { deploymentID: number; onClose: () => void; onChanged: () => void }) {
  const { t } = useTranslation("gpus");
  const { t: tCommon } = useTranslation("common");
  const [form] = Form.useForm<Values>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<DGPUQuotaResult | null>(null);
  const dep = usePolling(() => api.deployment(deploymentID), 0, [deploymentID]);
  const current = dep.data?.vram_limit_bytes ?? 0;
  const watched = Form.useWatch("vram_limit_gib", form);
  const unchanged = watched == null || Math.round(watched * GIB) === current;

  const save = async (values: Values) => {
    setSaving(true);
    setError("");
    try {
      const answer = await api.setDeploymentDGPUQuota(deploymentID, { vram_limit_gib: values.vram_limit_gib });
      message.success(t("quota.saved"));
      setResult(answer);
      onChanged();
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setSaving(false);
    }
  };

  if (result) {
    const limit = result.deployment.vram_limit_bytes ?? 0;
    return (
      <Modal
        open
        title={t("quota.title")}
        onCancel={onClose}
        footer={[
          <Link key="deployment" to={`/deployments?deployment=${deploymentID}`}><Button>{t("quota.openDeployment")}</Button></Link>,
          <Button key="done" type="primary" onClick={onClose}>{t("quota.done")}</Button>,
        ]}
      >
        <Alert
          type="success"
          showIcon
          message={result.live.length > 0
            ? t("quota.applied", { limit: fmtGiB(limit), count: result.live.length })
            : t("quota.appliedDeployment", { limit: fmtGiB(limit) })}
        />
        {result.pending.length > 0 && (
          <Alert
            style={{ marginTop: 12 }}
            type="warning"
            showIcon
            message={t("quota.pendingTitle", { count: result.pending.length })}
            description={
              <ul className="dgpu-quota-pending">
                {result.pending.map((item) => (
                  <li key={item.instance_uuid}>
                    <Typography.Text code>{item.instance_uuid.slice(0, 8)}</Typography.Text>{" "}
                    {t(`quota.pending.${item.reason}`, { used: item.used_bytes != null ? fmtGiB(item.used_bytes) : "", limit: fmtGiB(limit) })}
                  </li>
                ))}
              </ul>
            }
          />
        )}
        <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>{t("quota.restartHint")}</Typography.Paragraph>
      </Modal>
    );
  }

  return (
    <Modal
      open
      title={t("quota.title")}
      onCancel={onClose}
      onOk={() => form.submit()}
      okText={t("quota.save")}
      okButtonProps={{ disabled: !dep.data || unchanged }}
      confirmLoading={saving}
      destroyOnHidden
    >
      {dep.error && <Alert type="error" showIcon message={apiErrorText(tCommon, dep.error)} />}
      {!dep.data && !dep.error && <Skeleton active paragraph={{ rows: 3 }} />}
      {dep.data && (
        <>
          <Descriptions size="small" column={1} items={[
            { key: "deployment", label: t("quota.deployment"), children: dep.data.name },
            { key: "current", label: t("quota.current"), children: fmtGiB(current) },
          ]} />
          <Form form={form} layout="vertical" initialValues={{ vram_limit_gib: current / GIB }} onFinish={save} style={{ marginTop: 16 }}>
            <Form.Item
              name="vram_limit_gib"
              label={t("quota.limit")}
              rules={[{ required: true, message: t("quota.required") }, { type: "number", min: 0.5, message: t("quota.min") }]}
              extra={<span className="inline-flex items-center gap-1">{t("quota.hint")}<HelpTip content={t("quota.noteHelp")} /></span>}
            >
              <InputNumber min={0.5} step={0.5} precision={1} suffix="GiB" style={{ width: 200 }} autoFocus />
            </Form.Item>
          </Form>
          {error && <Alert type="error" showIcon message={error} />}
        </>
      )}
    </Modal>
  );
}
