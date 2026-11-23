import SettingsSaveButton from "./SettingsSaveButton";
import { useEffect, useState } from "react";
import { Alert, Button, Card, Form, Input, Skeleton, Space, Switch, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { APIError, apiErrorText } from "../../apiError";
import { useIsAdmin } from "../../components/useIsAdmin";
import { outboundPolicyApi, type OutboundPolicy } from "../../outboundPolicyApi";

export const OUTBOUND_POLICY_ANCHOR = "outbound-policy";

export default function OutboundPolicyCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const isAdmin = useIsAdmin();
  const [view, setView] = useState<OutboundPolicy | null>(null);
  const [restrict, setRestrict] = useState(false);
  const [hosts, setHosts] = useState("");
  const [reload, setReload] = useState(0);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [conflict, setConflict] = useState(false);
  const [saved, setSaved] = useState(false);

  const accept = (value: OutboundPolicy) => {
    setView(value);
    setRestrict(value.restrict_private_hosts);
    setHosts(value.allow_private_hosts.join("\n"));
    setConflict(false);
  };
  useEffect(() => {
    if (!isAdmin) return;
    let alive = true;
    setLoading(true);
    setError(null);
    outboundPolicyApi.get().then((value) => { if (alive) accept(value); })
      .catch((failure) => { if (alive) setError(failure); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [isAdmin, reload]);

  const dirty = view && (restrict !== view.restrict_private_hosts || hosts !== view.allow_private_hosts.join("\n"));
  const editable = isAdmin && view?.editable && !loading && !saving;
  const save = async () => {
    if (!view || !editable || !dirty || conflict) return;
    setSaving(true);
    setError(null);
    setSaved(false);
    try {
      const next = await outboundPolicyApi.put({
        restrict_private_hosts: restrict,
        allow_private_hosts: hosts.split(/\r?\n/).map((host) => host.trim()).filter(Boolean),
        revision: view.revision,
      });
      accept(next);
      setSaved(true);
    } catch (failure) {
      setError(failure);
      if (failure instanceof APIError && failure.code === "SERVER_OUTBOUND_POLICY_CONFLICT") setConflict(true);
    } finally { setSaving(false); }
  };
  const errorText = error instanceof APIError
    ? apiErrorText((key, options) => tCommon(key, { ...options, defaultValue: t("outboundPolicy.failed") }), error)
    : t("outboundPolicy.failed");

  return (
    <Card id={OUTBOUND_POLICY_ANCHOR} title={t("outboundPolicy.title")}>
      <Space direction="vertical" size={12} style={{ width: "100%", maxWidth: 760 }}>
        <Typography.Text type="secondary">{t("outboundPolicy.description")}</Typography.Text>
        <Alert type="info" showIcon message={t("outboundPolicy.permanent")} />
        {!isAdmin ? <Alert type="info" message={t("outboundPolicy.adminOnly")} /> : <>
          {loading && <><Typography.Text>{t("outboundPolicy.loading")}</Typography.Text><Skeleton active paragraph={{ rows: 2 }} /></>}
          {error != null && <Alert type="error" showIcon message={errorText} action={
            <Button disabled={loading || saving} onClick={() => { setSaved(false); setReload((value) => value + 1); }}>
              {t(view ? "outboundPolicy.reload" : "outboundPolicy.retry")}
            </Button>
          } />}
          {view && <>
            {!view.editable && <Alert type="warning" showIcon message={t("outboundPolicy.readOnly")} />}
            <Form layout="vertical">
              <Form.Item label={t("outboundPolicy.restrict")} htmlFor="outbound-restrict" extra={t("outboundPolicy.restrictHelp")}>
                <Switch id="outbound-restrict" checked={restrict} disabled={!editable} onChange={(value) => { setRestrict(value); setSaved(false); }} />
              </Form.Item>
              <Form.Item label={t("outboundPolicy.hosts")} htmlFor="outbound-hosts" extra={t("outboundPolicy.hostsHelp")}>
                <Input.TextArea id="outbound-hosts" autoSize={{ minRows: 4, maxRows: 12 }} value={hosts} disabled={!editable}
                  onChange={(event) => { setHosts(event.target.value); setSaved(false); }} />
              </Form.Item>
              <Typography.Paragraph type="secondary">{t("outboundPolicy.persisted")}</Typography.Paragraph>
              <SettingsSaveButton loading={saving} disabled={!editable || !dirty || conflict} onSave={save} />
            </Form>
          </>}
          {saved && <Alert type="success" showIcon message={t("outboundPolicy.saved")} />}
        </>}
      </Space>
    </Card>
  );
}
