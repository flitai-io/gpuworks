import { useState } from "react";
import { Alert, Button, Checkbox, Drawer, Form, Input, Radio, Space, Switch, Typography } from "antd";
import { ArrowDownOutlined, ArrowUpOutlined, CloseOutlined, PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, type NodeRequirementsMap, type PullSourceRule } from "../../../api";
import { OutboundHostAccess, useOutboundHostAccess } from "../../../components/OutboundHostAccess";
import NodeRequirementsSelect from "./NodeRequirementsSelect";
import PullRuleTest from "./PullRuleTest";
import { moved, pullErrorField, pullErrorText } from "./pullFormat";

/**
 * PullRuleDrawer creates or edits one pull source rule: which images it
 * covers (match), the alternative addresses to try first and in what order,
 * whether the image's own registry stays as the last resort, and which nodes
 * it applies to. A rule is either an image rule (matches registries) or a
 * model site rule (matches an http(s) site root); the type is chosen when the
 * rule is created and fixed afterwards, and only site rules can also serve
 * the management node (control_plane). What the server refuses comes back as a product error and is
 * shown at the field it belongs to.
 */
export default function PullRuleDrawer({
  rule,
  onClose,
  onSaved,
}: {
  rule: PullSourceRule | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [kind, setKind] = useState<"image" | "url">(rule?.kind ?? "image");
  const [match, setMatch] = useState(rule?.match ?? "");
  const [endpoints, setEndpoints] = useState<string[]>(rule?.endpoints?.length ? rule.endpoints : [""]);
  const [allowUpstream, setAllowUpstream] = useState(rule?.allow_upstream ?? true);
  const [requirements, setRequirements] = useState<NodeRequirementsMap>(rule?.node_requirements ?? {});
  // Untouched, a new site rule without node requirements also serves the management node.
  const [controlTouched, setControlTouched] = useState<boolean | null>(rule ? rule.control_plane : null);
  const [note, setNote] = useState(rule?.note ?? "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<{ field: string; text: string } | null>(null);
  const [focusIndex, setFocusIndex] = useState<number | null>(null);

  const site = kind === "url";
  const controlPlane = controlTouched ?? Object.keys(requirements).length === 0;
  const outboundAccess = useOutboundHostAccess([...endpoints, ...(allowUpstream ? [match] : [])], site && controlPlane);
  const suffix = site ? "Url" : "";
  const save = async () => {
    setSaving(true);
    setError(null);
    const body = {
      match: match.trim(),
      endpoints: endpoints.map((item) => item.trim()).filter(Boolean),
      allow_upstream: allowUpstream,
      note: note.trim(),
      node_requirements: requirements,
      ...(site ? { control_plane: controlPlane } : {}),
    };
    try {
      await outboundAccess.authorize();
      if (rule) await api.updatePullSourceRule(rule.id, body);
      else await api.createPullSourceRule(body);
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
      width={560}
      title={rule ? t("pull.rules.editTitle") : t("pull.rules.newTitle")}
      onClose={onClose}
      maskClosable={!saving}
      footer={
        <Space style={{ display: "flex", justifyContent: "flex-end" }}>
          <Button onClick={onClose} disabled={saving}>{t("pull.action.cancel")}</Button>
          <Button type="primary" loading={saving} onClick={() => void save()}>{t("pull.action.save")}</Button>
        </Space>
      }
    >
      <Form layout="vertical">
        <Form.Item label={t("pull.rules.type")} extra={t("pull.rules.typeHelp")}>
          <div role="radiogroup" aria-label={t("pull.rules.type")}>
            <Radio.Group
              value={kind}
              disabled={Boolean(rule)}
              onChange={(event) => setKind(event.target.value)}
              options={[
                { value: "image", label: t("pull.rules.kindImage") },
                { value: "url", label: t("pull.rules.kindUrl") },
              ]}
            />
          </div>
        </Form.Item>

        <Form.Item
          label={t("pull.rules.match")}
          htmlFor="pull-rule-match"
          extra={t(`pull.rules.matchHelp${suffix}`)}
          validateStatus={fieldStatus("match")}
          help={fieldHelp("match")}
        >
          <Input
            id="pull-rule-match"
            autoFocus
            autoComplete="off"
            value={match}
            placeholder={t(`pull.rules.matchPlaceholder${suffix}`)}
            onChange={(event) => setMatch(event.target.value)}
            onPressEnter={() => void save()}
          />
        </Form.Item>

        <Form.Item
          label={t("pull.rules.endpoints")}
          extra={t(`pull.rules.endpointsHelp${suffix}`)}
          validateStatus={fieldStatus("endpoints")}
          help={fieldHelp("endpoints")}
        >
          <Space direction="vertical" style={{ width: "100%" }} role="group" aria-label={t("pull.rules.endpoints")}>
            {endpoints.map((endpoint, index) => (
              <Space.Compact key={index} style={{ width: "100%" }}>
                <Input
                  aria-label={t("pull.rules.endpointN", { n: index + 1 })}
                  autoComplete="off"
                  autoFocus={focusIndex === index}
                  value={endpoint}
                  placeholder={t(`pull.rules.endpointPlaceholder${suffix}`)}
                  onChange={(event) => setEndpoints((list) => list.map((item, i) => (i === index ? event.target.value : item)))}
                  onPressEnter={() => void save()}
                />
                <Button
                  aria-label={t("pull.rules.endpointUp", { n: index + 1 })}
                  icon={<ArrowUpOutlined aria-hidden />}
                  disabled={index === 0}
                  onClick={() => setEndpoints((list) => moved(list, index, -1))}
                />
                <Button
                  aria-label={t("pull.rules.endpointDown", { n: index + 1 })}
                  icon={<ArrowDownOutlined aria-hidden />}
                  disabled={index === endpoints.length - 1}
                  onClick={() => setEndpoints((list) => moved(list, index, 1))}
                />
                <Button
                  aria-label={t("pull.rules.endpointRemove", { n: index + 1 })}
                  icon={<CloseOutlined aria-hidden />}
                  onClick={() => setEndpoints((list) => list.filter((_, i) => i !== index))}
                />
              </Space.Compact>
            ))}
            <Button
              icon={<PlusOutlined aria-hidden />}
              onClick={() => {
                setFocusIndex(endpoints.length);
                setEndpoints((list) => [...list, ""]);
              }}
            >
              {t("pull.rules.addEndpoint")}
            </Button>
          </Space>
        </Form.Item>

        <Form.Item label={t("pull.rules.allowUpstream")} validateStatus={fieldStatus("allow_upstream")} help={fieldHelp("allow_upstream")}>
          <Space align="center">
            <Switch
              aria-label={t("pull.rules.allowUpstream")}
              checked={allowUpstream}
              onChange={setAllowUpstream}
            />
            <Typography.Text type="secondary">{t("pull.rules.allowUpstreamHelp")}</Typography.Text>
          </Space>
          {!allowUpstream && <Alert style={{ marginTop: 8 }} type="warning" showIcon message={t("pull.rules.noUpstreamWarning")} />}
        </Form.Item>

        <Form.Item
          label={t("pull.rules.nodes")}
          extra={t("pull.rules.nodesHelp")}
          validateStatus={fieldStatus("requirements")}
          help={fieldHelp("requirements")}
        >
          <NodeRequirementsSelect ariaLabel={t("pull.rules.nodes")} value={requirements} onChange={setRequirements} />
        </Form.Item>

        {site && (
          <Form.Item validateStatus={fieldStatus("control_plane")} help={fieldHelp("control_plane")} extra={t("pull.rules.controlPlaneHelp")}>
            <Checkbox checked={controlPlane} onChange={(event) => setControlTouched(event.target.checked)}>{t("pull.rules.controlPlane")}</Checkbox>
          </Form.Item>
        )}
        <OutboundHostAccess access={outboundAccess} />

        <Form.Item label={t("pull.rules.note")} htmlFor="pull-rule-note" validateStatus={fieldStatus("note")} help={fieldHelp("note")}>
          <Input.TextArea id="pull-rule-note" rows={2} value={note} placeholder={t("pull.rules.notePlaceholder")} onChange={(event) => setNote(event.target.value)} />
        </Form.Item>

        {site && controlPlane && <p className="text-sm text-fg-muted">{t("pull.rules.managementTestScope")}</p>}
        {site && <PullRuleTest match={match} endpoints={endpoints} allowUpstream={allowUpstream} />}

        <Typography.Text type="secondary">{t("pull.rules.docsHint")}</Typography.Text>

        {error && !error.field && <Alert role="alert" type="error" showIcon message={error.text} />}
        {error && error.field && !["match", "endpoints", "allow_upstream", "requirements", "note", "control_plane"].includes(error.field) && (
          <Alert role="alert" type="error" showIcon message={error.text} />
        )}
      </Form>
    </Drawer>
  );
}
