import SettingsSaveButton from "../SettingsSaveButton";
import { useRef, useState } from "react";
import { Alert, Button, Drawer, Form, Input, InputNumber, Space, Switch } from "antd";
import { useTranslation } from "react-i18next";
import { api, type NodeRequirementsMap, type SyncGroup } from "../../../api";
import NodeRequirementsSelect from "./NodeRequirementsSelect";
import { kibToMiB, miBToKiB, pullErrorField, pullErrorText } from "./pullFormat";

/**
 * SyncGroupDrawer creates or edits a sync group: the nodes that copy images
 * and models from one another (a label selector) and an optional rate limit
 * for those copies. The form speaks MB/s; the API stores KiB/s.
 */
export default function SyncGroupDrawer({
  group,
  onClose,
  onSaved,
}: {
  group: SyncGroup | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const saveTrigger = useRef<HTMLButtonElement>(null);
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [name, setName] = useState(group?.name ?? "");
  const [requirements, setRequirements] = useState<NodeRequirementsMap>(group?.node_requirements ?? {});
  const [limited, setLimited] = useState((group?.rate_limit_kib_s ?? 0) > 0);
  const [rate, setRate] = useState<number | null>(group && group.rate_limit_kib_s > 0 ? kibToMiB(group.rate_limit_kib_s) : null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<{ field: string; text: string } | null>(null);

  const save = async () => {
    setError(null);
    if (limited && !(rate && rate > 0)) {
      setError({ field: "rate_limit", text: t("pull.groups.rateRequired") });
      return;
    }
    setSaving(true);
    const body = {
      name: name.trim(),
      node_requirements: requirements,
      // An untouched limit goes back exactly as stored: the form shows MB/s rounded to two places.
      rate_limit_kib_s: limited && rate ? (group && rate === kibToMiB(group.rate_limit_kib_s) ? group.rate_limit_kib_s : miBToKiB(rate)) : 0,
    };
    try {
      if (group) await api.updateSyncGroup(group.id, body);
      else await api.createSyncGroup(body);
      onSaved();
    } catch (err) {
      setError({ field: pullErrorField(err), text: pullErrorText(t, tCommon, err) });
    } finally {
      setSaving(false);
    }
  };
  const fieldHelp = (field: string) => (error?.field === field ? error.text : undefined);
  const fieldStatus = (field: string) => (error?.field === field ? "error" : undefined);

  return (
    <Drawer
      open
      width={520}
      title={group ? t("pull.groups.editTitle") : t("pull.groups.newTitle")}
      onClose={onClose}
      maskClosable={!saving}
      footer={
        <Space style={{ display: "flex", justifyContent: "flex-end" }}>
          <Button onClick={onClose} disabled={saving}>{t("pull.action.cancel")}</Button>
          <SettingsSaveButton triggerRef={saveTrigger} loading={saving} onSave={save} />
        </Space>
      }
    >
      <Form layout="vertical">
        <Form.Item
          label={t("pull.groups.name")}
          htmlFor="pull-group-name"
          validateStatus={fieldStatus("name")}
          help={fieldHelp("name")}
        >
          <Input
            id="pull-group-name"
            autoFocus
            autoComplete="off"
            value={name}
            onChange={(event) => setName(event.target.value)}
            onPressEnter={(event) => { event.preventDefault(); saveTrigger.current?.click(); }}
          />
        </Form.Item>
        <Form.Item
          label={t("pull.groups.selector")}
          extra={t("pull.groups.selectorHelp")}
          validateStatus={fieldStatus("requirements")}
          help={fieldHelp("requirements")}
        >
          <NodeRequirementsSelect ariaLabel={t("pull.groups.selector")} value={requirements} onChange={setRequirements} />
        </Form.Item>
        <Form.Item
          label={t("pull.groups.rateLimit")}
          extra={t("pull.groups.rateHelp")}
          validateStatus={fieldStatus("rate_limit")}
          help={fieldHelp("rate_limit")}
        >
          <Space align="center" wrap>
            <Switch aria-label={t("pull.groups.rateLimit")} checked={limited} onChange={setLimited} />
            <InputNumber
              aria-label={t("pull.groups.rateValue")}
              step={10}
              style={{ width: 120 }}
              disabled={!limited}
              value={rate}
              onChange={(value) => setRate(value == null ? null : Number(value))}
              onPressEnter={(event) => { event.preventDefault(); saveTrigger.current?.click(); }}
            />
            <span>{t("pull.groups.rateUnit")}</span>
          </Space>
        </Form.Item>
        {error && !["name", "requirements", "rate_limit"].includes(error.field) && (
          <Alert role="alert" type="error" showIcon message={error.text} />
        )}
      </Form>
    </Drawer>
  );
}
