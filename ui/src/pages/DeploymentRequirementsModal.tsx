import { Form, Modal, Radio, Select, Space, Typography, message } from "antd";
import { Alert, Tag } from "../pages/DeploymentControls";
import { Button } from "../components/ui/button";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { api, usePolling, type Deployment } from "../api";
import { apiErrorText } from "../apiError";
import SystemLabelConditions, { systemConditionsToRequirements } from "../components/SystemLabelConditions";
import { systemLabelValue } from "../components/SystemLabelTags";
import NodePicker from "../nodes/NodePicker";
import { useNodeFacets } from "../nodes/useNodeFacets";
import { HelpTip } from "../ui-kit/HelpTip";

const NODE_KEY = "gpuworks.ai/node";
const VENDOR_KEY = "gpuworks.ai/vendor";
const SYSTEM_PREFIX = "gpuworks.ai/";

type Values = {
  nodes: string[];
  system_conditions: string[];
  label_conditions: string[];
  placement_strategy: string;
};

// splitRequirements spreads stored requirements over the form's controls:
// the node list, the hardware and network picks (system labels), and the
// written-label picks. The vendor is not a control - it cannot change.
export function splitRequirements(reqs: Record<string, string[]> = {}): Omit<Values, "placement_strategy"> {
  const out: Omit<Values, "placement_strategy"> = { nodes: [], system_conditions: [], label_conditions: [] };
  for (const [key, values] of Object.entries(reqs)) {
    if (key === VENDOR_KEY) continue;
    if (key === NODE_KEY) out.nodes = [...values];
    else if (key.startsWith(SYSTEM_PREFIX)) out.system_conditions.push(...values.map((value) => `${key}=${value}`));
    else out.label_conditions.push(...values.map((value) => `${key}=${value}`));
  }
  return out;
}

// joinRequirements is the request body's node_requirements: the controls'
// picks merged, values of one key being alternatives. The vendor is left
// out; the server keeps it.
export function joinRequirements(values: Omit<Values, "placement_strategy">): Record<string, string[]> {
  const out = systemConditionsToRequirements([...(values.system_conditions ?? []), ...(values.label_conditions ?? [])]);
  if ((values.nodes ?? []).length > 0) out[NODE_KEY] = [...values.nodes];
  return out;
}

// EditRequirementsButton opens the editor for where a single-node
// deployment's replicas may run. The server is the authority on whether the
// result can ever run a replica; the count here is the live match only.
export function EditRequirementsButton({ dep, onChanged }: { dep: Deployment; onChanged: (dep: Deployment) => void }) {
  const { t } = useTranslation("deployments");
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="tertiary" size="xs" disabled={dep.desired_state === "deleting"} onClick={() => setOpen(true)}>
        {t("detail.placement.edit.open")}
      </Button>
      {open && <RequirementsModal dep={dep} onClose={() => setOpen(false)} onChanged={(next) => { setOpen(false); onChanged(next); }} />}
    </>
  );
}

function RequirementsModal({ dep, onClose, onChanged }: { dep: Deployment; onClose: () => void; onChanged: (dep: Deployment) => void }) {
  const { t } = useTranslation("deployments");
  const { t: tNodes } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const [form] = Form.useForm<Values>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const initial = useMemo(() => ({ ...splitRequirements(dep.node_requirements), placement_strategy: dep.placement_strategy || "spread" }), [dep]);
  const watched = Form.useWatch([], form) as Values | undefined;
  const vendor = dep.node_requirements?.[VENDOR_KEY]?.[0];
  const vendorLabel = dep.node_requirement_labels?.find((label) => label.key === VENDOR_KEY);
  // The choices and the live match count are the server's: the nodes of the
  // deployment's vendor, what they carry, and how many satisfy the
  // requirements as placement matches them.
  const sameVendor = useMemo(() => ({ label: vendor ? [`${VENDOR_KEY}=${vendor}`] : [] }), [vendor]);
  const facets = useNodeFacets(sameVendor);
  const reqs = joinRequirements(watched ?? initial);
  const reqPairs = Object.entries(reqs).flatMap(([key, values]) => values.map((value) => `${key}=${value}`));
  const match = usePolling(
    () => api.nodesPage({ page: 1, page_size: 1, label: [...sameVendor.label, ...reqPairs] }),
    0,
    [vendor, reqPairs.join("\n")],
  );
  const matching = match.data?.total ?? 0;
  const labelOptions = useMemo(
    () => (facets.data?.labels ?? []).flatMap(({ key, values }) => values.map(({ value }) => `${key}=${value}`)).map((value) => ({ value, label: value })),
    [facets.data],
  );

  const save = async (values: Values) => {
    setSaving(true);
    setError("");
    try {
      const next = await api.setDeploymentRequirements(dep.id, { node_requirements: joinRequirements(values), placement_strategy: values.placement_strategy });
      message.success(t("detail.placement.edit.saved"));
      onChanged(next);
    } catch (err) {
      setError(apiErrorText(tCommon, err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open
      title={t("detail.placement.edit.title")}
      onCancel={onClose}
      onOk={() => form.submit()}
      okText={t("detail.placement.edit.save")}
      confirmLoading={saving}
      width={640}
    >
      <Typography.Paragraph type="secondary">{t("detail.placement.edit.intro")}</Typography.Paragraph>
      <Form form={form} layout="vertical" initialValues={initial} onFinish={save}>
        {vendor && (
          <Form.Item label={t("detail.placement.edit.vendor")} extra={t("detail.placement.edit.vendorFixed")}>
            <Tag>{vendorLabel ? systemLabelValue(vendorLabel, tNodes) : vendor}</Tag>
          </Form.Item>
        )}
        <Form.Item name="nodes" label={t("detail.placement.edit.nodes")} extra={<span className="inline-flex items-center gap-1">{t("detail.placement.edit.nodesHint")}<HelpTip content={t("detail.placement.edit.nodesHelp")} /></span>}>
          <NodePicker
            mode="multiple"
            allowClear
            filter={sameVendor}
            placeholder={t("detail.placement.edit.nodesPlaceholder")}
            // A pinned node that no longer exists stays visible so it can be removed.
            unknownLabel={(uuid) => `${uuid} (${t("detail.placement.noNode")})`}
          />
        </Form.Item>
        <SystemLabelConditions facets={facets.data?.system_labels} />
        <Form.Item name="label_conditions" label={t("detail.placement.edit.labels")} extra={t("detail.placement.edit.labelsHint")}>
          <Select mode="tags" allowClear options={labelOptions} placeholder={t("detail.placement.edit.labelsPlaceholder")} />
        </Form.Item>
        <Form.Item name="placement_strategy" label={t("detail.placement.edit.strategy")}>
          <Radio.Group>
            <Radio value="spread">{t("detail.placement.strategy.spread")}</Radio>
            <Radio value="binpack">{t("detail.placement.strategy.binpack")}</Radio>
          </Radio.Group>
        </Form.Item>
      </Form>
      <Space direction="vertical" style={{ width: "100%" }}>
        {match.data && (matching === 0
          ? <Alert type="warning" showIcon message={t("detail.placement.edit.noMatch")} />
          : <Typography.Text type="secondary">{t("detail.placement.edit.matches", { count: matching, total: match.data.all })}</Typography.Text>)}
        {error && <Alert type="error" showIcon message={t("detail.placement.edit.failed", { detail: error })} />}
      </Space>
    </Modal>
  );
}
